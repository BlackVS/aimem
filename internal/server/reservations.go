package server

// The hub's reservation authorizer (task C5a; docs/DESIGN-AIFORGE-
// RESERVATION-WIRE.md). It is the only production caller of the store's
// reservation ledger. It derives the actor and the holder binding from the
// authenticated identity and, in team mode, the verified team context;
// nothing in a request can name them. It registers no route: C6 adds the
// routes and MCP tools on top of it.
//
// C5a authorizes what needs no aicrew coordination facts: personal
// standalone holds (claim, update, release, finalize), the team holder's
// update, and status and receipt reads. Team claim, transfer, release and
// finalize wait for C5b.

import (
	"context"
	"errors"
	"time"

	"aimem/internal/store"
)

// reservationVerificationAge bounds how old a team context's online answer
// may be when a transition starts (decision D2: the answer is taken once,
// before the transaction, within the claim's own deadline).
const reservationVerificationAge = 5 * time.Second

// reservationRefusal is a refusal in the reservation wire's terms. Detail
// is for the hub's log only; it never names another holder.
type reservationRefusal struct {
	Code   string
	Detail string
}

func (r *reservationRefusal) Error() string { return r.Code + ": " + r.Detail }

// Retryable follows the wire's refusal table.
func (r *reservationRefusal) Retryable() bool {
	return r.Code == "context_unavailable" || r.Code == "receipt_unresolved"
}

// beforeReservationRecheck, when a test sets it, runs each time the ledger
// rechecks the caller's authority inside a transition, so a test can revoke
// access at exactly that point.
var beforeReservationRecheck func()

func refuseReservation(code, detail string) error {
	return &reservationRefusal{Code: code, Detail: detail}
}

// reservationCaller is the verified party behind one call.
type reservationCaller struct {
	id      Identity
	team    *teamContext
	actor   store.TaskActor
	binding store.ReservationBinding
}

// reservationCallerFrom derives the caller from the request context only:
// the authenticated identity and, when the middleware verified one, the
// team context. Admin and legacy credentials have no path here.
func reservationCallerFrom(ctx context.Context) (reservationCaller, error) {
	id, ok := IdentityFrom(ctx)
	switch {
	case !ok:
		return reservationCaller{}, refuseReservation("invalid_credential", "no authenticated individual")
	case id.Role != "user" || id.UserID == "" || id.TokenID == "":
		return reservationCaller{}, refuseReservation("grant_denied", "role "+id.Role+" has no reservation path")
	}
	c := reservationCaller{id: id, actor: store.TaskActor{Kind: "user", UserID: id.UserID, TokenID: id.TokenID, Name: id.Name},
		binding: store.ReservationBinding{UserID: id.UserID, Mode: "personal"}}
	if tc, ok := teamContextFrom(ctx); ok {
		switch {
		case tc.UserID != id.UserID || tc.TokenID != id.TokenID:
			return reservationCaller{}, refuseReservation("identity_mismatch", "team context of another credential")
		case time.Since(tc.VerifiedAt) > reservationVerificationAge:
			return reservationCaller{}, refuseReservation("context_unavailable", "team context verification is too old")
		}
		c.team = &tc
		c.binding = store.ReservationBinding{UserID: id.UserID, Mode: "team", ProfileID: tc.ProfileID, TeamID: tc.TeamID,
			Role: tc.Role, SessionID: tc.SessionID, Generation: tc.Generation}
	}
	return c, nil
}

// reservationPermits applies the operation policy for C5a.
func reservationPermits(c reservationCaller, op store.ReservationOperation, in store.TaskReservationInput) error {
	if op == store.ReservationTransfer {
		return refuseReservation("role_forbidden", "transfer needs verified aicrew coordination")
	}
	if c.team == nil {
		if op == store.ReservationClaim && in.Holder.Mode != "standalone" {
			return refuseReservation("invalid_request", "a personal claim holds in standalone mode")
		}
		return nil
	}
	switch {
	case op != store.ReservationUpdate:
		return refuseReservation("role_forbidden", "team "+string(op)+" needs verified aicrew coordination")
	case c.binding.Role != "worker" && c.binding.Role != "independent":
		return refuseReservation("role_forbidden", "only a worker or independent holder updates its work")
	}
	return nil
}

// reservationAuthority decides, now, whether the caller may act on project
// through the access instance it had when the call began. The owner project
// needs the caller's write authority: a personal write grant, or the team
// profile's grant. A dependency project needs read authority: in personal
// mode a live credential whose scope reaches it, in team mode the profile's
// grant. It reads only the access store and the registry, never a project
// database, so the ledger can run it inside its transaction.
func (s *Server) reservationAuthority(c reservationCaller, project, instance string, owner bool) error {
	current, err := s.reg.ExistingProjectAccessID(project)
	if err != nil || current == "" || current != instance {
		return refuseReservation("task_unavailable", "project access identity changed or unavailable")
	}
	db, err := s.openAccess(false)
	if err != nil {
		return refuseReservation("context_unavailable", "access store")
	}
	live, err := db.TokenLive(c.id.UserID, c.id.TokenID, instance)
	switch {
	case err != nil:
		return refuseReservation("context_unavailable", "access store")
	case !live:
		return refuseReservation("invalid_credential", "credential revoked, expired or out of scope")
	}
	var allowed bool
	if c.team == nil {
		if !owner {
			return nil
		}
		allowed, err = db.CanWriteToken(c.id.UserID, c.id.TokenID, instance)
	} else {
		profile, perr := db.TeamProfileByKey(c.team.ServiceID, c.team.TeamID)
		if perr != nil || profile.Disabled || profile.ID != c.team.ProfileID {
			return refuseReservation("context_stale", "team profile disabled or replaced")
		}
		allowed, err = db.TeamGrantAllows(c.id.UserID, c.id.TokenID, c.team.ProfileID, instance)
	}
	switch {
	case err != nil:
		return refuseReservation("context_unavailable", "access store")
	case !allowed:
		return refuseReservation("grant_denied", "no live grant on the project")
	}
	return nil
}

// reservationTarget locates a task's project and the access instance the
// call is authorized against.
func (s *Server) reservationTarget(taskID string) (string, *store.DB, string, error) {
	project, db, err := s.reg.LocateTask(taskID)
	if err != nil {
		return "", nil, "", reservationError(err)
	}
	instance, err := s.reg.ExistingProjectAccessID(project)
	if err != nil || instance == "" {
		return "", nil, "", refuseReservation("task_unavailable", "project has no access identity")
	}
	return project, db, instance, nil
}

// reserve runs one reservation transition for the caller in ctx.
func (s *Server) reserve(ctx context.Context, op store.ReservationOperation, in store.TaskReservationInput, key string) (store.TaskReservationOutcome, error) {
	c, err := reservationCallerFrom(ctx)
	if err != nil {
		return store.TaskReservationOutcome{}, err
	}
	if err := reservationPermits(c, op, in); err != nil {
		return store.TaskReservationOutcome{}, err
	}
	project, _, instance, err := s.reservationTarget(in.TaskID)
	if err != nil {
		return store.TaskReservationOutcome{}, err
	}
	if err := s.reservationAuthority(c, project, instance, true); err != nil {
		return store.TaskReservationOutcome{}, err
	}
	var out store.TaskReservationOutcome
	if op == store.ReservationClaim {
		verify := func(_ context.Context, p, accessID string) error {
			if beforeReservationRecheck != nil {
				beforeReservationRecheck()
			}
			err := s.reservationAuthority(c, p, accessID, p == project)
			if err != nil && p != project {
				// A dependency the caller cannot read is an unresolved
				// dependency; its own refusal would describe a project the
				// caller did not ask about.
				return errors.New("dependency project not readable in this context")
			}
			return err
		}
		out, err = s.reg.ClaimTaskReservation(ctx, in, c.actor, c.binding, key, verify)
	} else {
		out, err = s.reg.ApplyTaskReservation(ctx, op, in, c.actor, c.binding, key, func() error {
			if beforeReservationRecheck != nil {
				beforeReservationRecheck()
			}
			return s.reservationAuthority(c, project, instance, true)
		})
	}
	if err != nil {
		return store.TaskReservationOutcome{}, reservationError(err)
	}
	return out, nil
}

// reservationState is a status read: the caller's own hold, or none.
type reservationState struct {
	State       string                 `json:"state"`
	Reservation *store.TaskReservation `json:"reservation,omitempty"`
}

// reservationStatus reports the caller's own hold on a task. Any other
// holder's hold reads as none: the answer never names another holder.
func (s *Server) reservationStatus(ctx context.Context, taskID string) (reservationState, error) {
	c, err := reservationCallerFrom(ctx)
	if err != nil {
		return reservationState{}, err
	}
	project, db, instance, err := s.reservationTarget(taskID)
	if err != nil {
		return reservationState{}, err
	}
	if err := s.reservationAuthority(c, project, instance, true); err != nil {
		return reservationState{}, err
	}
	r, err := db.GetTaskReservation(taskID)
	if err != nil {
		return reservationState{}, reservationError(err)
	}
	if r.ID == "" || !r.Binding.SameHolder(c.binding) {
		return reservationState{State: "none"}, nil
	}
	return reservationState{State: "held", Reservation: &r}, nil
}

// reservationReceipt reconciles a lost reply: the recorded outcome of the
// caller's own transition, after its current authority is rechecked.
func (s *Server) reservationReceipt(ctx context.Context, op store.ReservationOperation, in store.TaskReservationInput, key string) (store.TaskReservationOutcome, bool, error) {
	c, err := reservationCallerFrom(ctx)
	if err != nil {
		return store.TaskReservationOutcome{}, false, err
	}
	project, db, instance, err := s.reservationTarget(in.TaskID)
	if err != nil {
		return store.TaskReservationOutcome{}, false, err
	}
	if err := s.reservationAuthority(c, project, instance, true); err != nil {
		return store.TaskReservationOutcome{}, false, err
	}
	out, found, err := db.GetTaskReservationReceipt(op, in, c.actor, c.binding, key)
	if err != nil {
		return store.TaskReservationOutcome{}, false, reservationError(err)
	}
	return out, found, nil
}

// reservationError maps a ledger error to the wire's refusal codes. A
// refusal from the authorizer itself keeps its code, even when the ledger
// wrapped it.
func reservationError(err error) error {
	var r *reservationRefusal
	var conflict *store.TaskConflict
	switch {
	case errors.As(err, &r):
		return r
	case errors.As(err, &conflict):
		return refuseReservation("revision_conflict", "expected revision is stale")
	case errors.Is(err, store.ErrReservationStale):
		return refuseReservation("stale_fence", "reservation ID or fence is stale")
	case errors.Is(err, store.ErrReservationConflict), errors.Is(err, store.ErrReservationHolder):
		return refuseReservation("reservation_conflict", "the task is not held by this holder")
	case errors.Is(err, store.ErrTaskRetryConflict):
		return refuseReservation("idempotency_conflict", "changed input under a used key")
	case errors.Is(err, store.ErrDependencyUnresolved):
		return refuseReservation("dependency_unresolved", "dependency evidence is not readable and done")
	case errors.Is(err, store.ErrTaskNotFound):
		return refuseReservation("task_unavailable", "task not found")
	case errors.Is(err, store.ErrTaskInvalid):
		return refuseReservation("invalid_request", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return refuseReservation("context_unavailable", "the claim's deadline passed")
	}
	return err
}
