package server

// The member reservation routes (task C6a; docs/DESIGN-AIFORGE-RESERVATION-
// WIRE.md). They are thin adapters over the C5 authorizer: every mutation
// arrives over the acting member's own connection (decision D4a), and the
// authorizer derives the actor and binding from it. Here the wire's version,
// key and path checks are applied and the store's outcomes are mapped to the
// wire's Outcome, Status and ReceiptStatus shapes. Any refusal uses the
// reservation envelope.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

const reservationVersionHeader = "X-Aimem-Reservation-Version"

// reservationReceiptFault, when a test sets it, replaces the receipt read's
// result with a storage error, so the unresolved answer can be observed.
var reservationReceiptFault func() error

// reservationRoutePatterns are the member surface, exactly as the wire freezes
// it. The gate classifies a request with the route mux's own matching.
var reservationRoutePatterns = []string{
	"POST /v1/projects/{p}/tasks/{task_id}/reservation/claim",
	"POST /v1/projects/{p}/tasks/{task_id}/reservation/transfer",
	"POST /v1/projects/{p}/tasks/{task_id}/reservation/update",
	"POST /v1/projects/{p}/tasks/{task_id}/reservation/release",
	"POST /v1/projects/{p}/tasks/{task_id}/reservation/finalize",
	"GET /v1/projects/{p}/tasks/{task_id}/reservation",
	"GET /v1/projects/{p}/tasks/{task_id}/reservation/receipts/{operation}/{request_key}",
}

var reservationRouteMux = func() *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range reservationRoutePatterns {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}()

// reservationRoute reports whether r is on the member reservation surface.
func reservationRoute(r *http.Request) bool {
	if !canonicalPath(r) {
		return false
	}
	_, pattern := reservationRouteMux.Handler(r)
	return pattern != ""
}

type reservationRefusalSpec struct {
	status int
	next   string
}

// reservationRefusals is the wire's refusal table: HTTP status and the one
// permitted next action per code.
var reservationRefusals = map[string]reservationRefusalSpec{
	"invalid_credential":     {http.StatusUnauthorized, "Recover the individual credential through its authorized flow."},
	"identity_link_required": {http.StatusForbidden, "Re-prove or resume the team context and reconcile outstanding work."},
	"identity_mismatch":      {http.StatusForbidden, "Re-prove or resume the team context and reconcile outstanding work."},
	"context_missing":        {http.StatusForbidden, "Re-prove or resume the team context and reconcile outstanding work."},
	"context_stale":          {http.StatusForbidden, "Re-prove or resume the team context and reconcile outstanding work."},
	"context_unavailable":    {http.StatusServiceUnavailable, "Retry verification later with the same context and key."},
	"grant_denied":           {http.StatusForbidden, "Request authorized access or use the permitted aicrew flow."},
	"role_forbidden":         {http.StatusForbidden, "Request authorized access or use the permitted aicrew flow."},
	"task_unavailable":       {http.StatusNotFound, "Verify the task reference with an authorized project reader."},
	"dependency_unresolved":  {http.StatusNotFound, "Verify readable dependency evidence before a same-key retry."},
	"reservation_conflict":   {http.StatusConflict, "Read own authorized status and reconcile; stale holder stops."},
	"revision_conflict":      {http.StatusConflict, "Read own authorized status and reconcile; stale holder stops."},
	"stale_fence":            {http.StatusConflict, "Read own authorized status and reconcile; stale holder stops."},
	"idempotency_conflict":   {http.StatusConflict, "Investigate changed input; never replace the original key to force a transition."},
	"receipt_unresolved":     {http.StatusServiceUnavailable, "Reconcile the original receipt; do not send a later transition."},
	"work_outstanding":       {http.StatusConflict, "Reconcile accepted or stopped work before leaving or rotating."},
	"unsupported_version":    {http.StatusBadRequest, "Use a reviewed supported version or correct the request."},
	"invalid_request":        {http.StatusBadRequest, "Use a reviewed supported version or correct the request."},
	"coordination_rejected":  {http.StatusForbidden, "Begin the step again through aicrew; never reuse the proof or the key."},
	"process_mismatch":       {http.StatusConflict, "Reload the project's current process, then begin the step again through aicrew under it; never reuse the proof or the key."},
}

// reservationMessages are short, nonsecret messages per code. A refusal's
// detail is logged, never sent: it may describe another holder.
var reservationMessages = map[string]string{
	"invalid_credential":    "The individual credential is missing, revoked or not an individual credential.",
	"context_unavailable":   "The context or the coordination fact could not be verified now.",
	"grant_denied":          "The verified context has no live grant on the project.",
	"role_forbidden":        "The verified role has no path for this operation.",
	"task_unavailable":      "The task is not available in this project.",
	"reservation_conflict":  "The task is not held by this holder.",
	"revision_conflict":     "The expected revision is stale.",
	"stale_fence":           "The reservation ID or fence is stale.",
	"idempotency_conflict":  "The request key was used with different input.",
	"receipt_unresolved":    "The receipt could not be read now.",
	"unsupported_version":   "The reservation protocol version is missing or unsupported.",
	"invalid_request":       "The request is not a valid reservation request.",
	"coordination_rejected": "The coordination fact does not vouch for this step.",
	"process_mismatch":      "The process pin is not the project's current selection.",
}

// reservationRefuse answers a refusal in the reservation envelope. The active
// mode is shown only once the credential is known to be an individual's.
func (s *Server) reservationRefuse(w http.ResponseWriter, r *http.Request, code string) {
	spec, ok := reservationRefusals[code]
	if !ok {
		code, spec = "context_unavailable", reservationRefusals["context_unavailable"]
	}
	mode := ""
	switch code {
	case "invalid_credential", "unsupported_version", "invalid_request":
	default:
		if _, ok := teamContextFrom(r.Context()); ok {
			mode = "team"
		} else if _, ok := IdentityFrom(r.Context()); ok {
			mode = "personal"
		}
	}
	msg := reservationMessages[code]
	if msg == "" {
		msg = "The reservation request was refused."
	}
	cid := uuidv7.New()
	s.log.Warn("reservation request refused", "code", code, "correlation_id", cid)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(spec.status)
	json.NewEncoder(w).Encode(identityRefusalBody{Code: code, Message: msg, ActiveMode: mode,
		Retryable: code == "context_unavailable" || code == "receipt_unresolved", NextAction: spec.next, CorrelationID: cid})
}

// reservationFail answers an error from the authorizer.
func (s *Server) reservationFail(w http.ResponseWriter, r *http.Request, err error) {
	var ref *reservationRefusal
	if errors.As(reservationError(err), &ref) {
		s.log.Info("reservation refusal detail", "code", ref.Code, "detail", ref.Detail)
		s.reservationRefuse(w, r, ref.Code)
		return
	}
	s.log.Error("reservation", "err", err)
	s.reservationRefuse(w, r, "context_unavailable")
}

// reservationPreamble applies the version header and resolves the path's
// project against the task's current locator. It answers any refusal itself.
func (s *Server) reservationPreamble(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Header.Get(reservationVersionHeader) != "1" {
		s.reservationRefuse(w, r, "unsupported_version")
		return "", false
	}
	taskID := r.PathValue("task_id")
	project, _, err := s.reg.LocateTask(taskID)
	if err != nil || project != r.PathValue("p") {
		s.reservationRefuse(w, r, "task_unavailable")
		return "", false
	}
	return taskID, true
}

// reservationMutationBody is the wire's Mutation. No field names an actor.
type reservationMutationBody struct {
	ExpectedRevision int64  `json:"expected_revision"`
	ReservationID    string `json:"reservation_id"`
	Fence            string `json:"fence"`
	Holder           *struct {
		Mode    string `json:"mode"`
		WorkRef string `json:"work_ref"`
	} `json:"holder"`
	Intent            string             `json:"intent"`
	Content           *store.TaskContent `json:"content"`
	Reason            string             `json:"reason"`
	TerminalEvidence  []string           `json:"terminal_evidence"`
	CoordinationProof string             `json:"coordination_proof"`
}

func (s *Server) reservationMutate(op store.ReservationOperation) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID, ok := s.reservationPreamble(w, r)
		if !ok {
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > store.MaxTaskKeyBytes {
			s.reservationRefuse(w, r, "invalid_request")
			return
		}
		var body reservationMutationBody
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<18))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			s.reservationRefuse(w, r, "invalid_request")
			return
		}
		in := store.TaskReservationInput{TaskID: taskID, ID: body.ReservationID, ExpectedRevision: body.ExpectedRevision,
			Content: body.Content, Reason: body.Reason, TerminalEvidence: body.TerminalEvidence, Intent: body.Intent}
		if body.Fence != "" {
			f, err := strconv.ParseInt(body.Fence, 10, 64)
			if err != nil || f < 1 || strconv.FormatInt(f, 10) != body.Fence {
				s.reservationRefuse(w, r, "invalid_request")
				return
			}
			in.Fence = f
		}
		if body.Holder != nil {
			in.Holder = store.ReservationHolder{Mode: body.Holder.Mode, Ref: body.Holder.WorkRef}
		}
		out, err := s.reserve(r.Context(), op, in, key, body.CoordinationProof)
		if err != nil {
			s.reservationFail(w, r, err)
			return
		}
		s.reservationOK(w, reservationOutcome(op, taskID, key, out))
	}
}

func (s *Server) reservationStatusRoute(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.reservationPreamble(w, r)
	if !ok {
		return
	}
	st, err := s.reservationStatus(r.Context(), taskID)
	if err != nil {
		s.reservationFail(w, r, err)
		return
	}
	out := wireStatus{State: st.State}
	if st.Reservation != nil {
		h := st.Reservation
		out.ReservationID, out.Fence, out.OwnWorkRef, out.TaskRevision = h.ID, strconv.FormatInt(h.Fence, 10), h.Holder.Ref, h.TaskRevision
	}
	s.reservationOK(w, out)
}

func (s *Server) reservationReceiptRoute(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.reservationPreamble(w, r)
	if !ok {
		return
	}
	op, key := store.ReservationOperation(r.PathValue("operation")), r.PathValue("request_key")
	switch op {
	case store.ReservationClaim, store.ReservationTransfer, store.ReservationUpdate, store.ReservationRelease, store.ReservationFinalize:
	default:
		s.reservationRefuse(w, r, "invalid_request")
		return
	}
	if key == "" || len(key) > store.MaxTaskKeyBytes {
		s.reservationRefuse(w, r, "invalid_request")
		return
	}
	out, found, err := s.reservationReceipt(r.Context(), op, taskID, key)
	if reservationReceiptFault != nil {
		err = reservationReceiptFault()
	}
	if err != nil {
		var ref *reservationRefusal
		if errors.As(err, &ref) {
			s.reservationFail(w, r, err)
			return
		}
		// The store could not answer: the outcome is unknown, never
		// "not committed".
		s.log.Error("reservation receipt", "err", err)
		s.reservationRefuse(w, r, "receipt_unresolved")
		return
	}
	res := wireReceiptStatus{State: "not_committed", Operation: string(op), RequestKey: key}
	if found {
		outcome := reservationOutcome(op, taskID, key, out)
		res.State, res.Outcome = "committed", &outcome
	}
	s.reservationOK(w, res)
}

func (s *Server) reservationOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// The wire's result shapes.
type wireReceipt struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Operation    string `json:"operation"`
	RequestKey   string `json:"request_key"`
	Replayed     bool   `json:"replayed"`
	ActorID      string `json:"actor_id"`
	VerifiedMode string `json:"verified_mode"`
}

type wireReservation struct {
	ID         string `json:"id"`
	Fence      string `json:"fence"`
	Active     bool   `json:"active"`
	HolderMode string `json:"holder_mode,omitempty"`
	OwnWorkRef string `json:"own_work_ref,omitempty"`
}

type wireOutcome struct {
	Receipt      wireReceipt     `json:"receipt"`
	TaskRevision int64           `json:"task_revision"`
	Reservation  wireReservation `json:"reservation"`
}

type wireStatus struct {
	State         string `json:"state"`
	ReservationID string `json:"reservation_id,omitempty"`
	Fence         string `json:"fence,omitempty"`
	OwnWorkRef    string `json:"own_work_ref,omitempty"`
	TaskRevision  int64  `json:"task_revision,omitempty"`
}

type wireReceiptStatus struct {
	State      string       `json:"state"`
	Operation  string       `json:"operation"`
	RequestKey string       `json:"request_key"`
	Outcome    *wireOutcome `json:"outcome,omitempty"`
}

// reservationOutcome maps a committed store outcome to the wire's Outcome.
// The receipt's actor is the acting member recorded on it.
func reservationOutcome(op store.ReservationOperation, taskID, key string, out store.TaskReservationOutcome) wireOutcome {
	b := out.Binding
	id := store.ReservationReceiptID(b.UserID, op, taskID, key)
	res := out.Reservation
	w := wireOutcome{
		Receipt: wireReceipt{ID: id, State: "committed", Operation: string(op), RequestKey: key, Replayed: out.Replayed,
			ActorID: b.UserID, VerifiedMode: b.Mode},
		TaskRevision: out.Task.Revision,
		Reservation:  wireReservation{ID: res.ID, Fence: strconv.FormatInt(res.Fence, 10), Active: res.ID != ""},
	}
	if res.ID != "" {
		w.Reservation.HolderMode, w.Reservation.OwnWorkRef = res.Holder.Mode, res.Holder.Ref
	}
	return w
}
