package store

// Internal task reservation ledger. These methods take trusted attribution,
// like the task store; they do not authorize a caller or register a route.
// The caller policy and generic task-write protection belong to later work.

import (
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

type TaskReservation struct {
	TaskID       string            `json:"task_id"`
	ID           string            `json:"id"`
	Fence        int64             `json:"fence"`
	Holder       ReservationHolder `json:"holder"`
	TaskRevision int64             `json:"task_revision"`
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

type TaskReservationOutcome struct {
	Task        Task            `json:"task"`
	Reservation TaskReservation `json:"reservation"`
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
	err := tx.QueryRow(`SELECT fence,active_id,holder_mode,holder_ref FROM task_reservations WHERE task_id=?`, taskID).
		Scan(&r.Fence, &r.ID, &r.Holder.Mode, &r.Holder.Ref)
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
	_, err := tx.Exec(`INSERT INTO task_reservations(task_id,fence,active_id,holder_mode,holder_ref)
		VALUES(?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET
		fence=excluded.fence,active_id=excluded.active_id,
		holder_mode=excluded.holder_mode,holder_ref=excluded.holder_ref`,
		r.TaskID, r.Fence, r.ID, r.Holder.Mode, r.Holder.Ref)
	return err
}

func recordReservationEvent(tx *sql.Tx, op ReservationOperation, before, after TaskReservation, actor TaskActor, reason string) error {
	body, err := json.Marshal(struct {
		Before TaskReservation `json:"before"`
		After  TaskReservation `json:"after"`
		Actor  TaskActor       `json:"actor"`
		Reason string          `json:"reason,omitempty"`
	}{before, after, actor, reason})
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

func reservationMutation(d *DB, actor TaskActor, op ReservationOperation, in TaskReservationInput, key string,
	fn func(*sql.Tx) (TaskReservationOutcome, error)) (TaskReservationOutcome, error) {
	if err := d.taskScopeOK(); err != nil {
		return TaskReservationOutcome{}, err
	}
	principal, err := reservationPrincipal(actor)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if err := taskText(key, MaxTaskKeyBytes, true); err != nil {
		return TaskReservationOutcome{}, invalid(fmt.Errorf("idempotency key: %w", err))
	}
	digest, err := receiptDigest(in)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	defer tx.Rollback()
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
		return out, nil
	case !errors.Is(err, sql.ErrNoRows):
		return TaskReservationOutcome{}, err
	}
	out, err := fn(tx)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	result, err := json.Marshal(out)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if _, err := tx.Exec(`INSERT INTO task_reservation_requests(principal,operation,task_id,key,digest,result) VALUES(?,?,?,?,?,?)`,
		principal, string(op), in.TaskID, key, digest, string(result)); err != nil {
		return TaskReservationOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskReservationOutcome{}, err
	}
	return out, nil
}

// ApplyTaskReservation is a store-only transition. TaskActor records trusted
// attribution and scopes the receipt; it does not grant permission to act on
// the holder. Caller authorization is deliberately deferred to C4/C5.
func (d *DB) ApplyTaskReservation(op ReservationOperation, in TaskReservationInput, actor TaskActor, key string) (TaskReservationOutcome, error) {
	if err := validateReservationInput(op, &in); err != nil {
		return TaskReservationOutcome{}, err
	}
	return reservationMutation(d, actor, op, in, key,
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
				var managed bool
				if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_managed_tasks WHERE task_id=? AND managed=1)`, in.TaskID).Scan(&managed); err != nil {
					return TaskReservationOutcome{}, err
				}
				if managed || r.ID != "" || t.State != "READY" || t.Archived {
					return TaskReservationOutcome{}, ErrReservationConflict
				}
				r.ID, r.Holder = uuidv7.New(), in.Holder
			} else {
				if r.ID == "" || r.ID != in.ID || r.Fence != in.Fence {
					return TaskReservationOutcome{}, ErrReservationStale
				}
				switch op {
				case ReservationTransfer:
					r.Holder = in.Holder
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
						r.ID, r.Holder = "", ReservationHolder{}
					}
				}
			}
			if err := advanceReservation(tx, &r); err != nil {
				return TaskReservationOutcome{}, err
			}
			if err := recordReservationEvent(tx, op, before, r, actor, in.Reason); err != nil {
				return TaskReservationOutcome{}, err
			}
			return TaskReservationOutcome{Task: t, Reservation: r}, nil
		})
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
// retry key. It is internal and does not itself authorize disclosure.
func (d *DB) GetTaskReservationReceipt(op ReservationOperation, in TaskReservationInput, actor TaskActor, key string) (TaskReservationOutcome, bool, error) {
	if err := d.taskScopeOK(); err != nil {
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
	return out, true, nil
}
