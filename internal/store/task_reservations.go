package store

// Internal task reservation ledger. These methods take trusted attribution,
// like the task store; they do not authorize a caller or register a route.
// The caller policy and generic task-write protection belong to later work.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"aimem/internal/uuidv7"
)

type ReservationOperation string

const (
	ReservationClaim    ReservationOperation = "claim"
	ReservationTransfer ReservationOperation = "transfer"
	ReservationUpdate   ReservationOperation = "update"
	ReservationRelease  ReservationOperation = "release"
	ReservationFinalize ReservationOperation = "finalize"
)

var (
	ErrReservationConflict = errors.New("task already has an active reservation or is legacy managed")
	ErrReservationStale    = errors.New("reservation ID or fence is stale")
	ErrReservationOverflow = errors.New("reservation fence exhausted")
	ErrTaskReserved        = errors.New("task has an active reservation; generic task writes are unavailable")
	// ErrReservationHolder: the caller is not the verified holder the
	// reservation, or the receipt, is bound to.
	ErrReservationHolder = errors.New("reservation is bound to another holder")
)

func rejectActiveReservation(tx *sql.Tx, taskID string) error {
	var active bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_reservations WHERE task_id=? AND active_id<>'')`, taskID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrTaskReserved
	}
	return nil
}

// ReservationHolder is an opaque storage label, not proof of actor authority.
// Its mode and reference semantics are fixed by later reviewed contracts.
type ReservationHolder struct {
	Mode string `json:"mode"`
	Ref  string `json:"ref"`
}

// ReservationBinding is the holder as the hub's authorizer verified it: the
// individual user, the mode (personal or team) and, in team mode, the
// service, profile, team, role, session and generation. The authorizer
// derives it from the authenticated connection; no request field can name
// it. Mode "recovery" is the binding of a recovery principal (C5c): its
// UserID is the admin credential's name, and nothing else is set.
type ReservationBinding struct {
	UserID     string `json:"user_id"`
	Mode       string `json:"mode"`
	ServiceID  string `json:"service_id,omitempty"`
	ProfileID  string `json:"profile_id,omitempty"`
	TeamID     string `json:"team_id,omitempty"`
	Role       string `json:"role,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Generation string `json:"generation,omitempty"`
}

// SameHolder reports whether b and o are the same holder: user, mode,
// profile and role. The session and generation may move on (a resumed
// session keeps its hold); a token is never part of the binding, so a
// rotated credential keeps it too.
func (b ReservationBinding) SameHolder(o ReservationBinding) bool {
	return b.UserID != "" && b.Mode != "" &&
		b.UserID == o.UserID && b.Mode == o.Mode && b.ProfileID == o.ProfileID && b.Role == o.Role
}

func (b ReservationBinding) validate() error {
	switch {
	case b.UserID == "":
		return invalid(errors.New("reservation binding needs a user"))
	case (b.Mode == "personal" || b.Mode == "recovery") && b.ServiceID == "" && b.ProfileID == "" && b.TeamID == "" && b.Role == "" && b.SessionID == "" && b.Generation == "":
		return nil
	case b.Mode == "team" && b.ServiceID != "" && b.ProfileID != "" && b.TeamID != "" && b.Role != "" && b.SessionID != "" && b.Generation != "":
		return nil
	}
	return invalid(errors.New("reservation binding is incomplete"))
}

type TaskReservation struct {
	TaskID       string             `json:"task_id"`
	ID           string             `json:"id"`
	Fence        int64              `json:"fence"`
	Holder       ReservationHolder  `json:"holder"`
	Binding      ReservationBinding `json:"binding"`
	TaskRevision int64              `json:"task_revision"`
}

type TaskReservationInput struct {
	TaskID           string            `json:"task_id"`
	ID               string            `json:"reservation_id,omitempty"`
	Fence            int64             `json:"fence,omitempty"`
	ExpectedRevision int64             `json:"expected_revision"`
	Holder           ReservationHolder `json:"holder,omitempty"`
	Content          *TaskContent      `json:"content,omitempty"`
	Reason           string            `json:"reason,omitempty"`
}

// TaskReservationOutcome is a committed transition, as its receipt records
// it. Binding is the verified caller that made the transition; a replay or
// receipt read is served only to that same holder.
type TaskReservationOutcome struct {
	Task        Task               `json:"task"`
	Reservation TaskReservation    `json:"reservation"`
	Binding     ReservationBinding `json:"binding"`
	Recovery    *RecoveryRecord    `json:"recovery,omitempty"`
	// Replayed marks an outcome served from its committed receipt rather
	// than a new transition. It is never stored in the receipt.
	Replayed bool `json:"-"`
}

// DependencyEvidence is the authoritative state observed while the claim held
// write-intent locks on the dependency project. It grants no future authority.
type DependencyEvidence struct {
	TaskID   string `json:"task_id"`
	Project  string `json:"project"`
	AccessID string `json:"access_id"`
	Revision int64  `json:"revision"`
}

func validReservationOperation(op ReservationOperation) bool {
	switch op {
	case ReservationClaim, ReservationTransfer, ReservationUpdate, ReservationRelease, ReservationFinalize:
		return true
	}
	return false
}

func validateReservationInput(op ReservationOperation, in *TaskReservationInput) error {
	if !validReservationOperation(op) {
		return invalid(errors.New("unknown reservation operation"))
	}
	if !taskIDRE.MatchString(in.TaskID) || in.ExpectedRevision < 1 {
		return invalid(errors.New("task ID and positive expected revision required"))
	}
	if err := taskText(in.Reason, 4096, false); err != nil {
		return invalid(fmt.Errorf("reason: %w", err))
	}
	if op == ReservationClaim || op == ReservationTransfer {
		if err := taskText(in.Holder.Mode, 64, true); err != nil {
			return invalid(fmt.Errorf("holder mode: %w", err))
		}
		if err := taskText(in.Holder.Ref, 256, true); err != nil {
			return invalid(fmt.Errorf("holder reference: %w", err))
		}
	} else if in.Holder != (ReservationHolder{}) {
		return invalid(errors.New("holder belongs to claim or transfer"))
	}
	if op == ReservationClaim {
		if in.ID != "" || in.Fence != 0 || in.Content != nil || in.Reason != "" {
			return invalid(errors.New("claim takes only task, revision and holder"))
		}
	} else if !taskIDRE.MatchString(in.ID) || in.Fence < 1 {
		return invalid(errors.New("current reservation ID and fence required"))
	}
	if op == ReservationUpdate || op == ReservationRelease || op == ReservationFinalize {
		if in.Content == nil {
			return invalid(errors.New("task content required"))
		}
		if err := in.Content.validate(); err != nil {
			return invalid(err)
		}
	} else if in.Content != nil {
		return invalid(errors.New("task content belongs to update, release or finalize"))
	}
	if op == ReservationRelease || op == ReservationFinalize {
		if err := taskText(in.Reason, 4096, true); err != nil {
			return invalid(fmt.Errorf("reason: %w", err))
		}
	}
	if op == ReservationRelease && in.Content.State != "READY" && in.Content.State != "BLOCKED" {
		return invalid(errors.New("release must leave task READY or BLOCKED"))
	}
	if op == ReservationUpdate && (in.Content.State == "DONE" || in.Content.State == "CANCELLED") {
		return invalid(errors.New("terminal state requires finalize"))
	}
	if op == ReservationFinalize && in.Content.State != "DONE" && in.Content.State != "CANCELLED" {
		return invalid(errors.New("finalize must leave task DONE or CANCELLED"))
	}
	return nil
}

func readTaskReservation(tx *sql.Tx, taskID string, revision int64) (TaskReservation, error) {
	r := TaskReservation{TaskID: taskID, TaskRevision: revision}
	b := &r.Binding
	err := tx.QueryRow(`SELECT fence,active_id,holder_mode,holder_ref,
		bound_user,bound_mode,bound_service,bound_profile,bound_team,bound_role,bound_session,bound_generation
		FROM task_reservations WHERE task_id=?`, taskID).
		Scan(&r.Fence, &r.ID, &r.Holder.Mode, &r.Holder.Ref,
			&b.UserID, &b.Mode, &b.ServiceID, &b.ProfileID, &b.TeamID, &b.Role, &b.SessionID, &b.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	return r, err
}

func advanceReservation(tx *sql.Tx, r *TaskReservation) error {
	if r.Fence == math.MaxInt64 {
		return ErrReservationOverflow
	}
	r.Fence++
	b := r.Binding
	_, err := tx.Exec(`INSERT INTO task_reservations(task_id,fence,active_id,holder_mode,holder_ref,
		bound_user,bound_mode,bound_service,bound_profile,bound_team,bound_role,bound_session,bound_generation)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET
		fence=excluded.fence,active_id=excluded.active_id,
		holder_mode=excluded.holder_mode,holder_ref=excluded.holder_ref,
		bound_user=excluded.bound_user,bound_mode=excluded.bound_mode,bound_service=excluded.bound_service,
		bound_profile=excluded.bound_profile,bound_team=excluded.bound_team,bound_role=excluded.bound_role,
		bound_session=excluded.bound_session,bound_generation=excluded.bound_generation`,
		r.TaskID, r.Fence, r.ID, r.Holder.Mode, r.Holder.Ref,
		b.UserID, b.Mode, b.ServiceID, b.ProfileID, b.TeamID, b.Role, b.SessionID, b.Generation)
	return err
}

func recordReservationEvent(tx *sql.Tx, op ReservationOperation, before, after TaskReservation, actor TaskActor,
	binding ReservationBinding, reason string, deps []DependencyEvidence) error {
	body, err := json.Marshal(struct {
		Before       TaskReservation      `json:"before"`
		After        TaskReservation      `json:"after"`
		Actor        TaskActor            `json:"actor"`
		Binding      ReservationBinding   `json:"binding"`
		Reason       string               `json:"reason,omitempty"`
		Dependencies []DependencyEvidence `json:"dependencies"`
	}{before, after, actor, binding, reason, deps})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO task_reservation_events(task_id,fence,operation,body) VALUES(?,?,?,?)`,
		after.TaskID, after.Fence, string(op), string(body))
	return err
}

// The receipt belongs to the stable actor, not one credential. The later
// authorization layer must still check current credentials before replay or
// disclosure; this storage helper makes no such decision.
func reservationPrincipal(actor TaskActor) (string, error) {
	if _, err := actor.key(); err != nil {
		return "", err
	}
	if actor.Kind == "user" {
		return "user/" + actor.UserID, nil
	}
	return "admin/" + actor.Name, nil
}

// recoveryPrincipal scopes a recovery principal's receipts. The recovery
// kind is accepted here only, never by a generic task write.
func recoveryPrincipal(actor TaskActor) (string, error) {
	if actor.Kind != "recovery" || actor.UserID != "" || actor.TokenID != "" {
		return "", errors.New("invalid recovery actor")
	}
	if err := taskText(actor.Name, maxTaskActorName, true); err != nil {
		return "", fmt.Errorf("invalid recovery actor name: %w", err)
	}
	return "recovery/" + actor.Name, nil
}

func reservationMutation(d *DB, actor TaskActor, binding ReservationBinding, op ReservationOperation, in TaskReservationInput, key string,
	extra any, fn func(*sql.Tx) (TaskReservationOutcome, error), authorize func() error) (TaskReservationOutcome, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	defer tx.Rollback()
	out, err := reservationMutationTx(d, tx, actor, binding, op, in, key, extra, fn, authorize)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskReservationOutcome{}, err
	}
	return out, nil
}

// reservationMutationTx lets the registry retain dependency locks through the
// owner commit. authorize runs on both receipt replay and immediately before
// commit; it must not access this project's single-connection DB. A replay
// is served only to the holder binding that made the transition. extra is
// further request input the receipt digest covers (a recovery's evidence);
// nil keeps the digest of in alone.
func reservationMutationTx(d *DB, tx *sql.Tx, actor TaskActor, binding ReservationBinding, op ReservationOperation, in TaskReservationInput, key string,
	extra any, fn func(*sql.Tx) (TaskReservationOutcome, error), authorize func() error) (TaskReservationOutcome, error) {
	if err := d.taskScopeOK(); err != nil {
		return TaskReservationOutcome{}, err
	}
	if authorize == nil {
		return TaskReservationOutcome{}, errors.New("reservation transition without an authorization check")
	}
	if err := binding.validate(); err != nil {
		return TaskReservationOutcome{}, err
	}
	var principal string
	var err error
	if binding.Mode == "recovery" {
		principal, err = recoveryPrincipal(actor)
	} else {
		principal, err = reservationPrincipal(actor)
	}
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if err := taskText(key, MaxTaskKeyBytes, true); err != nil {
		return TaskReservationOutcome{}, invalid(fmt.Errorf("idempotency key: %w", err))
	}
	var digest string
	if extra == nil {
		digest, err = receiptDigest(in)
	} else {
		digest, err = receiptDigest(struct {
			In    TaskReservationInput `json:"in"`
			Extra any                  `json:"extra"`
		}{in, extra})
	}
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	var savedDigest, saved string
	err = tx.QueryRow(`SELECT digest,result FROM task_reservation_requests WHERE principal=? AND operation=? AND task_id=? AND key=?`,
		principal, string(op), in.TaskID, key).Scan(&savedDigest, &saved)
	switch {
	case err == nil:
		if savedDigest != digest {
			return TaskReservationOutcome{}, ErrTaskRetryConflict
		}
		var out TaskReservationOutcome
		if err := json.Unmarshal([]byte(saved), &out); err != nil {
			return TaskReservationOutcome{}, err
		}
		if !out.Binding.SameHolder(binding) {
			return TaskReservationOutcome{}, ErrReservationHolder
		}
		if err := authorize(); err != nil {
			return TaskReservationOutcome{}, err
		}
		out.Replayed = true
		return out, nil
	case !errors.Is(err, sql.ErrNoRows):
		return TaskReservationOutcome{}, err
	}
	out, err := fn(tx)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	out.Binding = binding
	result, err := json.Marshal(out)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if _, err := tx.Exec(`INSERT INTO task_reservation_requests(principal,operation,task_id,key,digest,result) VALUES(?,?,?,?,?,?)`,
		principal, string(op), in.TaskID, key, digest, string(result)); err != nil {
		return TaskReservationOutcome{}, err
	}
	if err := authorize(); err != nil {
		return TaskReservationOutcome{}, err
	}
	return out, nil
}

// ApplyTaskReservation is a store-only transition, called only by the hub's
// reservation authorizer. TaskActor records attribution and scopes the
// receipt; binding is the verified holder. A claim or transfer binds the
// hold to it; update, release and finalize must come from the bound holder.
// authorize is the authorizer's recheck, run immediately before commit and
// on replay; it is required.
func (d *DB) ApplyTaskReservation(op ReservationOperation, in TaskReservationInput, actor TaskActor,
	binding ReservationBinding, key string, authorize func() error) (TaskReservationOutcome, error) {
	if err := validateReservationInput(op, &in); err != nil {
		return TaskReservationOutcome{}, err
	}
	if binding.Mode == "recovery" {
		return TaskReservationOutcome{}, invalid(errors.New("a recovery goes through RecoverTaskReservation"))
	}
	return reservationMutation(d, actor, binding, op, in, key, nil,
		func(tx *sql.Tx) (TaskReservationOutcome, error) {
			t, err := readTask(tx, in.TaskID)
			if err != nil {
				return TaskReservationOutcome{}, err
			}
			if t.Revision != in.ExpectedRevision {
				return TaskReservationOutcome{}, &TaskConflict{Current: t}
			}
			r, err := readTaskReservation(tx, in.TaskID, t.Revision)
			if err != nil {
				return TaskReservationOutcome{}, err
			}
			before := r
			if op == ReservationClaim {
				if len(t.Dependencies) != 0 {
					return TaskReservationOutcome{}, ErrDependencyUnresolved
				}
				var managed bool
				if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_managed_tasks WHERE task_id=? AND managed=1)`, in.TaskID).Scan(&managed); err != nil {
					return TaskReservationOutcome{}, err
				}
				if managed || r.ID != "" || t.State != "READY" || t.Archived {
					return TaskReservationOutcome{}, ErrReservationConflict
				}
				r.ID, r.Holder, r.Binding = uuidv7.New(), in.Holder, binding
			} else {
				if r.ID == "" || r.ID != in.ID || r.Fence != in.Fence {
					return TaskReservationOutcome{}, ErrReservationStale
				}
				if op != ReservationTransfer && !r.Binding.SameHolder(binding) {
					return TaskReservationOutcome{}, ErrReservationHolder
				}
				switch op {
				case ReservationTransfer:
					r.Holder, r.Binding = in.Holder, binding
				case ReservationUpdate, ReservationRelease, ReservationFinalize:
					if err := checkEpicAssignable(tx, in.Content.Epic, t.Epic); err != nil {
						return TaskReservationOutcome{}, err
					}
					t.TaskContent = *in.Content
					t.Revision++
					t.UpdatedAt = nowUTC()
					if err := saveReservedTask(tx, t, actor); err != nil {
						return TaskReservationOutcome{}, err
					}
					r.TaskRevision = t.Revision
					if op != ReservationUpdate {
						r.ID, r.Holder, r.Binding = "", ReservationHolder{}, ReservationBinding{}
					}
				}
			}
			if err := advanceReservation(tx, &r); err != nil {
				return TaskReservationOutcome{}, err
			}
			if err := recordReservationEvent(tx, op, before, r, actor, binding, in.Reason, nil); err != nil {
				return TaskReservationOutcome{}, err
			}
			if err := trackServiceReservation(tx, before, r, holderClosedBy(op)); err != nil {
				return TaskReservationOutcome{}, err
			}
			return TaskReservationOutcome{Task: t, Reservation: r}, nil
		}, authorize)
}

// ApplyTaskReservation runs a non-claim transition under the registry's
// project lifecycle read lock, as ClaimTaskReservation does. Drop, rename
// and merge take that lock before the registry mutex and then wait for the
// project's single connection; holding it across the transaction keeps
// authorize, which reads the registry, from waiting on a lifecycle
// operation that waits on this transaction.
func (r *Registry) ApplyTaskReservation(ctx context.Context, op ReservationOperation, in TaskReservationInput, actor TaskActor,
	binding ReservationBinding, key string, authorize func() error) (TaskReservationOutcome, error) {
	if op == ReservationClaim {
		return TaskReservationOutcome{}, invalid(errors.New("a claim goes through ClaimTaskReservation"))
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyClaimTimeout)
	defer cancel()
	if err := r.lockClaimLifecycle(ctx); err != nil {
		return TaskReservationOutcome{}, err
	}
	defer r.teamMu.RUnlock()
	_, db, err := r.LocateTask(in.TaskID)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	return db.ApplyTaskReservation(op, in, actor, binding, key, authorize)
}

// GetTaskReservation is an internal status read. The caller must supply its
// own authorization before showing the holder or fence to a client.
func (d *DB) GetTaskReservation(taskID string) (TaskReservation, error) {
	if err := d.taskScopeOK(); err != nil {
		return TaskReservation{}, err
	}
	if !taskIDRE.MatchString(taskID) {
		return TaskReservation{}, ErrTaskNotFound
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return TaskReservation{}, err
	}
	defer tx.Rollback()
	t, err := readTask(tx, taskID)
	if err != nil {
		return TaskReservation{}, err
	}
	return readTaskReservation(tx, taskID, t.Revision)
}

// GetTaskReservationReceipt reconciles a lost reply with the same input and
// retry key. It serves the receipt only to the holder binding that made the
// transition; the caller must still recheck that holder's current authority.
func (d *DB) GetTaskReservationReceipt(op ReservationOperation, in TaskReservationInput, actor TaskActor,
	binding ReservationBinding, key string) (TaskReservationOutcome, bool, error) {
	if err := d.taskScopeOK(); err != nil {
		return TaskReservationOutcome{}, false, err
	}
	if err := binding.validate(); err != nil {
		return TaskReservationOutcome{}, false, err
	}
	if err := validateReservationInput(op, &in); err != nil {
		return TaskReservationOutcome{}, false, err
	}
	principal, err := reservationPrincipal(actor)
	if err != nil {
		return TaskReservationOutcome{}, false, err
	}
	if err := taskText(key, MaxTaskKeyBytes, true); err != nil {
		return TaskReservationOutcome{}, false, invalid(err)
	}
	digest, err := receiptDigest(in)
	if err != nil {
		return TaskReservationOutcome{}, false, err
	}
	var savedDigest, saved string
	err = d.sql.QueryRow(`SELECT digest,result FROM task_reservation_requests WHERE principal=? AND operation=? AND task_id=? AND key=?`,
		principal, string(op), in.TaskID, key).Scan(&savedDigest, &saved)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskReservationOutcome{}, false, nil
	}
	if err != nil {
		return TaskReservationOutcome{}, false, err
	}
	if savedDigest != digest {
		return TaskReservationOutcome{}, false, ErrTaskRetryConflict
	}
	var out TaskReservationOutcome
	if err := json.Unmarshal([]byte(saved), &out); err != nil {
		return TaskReservationOutcome{}, false, err
	}
	if !out.Binding.SameHolder(binding) {
		return TaskReservationOutcome{}, false, ErrReservationHolder
	}
	return out, true, nil
}
