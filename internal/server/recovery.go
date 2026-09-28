package server

// Reservation recovery (task C5c; decisions in C5c seq206). A hub admin, over
// TLS this hub terminated itself, closes a hold its holder cannot close:
// release (READY or BLOCKED) or cancel (CANCELLED), never DONE. It gives a
// reason and exactly one evidence: aicrew stop evidence verified through
// coordination.v1, or an operator attestation. The same admin is the
// recovery reader. Every request, including a replay and every refusal after
// the admin gate, leaves exactly one reservation.recovery.* audit record,
// never with a proof or an attestation's text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"aimem/internal/access"
	"aimem/internal/introspect"
	"aimem/internal/store"
)

var recoveryKeyDigest = regexp.MustCompile(`^k1_[A-Za-z0-9_-]{43}$`)

type recoveryRequest struct {
	ReservationID    string             `json:"reservation_id"`
	Fence            string             `json:"fence"`
	ExpectedRevision int64              `json:"expected_revision"`
	Content          *store.TaskContent `json:"content"`
	Reason           string             `json:"reason"`
	Evidence         struct {
		Kind          string `json:"kind"`
		Proof         string `json:"proof,omitempty"`
		AttestationID string `json:"attestation_id,omitempty"`
		Statement     string `json:"statement,omitempty"`
	} `json:"evidence"`
}

type recoveryResponse struct {
	Operation    store.RecoveryOperation  `json:"operation"`
	RequestKey   string                   `json:"request_key"`
	Principal    string                   `json:"principal"`
	TaskID       string                   `json:"task_id"`
	TaskState    string                   `json:"task_state"`
	TaskRevision int64                    `json:"task_revision"`
	ClosingFence string                   `json:"closing_fence"`
	Affected     store.ReservationBinding `json:"affected"`
	EvidenceKind string                   `json:"evidence_kind"`
	EvidenceRef  string                   `json:"evidence_ref"`
}

// recoveryRefuse answers a recovery refusal with the reservation envelope.
func (s *Server) recoveryRefuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message, "retryable": status == http.StatusServiceUnavailable})
}

// recoveryAdmin is the admin behind a recovery request and the recheck of
// that admin inside the transaction. The check is about the credential that
// authenticated the request, never its name: the host's env token lasts for
// the process, and a registry token must still be registered, as admin,
// under the same digest. A registry token named "env", or a replacement
// under a removed token's name, is not the credential that was checked.
func (s *Server) recoveryAdmin(r *http.Request) (Identity, func() bool) {
	id, _ := IdentityFrom(r.Context())
	still := func() bool {
		switch id.credSource {
		case "env":
			return true
		case "registry":
			for _, t := range LoadTokens(s.reg.Root()) {
				if t.SHA256 == id.credDigest && t.Role == "admin" {
					return true
				}
			}
		}
		return false
	}
	return id, still
}

// recoveryAuditor is the one audit path of the recovery routes.
func (s *Server) recoveryAuditor(db *access.Store, admin, taskID string) func(outcome, detail string) {
	return func(outcome, detail string) {
		if err := db.RecordTeamRequest("admin:"+admin, "reservation.recovery."+outcome, fmt.Sprintf("task=%s %s", taskID, detail)); err != nil {
			s.log.Error("recovery audit", "err", err)
		}
	}
}

// recoveryCommitted audits a committed recovery, or its replay, and answers
// it. The detail names the evidence reference, never the proof or statement.
func (s *Server) recoveryCommitted(w http.ResponseWriter, audit func(outcome, detail string), op store.RecoveryOperation,
	key, admin, taskID, reservationID string, out store.TaskReservationOutcome) {
	outcome := string(op)[len("recovery_"):]
	if out.Replayed {
		outcome = "replay." + outcome
	}
	detail := fmt.Sprintf("reservation=%s fence=%d", reservationID, out.Reservation.Fence)
	if rec := out.Recovery; rec != nil {
		detail += fmt.Sprintf(" affected_user=%s affected_mode=%s evidence=%s ref=%s",
			rec.Affected.UserID, rec.Affected.Mode, rec.EvidenceKind, rec.EvidenceRef)
	}
	audit(outcome, detail)
	s.recoveryAnswer(w, op, key, admin, taskID, out)
}

func (s *Server) recoverRelease(w http.ResponseWriter, r *http.Request) {
	s.recoverReservation(w, r, store.RecoveryRelease)
}

func (s *Server) recoverCancel(w http.ResponseWriter, r *http.Request) {
	s.recoverReservation(w, r, store.RecoveryCancel)
}

func (s *Server) recoverReservation(w http.ResponseWriter, r *http.Request, op store.RecoveryOperation) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	id, stillAdmin := s.recoveryAdmin(r)
	taskID := r.PathValue("task_id")
	audit := s.recoveryAuditor(db, id.Name, taskID)
	refuse := func(status int, code, message string) {
		audit("refused."+code, "operation="+string(op))
		s.recoveryRefuse(w, status, code, message)
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > store.MaxTaskKeyBytes {
		refuse(http.StatusBadRequest, "invalid_request", "an Idempotency-Key header is required")
		return
	}
	var req recoveryRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<18))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "invalid_request", "the body is not a recovery request")
		return
	}
	fence, err := strconv.ParseInt(req.Fence, 10, 64)
	if err != nil || fence < 1 {
		refuse(http.StatusBadRequest, "invalid_request", "fence must be a positive decimal string")
		return
	}
	in := store.TaskReservationInput{TaskID: taskID, ID: req.ReservationID, Fence: fence,
		ExpectedRevision: req.ExpectedRevision, Content: req.Content, Reason: req.Reason}
	var evidence store.RecoveryEvidence
	var verifiedAt time.Time
	switch req.Evidence.Kind {
	case "attestation":
		if req.Evidence.Proof != "" {
			refuse(http.StatusBadRequest, "invalid_request", "an attestation carries no proof")
			return
		}
		evidence = store.RecoveryEvidence{Kind: "attestation", Ref: req.Evidence.AttestationID, Statement: req.Evidence.Statement}
	case "stop_evidence":
		if req.Evidence.AttestationID != "" || req.Evidence.Statement != "" {
			refuse(http.StatusBadRequest, "invalid_request", "stop evidence carries only its proof")
			return
		}
		// A committed recovery replays from its receipt; aicrew is asked
		// only about a request that has not committed (the replay rule).
		if introspect.ValidProof(req.Evidence.Proof) {
			replayEvidence := store.RecoveryEvidence{Kind: "stop_evidence", Ref: proofP1Digest(req.Evidence.Proof)}
			prior, found, err := s.reg.RecoveryReplay(op, in, replayEvidence, store.TaskActor{Kind: "recovery", Name: id.Name}, key)
			if err != nil {
				status, code, msg := recoveryErrorStatus(err)
				refuse(status, code, msg)
				return
			}
			if found {
				if !stillAdmin() {
					refuse(http.StatusUnauthorized, "invalid_credential", "the admin credential is no longer registered")
					return
				}
				s.recoveryCommitted(w, audit, op, key, id.Name, taskID, req.ReservationID, prior)
				return
			}
		}
		ref, code, msg := s.verifyStopEvidence(r.Context(), taskID, key, req.Evidence.Proof)
		if code != "" {
			status := http.StatusForbidden
			if code == "context_unavailable" {
				status = http.StatusServiceUnavailable
			}
			refuse(status, code, msg)
			return
		}
		evidence, verifiedAt = store.RecoveryEvidence{Kind: "stop_evidence", Ref: ref}, time.Now()
	default:
		refuse(http.StatusBadRequest, "invalid_request", "evidence kind must be stop_evidence or attestation")
		return
	}
	actor := store.TaskActor{Kind: "recovery", Name: id.Name}
	out, err := s.reg.RecoverTaskReservation(r.Context(), op, in, evidence, actor, key, func() error {
		if beforeReservationRecheck != nil {
			beforeReservationRecheck()
		}
		if !stillAdmin() {
			return refuseReservation("invalid_credential", "the admin credential is no longer registered")
		}
		if !verifiedAt.IsZero() && time.Since(verifiedAt) > reservationVerificationAge {
			return refuseReservation("context_unavailable", "the stop evidence was verified too long ago")
		}
		return nil
	})
	if err != nil {
		status, code, msg := recoveryErrorStatus(err)
		refuse(status, code, msg)
		return
	}
	s.recoveryCommitted(w, audit, op, key, id.Name, taskID, req.ReservationID, out)
}

func (s *Server) recoveryAnswer(w http.ResponseWriter, op store.RecoveryOperation, key, admin, taskID string, out store.TaskReservationOutcome) {
	rec := out.Recovery
	if rec == nil {
		s.recoveryRefuse(w, http.StatusInternalServerError, "internal", "the receipt is not a recovery")
		return
	}
	s.ok(w, recoveryResponse{Operation: op, RequestKey: key, Principal: "recovery/" + admin, TaskID: taskID,
		TaskState: out.Task.State, TaskRevision: out.Task.Revision, ClosingFence: strconv.FormatInt(out.Reservation.Fence, 10),
		Affected: rec.Affected, EvidenceKind: rec.EvidenceKind, EvidenceRef: rec.EvidenceRef})
}

// verifyStopEvidence checks a coordination.v1 stopped fact against the hold
// it would close. The acting member is the holder recorded on the hold, not
// the admin; the session may have moved on, so user, team and role must
// match, with the attempt, the task and this request's key. It returns the
// proof's p1_ digest, or a refusal code and message.
func (s *Server) verifyStopEvidence(ctx context.Context, taskID, key, proof string) (string, string, string) {
	if !introspect.ValidProof(proof) {
		return "", "coordination_rejected", "the stop proof is malformed"
	}
	_, pdb, err := s.reg.LocateTask(taskID)
	if err != nil {
		return "", "task_unavailable", "task not found"
	}
	hold, err := pdb.GetTaskReservation(taskID)
	if err != nil {
		return "", "task_unavailable", "task not found"
	}
	if hold.ID == "" || hold.Binding.Mode != "team" {
		return "", "coordination_rejected", "stop evidence applies only to a team hold"
	}
	peer, ok := s.operationalPeer()
	if !ok {
		return "", "context_unavailable", "no operational aicrew peer"
	}
	if s.introspect == nil {
		return "", "context_unavailable", "introspection is not configured"
	}
	fact, err := s.introspect.Coordinate(ctx, toIntrospectPeer(peer), proof)
	if err != nil {
		var f *introspect.Failure
		if errors.As(err, &f) && f.Code == introspect.CodeRejected {
			return "", "coordination_rejected", "aicrew does not vouch for this stop"
		}
		return "", "context_unavailable", "aicrew could not be asked"
	}
	b := hold.Binding
	switch {
	case fact.Kind != "stopped", fact.TaskID != taskID, fact.RequestKey != store.RequestKeyDigest(key),
		fact.AttemptRef != hold.Holder.Ref, fact.ServiceID != b.ServiceID,
		fact.Member.UserID != b.UserID, fact.Member.TeamID != b.TeamID, fact.Member.Role != b.Role:
		return "", "coordination_rejected", "the stop evidence is not for this hold"
	}
	return proofP1Digest(proof), "", ""
}

// operationalPeer is the single operational aicrew peer, as the team
// verifier picks it.
func (s *Server) operationalPeer() (access.IdentityPeer, bool) {
	db, err := s.openAccess(false)
	if err != nil {
		return access.IdentityPeer{}, false
	}
	peers, err := s.introspectionPeers(db)
	if err != nil {
		return access.IdentityPeer{}, false
	}
	var found *peerState
	for i := range peers {
		if peers[i].NotOperational == "" {
			if found != nil {
				return access.IdentityPeer{}, false
			}
			found = &peers[i]
		}
	}
	if found == nil {
		return access.IdentityPeer{}, false
	}
	return found.IdentityPeer, true
}

func proofP1Digest(proof string) string {
	return "p1_" + store.RequestKeyDigest(proof)[len("k1_"):]
}

func recoveryErrorStatus(err error) (int, string, string) {
	var r *reservationRefusal
	if errors.Is(err, store.ErrRecoveryEvidence) {
		return http.StatusBadRequest, "invalid_request", "a reason and one valid evidence kind this hold accepts are required"
	}
	if !errors.As(reservationError(err), &r) {
		return http.StatusInternalServerError, "internal", "recovery failed"
	}
	switch r.Code {
	case "invalid_request":
		return http.StatusBadRequest, r.Code, "the recovery request is invalid"
	case "task_unavailable":
		return http.StatusNotFound, r.Code, "task not found"
	case "stale_fence", "revision_conflict", "idempotency_conflict", "reservation_conflict":
		return http.StatusConflict, r.Code, r.Detail
	case "context_unavailable":
		return http.StatusServiceUnavailable, r.Code, r.Detail
	case "invalid_credential":
		return http.StatusUnauthorized, r.Code, r.Detail
	}
	return http.StatusForbidden, r.Code, r.Detail
}

type recoveryHold struct {
	TaskID        string                   `json:"task_id"`
	TaskRevision  int64                    `json:"task_revision"`
	State         string                   `json:"state"`
	ReservationID string                   `json:"reservation_id,omitempty"`
	Fence         string                   `json:"fence"`
	HolderMode    string                   `json:"holder_mode,omitempty"`
	HolderRef     string                   `json:"holder_ref,omitempty"`
	Binding       store.ReservationBinding `json:"binding"`
}

// recoveryStatus is the recovery reader's view of any hold on a task.
func (s *Server) recoveryStatus(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	id, _ := s.recoveryAdmin(r)
	taskID := r.PathValue("task_id")
	audit := s.recoveryAuditor(db, id.Name, taskID)
	_, pdb, err := s.reg.LocateTask(taskID)
	if err != nil {
		audit("refused.task_unavailable", "operation=read status")
		s.recoveryRefuse(w, http.StatusNotFound, "task_unavailable", "task not found")
		return
	}
	hold, err := pdb.GetTaskReservation(taskID)
	if err != nil {
		audit("refused.task_unavailable", "operation=read status")
		s.recoveryRefuse(w, http.StatusNotFound, "task_unavailable", "task not found")
		return
	}
	audit("read", "status")
	out := recoveryHold{TaskID: taskID, TaskRevision: hold.TaskRevision, State: "none", Fence: strconv.FormatInt(hold.Fence, 10)}
	if hold.ID != "" {
		out.State, out.ReservationID, out.HolderMode, out.HolderRef, out.Binding = "held", hold.ID, hold.Holder.Mode, hold.Holder.Ref, hold.Binding
	}
	s.ok(w, out)
}

var recoveryReadOperations = map[string]bool{"claim": true, "transfer": true, "update": true, "release": true, "finalize": true,
	string(store.RecoveryRelease): true, string(store.RecoveryCancel): true}

// recoveryReceipts is the recovery reader's receipt lookup: any actor's
// committed receipt for a task, operation and request-key digest.
func (s *Server) recoveryReceipts(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	id, _ := s.recoveryAdmin(r)
	taskID, op, digest := r.PathValue("task_id"), r.PathValue("operation"), r.PathValue("request_key_digest")
	audit := s.recoveryAuditor(db, id.Name, taskID)
	refuse := func(status int, code, message string) {
		// The operation and digest are recorded only once they are known
		// to be well formed.
		audit("refused."+code, "operation=read receipts")
		s.recoveryRefuse(w, status, code, message)
	}
	if !recoveryReadOperations[op] || !recoveryKeyDigest.MatchString(digest) {
		refuse(http.StatusBadRequest, "invalid_request", "unknown operation or malformed key digest")
		return
	}
	_, pdb, err := s.reg.LocateTask(taskID)
	if err != nil {
		refuse(http.StatusNotFound, "task_unavailable", "task not found")
		return
	}
	receipts, err := pdb.RecoveryReceipts(taskID, store.ReservationOperation(op), digest)
	if err != nil {
		refuse(http.StatusInternalServerError, "internal", "cannot read receipts")
		return
	}
	audit("read", fmt.Sprintf("receipts operation=%s key=%s found=%d", op, digest, len(receipts)))
	s.ok(w, map[string]any{"task_id": taskID, "operation": op, "request_key_digest": digest, "receipts": receipts})
}
