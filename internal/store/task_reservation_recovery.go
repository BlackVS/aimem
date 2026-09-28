package store

// Reservation recovery (task C5c) and per-service closure evidence (task
// C5c-w, docs/DESIGN-AIFORGE-COORDINATION-WIRE.md "Closure evidence").
//
// Recovery is the one ledger transition that does not come from the holder.
// It keeps every other guard: the reservation ID, fence and task revision
// must match, the task content is complete, and a recovery can only release
// the task (READY or BLOCKED) or cancel it. It never produces DONE.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"unicode/utf8"
)

// RecoveryOperation is a recovery transition's kind, and the operation its
// receipt is stored under.
type RecoveryOperation string

const (
	RecoveryRelease RecoveryOperation = "recovery_release"
	RecoveryCancel  RecoveryOperation = "recovery_cancel"
)

// ErrRecoveryEvidence: the recovery's evidence is missing, malformed, or not
// the kind this hold allows (an unbound hold needs an attestation).
var ErrRecoveryEvidence = errors.New("recovery needs one valid evidence kind this hold accepts")

var proofDigestRE = regexp.MustCompile(`^p1_[A-Za-z0-9_-]{43}$`)

// RecoveryEvidence is why the recovery may act: a verified aicrew stop
// ("stop_evidence", Ref the p1_ digest of the stop proof the hub verified)
// or an operator attestation ("attestation", Ref its operator-chosen ID).
type RecoveryEvidence struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	Statement string `json:"statement,omitempty"`
}

func (e RecoveryEvidence) validate() error {
	switch e.Kind {
	case "stop_evidence":
		if proofDigestRE.MatchString(e.Ref) && e.Statement == "" {
			return nil
		}
	case "attestation":
		n := utf8.RuneCountInString(e.Statement)
		if taskText(e.Ref, 128, true) == nil && taskText(e.Statement, 8192, true) == nil && n >= 16 && n <= 2048 {
			return nil
		}
	}
	return ErrRecoveryEvidence
}

// RecoveryRecord is what a recovery receipt records beyond the transition:
// the operation, the evidence kind and reference (never the statement), and
// the affected actor, the hold's stored binding; a zero binding is a hold
// that predates bindings.
type RecoveryRecord struct {
	Operation    RecoveryOperation  `json:"operation"`
	EvidenceKind string             `json:"evidence_kind"`
	EvidenceRef  string             `json:"evidence_ref"`
	Affected     ReservationBinding `json:"affected"`
}

// RecoverTaskReservation closes a hold its holder cannot close, under the
// project lifecycle lock like every other transition. actor is the recovery
// principal (kind "recovery", the admin credential's name); authorize is the
// route's recheck of that admin, run before commit and on replay.
func (r *Registry) RecoverTaskReservation(ctx context.Context, op RecoveryOperation, in TaskReservationInput, evidence RecoveryEvidence,
	actor TaskActor, key string, authorize func() error) (TaskReservationOutcome, error) {
	if err := validateRecovery(op, &in, evidence, actor); err != nil {
		return TaskReservationOutcome{}, err
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
	binding := ReservationBinding{UserID: actor.Name, Mode: "recovery"}
	return reservationMutation(db, actor, binding, ReservationOperation(op), in, key, evidence,
		func(tx *sql.Tx) (TaskReservationOutcome, error) {
			t, err := readTask(tx, in.TaskID)
			if err != nil {
				return TaskReservationOutcome{}, err
			}
			if t.Revision != in.ExpectedRevision {
				return TaskReservationOutcome{}, &TaskConflict{Current: t}
			}
			hold, err := readTaskReservation(tx, in.TaskID, t.Revision)
			if err != nil {
				return TaskReservationOutcome{}, err
			}
			if hold.ID == "" || hold.ID != in.ID || hold.Fence != in.Fence {
				return TaskReservationOutcome{}, ErrReservationStale
			}
			affected := hold.Binding
			if affected == (ReservationBinding{}) && evidence.Kind != "attestation" {
				// A hold from before bindings names no member aicrew can
				// vouch for; only an operator attestation recovers it.
				return TaskReservationOutcome{}, ErrRecoveryEvidence
			}
			if err := checkEpicAssignable(tx, in.Content.Epic, t.Epic); err != nil {
				return TaskReservationOutcome{}, err
			}
			before := hold
			t.TaskContent = *in.Content
			t.Revision++
			t.UpdatedAt = nowUTC()
			if err := saveReservedTask(tx, t, actor); err != nil {
				return TaskReservationOutcome{}, err
			}
			hold.TaskRevision = t.Revision
			hold.ID, hold.Holder, hold.Binding = "", ReservationHolder{}, ReservationBinding{}
			if err := advanceReservation(tx, &hold); err != nil {
				return TaskReservationOutcome{}, err
			}
			record := &RecoveryRecord{Operation: op, EvidenceKind: evidence.Kind, EvidenceRef: evidence.Ref, Affected: affected}
			if err := recordRecoveryEvent(tx, before, hold, actor, record, evidence.Statement, in.Reason); err != nil {
				return TaskReservationOutcome{}, err
			}
			if err := trackServiceReservation(tx, before, hold, string(op)); err != nil {
				return TaskReservationOutcome{}, err
			}
			return TaskReservationOutcome{Task: t, Reservation: hold, Recovery: record}, nil
		}, authorize)
}

func validateRecovery(op RecoveryOperation, in *TaskReservationInput, evidence RecoveryEvidence, actor TaskActor) error {
	switch op {
	case RecoveryRelease:
		if err := validateReservationInput(ReservationRelease, in); err != nil {
			return err
		}
	case RecoveryCancel:
		if err := validateReservationInput(ReservationFinalize, in); err != nil {
			return err
		}
		if in.Content.State != "CANCELLED" {
			return invalid(errors.New("a recovery can cancel a task, never complete it"))
		}
	default:
		return invalid(errors.New("unknown recovery operation"))
	}
	if err := evidence.validate(); err != nil {
		return err
	}
	_, err := recoveryPrincipal(actor)
	return err
}

// RecoveryReplay finds the committed receipt of this exact recovery request
// (the same principal, operation, task, key, input and evidence) without
// running it. The route consults it before asking aicrew about a stop proof
// again: a replay never re-queries a coordination fact (the replay rule). A
// changed request under a used key is ErrTaskRetryConflict.
func (r *Registry) RecoveryReplay(op RecoveryOperation, in TaskReservationInput, evidence RecoveryEvidence,
	actor TaskActor, key string) (TaskReservationOutcome, bool, error) {
	if err := validateRecovery(op, &in, evidence, actor); err != nil {
		return TaskReservationOutcome{}, false, err
	}
	_, db, err := r.LocateTask(in.TaskID)
	if err != nil {
		return TaskReservationOutcome{}, false, err
	}
	principal, _ := recoveryPrincipal(actor)
	digest, err := receiptDigest(struct {
		In    TaskReservationInput `json:"in"`
		Extra any                  `json:"extra"`
	}{in, evidence})
	if err != nil {
		return TaskReservationOutcome{}, false, err
	}
	var savedDigest, saved string
	err = db.sql.QueryRow(`SELECT digest,result FROM task_reservation_requests WHERE principal=? AND operation=? AND task_id=? AND key=?`,
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

// recordRecoveryEvent keeps the full recovery record, including the
// attestation statement, in the project's reservation event log.
func recordRecoveryEvent(tx *sql.Tx, before, after TaskReservation, actor TaskActor, record *RecoveryRecord, statement, reason string) error {
	body, err := json.Marshal(struct {
		Before    TaskReservation `json:"before"`
		After     TaskReservation `json:"after"`
		Actor     TaskActor       `json:"actor"`
		Recovery  *RecoveryRecord `json:"recovery"`
		Statement string          `json:"statement,omitempty"`
		Reason    string          `json:"reason"`
	}{before, after, actor, record, statement, reason})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO task_reservation_events(task_id,fence,operation,body) VALUES(?,?,?,?)`,
		after.TaskID, after.Fence, string(record.Operation), string(body))
	return err
}

// holderClosedBy is the closure evidence a member transition records.
func holderClosedBy(op ReservationOperation) string {
	switch op {
	case ReservationRelease:
		return "holder_release"
	case ReservationFinalize:
		return "holder_finalize"
	}
	return ""
}

// trackServiceReservation keeps, per task and service, the last reservation
// that service's verified team context established (a claim or transfer
// bound to it) and how it closed. The hub's read scope answers "closed" from
// it, whatever holds the task afterwards.
func trackServiceReservation(tx *sql.Tx, before, after TaskReservation, closedBy string) error {
	switch {
	case after.ID != "" && after.Binding.ServiceID != "" &&
		(after.ID != before.ID || after.Binding.ServiceID != before.Binding.ServiceID):
		_, err := tx.Exec(`INSERT INTO task_reservation_services(task_id,service_id,reservation_id,closing_fence,closed_by,closed_at,task_revision)
			VALUES(?,?,?,0,'','',?) ON CONFLICT(task_id,service_id) DO UPDATE SET
			reservation_id=excluded.reservation_id,closing_fence=0,closed_by='',closed_at='',task_revision=excluded.task_revision`,
			after.TaskID, after.Binding.ServiceID, after.ID, after.TaskRevision)
		return err
	case before.ID != "" && after.ID == "" && before.Binding.ServiceID != "":
		if closedBy == "" {
			return errors.New("closing a service's reservation without a closure kind")
		}
		_, err := tx.Exec(`UPDATE task_reservation_services SET closing_fence=?,closed_by=?,closed_at=?,task_revision=?
			WHERE task_id=? AND service_id=? AND reservation_id=?`,
			after.Fence, closedBy, nowUTC(), after.TaskRevision, after.TaskID, before.Binding.ServiceID, before.ID)
		return err
	}
	return nil
}

// ServiceHold is a hold as one service's read scope sees it (C5c-w): its own
// current hold, its own most recent reservation closed, or none. It never
// describes another holder.
type ServiceHold struct {
	State         string `json:"state"`
	ReservationID string `json:"reservation_id,omitempty"`
	Fence         int64  `json:"fence,omitempty"`
	HolderMode    string `json:"holder_mode,omitempty"`
	OwnWorkRef    string `json:"own_work_ref,omitempty"`
	TaskRevision  int64  `json:"task_revision,omitempty"`
	ClosingFence  int64  `json:"closing_fence,omitempty"`
	ClosedBy      string `json:"closed_by,omitempty"`
	ClosedAt      string `json:"closed_at,omitempty"`
}

// ServiceHoldStatus answers hold status for serviceID. The caller must have
// authenticated that service's read credential.
func (d *DB) ServiceHoldStatus(taskID, serviceID string) (ServiceHold, error) {
	if err := d.taskScopeOK(); err != nil {
		return ServiceHold{}, err
	}
	if !taskIDRE.MatchString(taskID) || serviceID == "" {
		return ServiceHold{State: "none"}, nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return ServiceHold{}, err
	}
	defer tx.Rollback()
	t, err := readTask(tx, taskID)
	if errors.Is(err, ErrTaskNotFound) {
		return ServiceHold{State: "none"}, nil
	}
	if err != nil {
		return ServiceHold{}, err
	}
	hold, err := readTaskReservation(tx, taskID, t.Revision)
	if err != nil {
		return ServiceHold{}, err
	}
	if hold.ID != "" && hold.Binding.ServiceID == serviceID {
		return ServiceHold{State: "held", ReservationID: hold.ID, Fence: hold.Fence, HolderMode: hold.Holder.Mode,
			OwnWorkRef: hold.Holder.Ref, TaskRevision: t.Revision}, nil
	}
	var out ServiceHold
	err = tx.QueryRow(`SELECT reservation_id,closing_fence,closed_by,closed_at,task_revision FROM task_reservation_services
		WHERE task_id=? AND service_id=? AND closing_fence>0`, taskID, serviceID).
		Scan(&out.ReservationID, &out.ClosingFence, &out.ClosedBy, &out.ClosedAt, &out.TaskRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceHold{State: "none"}, nil
	}
	if err != nil {
		return ServiceHold{}, err
	}
	out.State = "closed"
	return out, nil
}

// RequestKeyDigest is the k1_ encoding of a request key (identity.v1).
func RequestKeyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// RecoveryReceipt is one committed receipt as the recovery reader sees it.
type RecoveryReceipt struct {
	Principal string                 `json:"principal"`
	Outcome   TaskReservationOutcome `json:"outcome"`
}

// RecoveryReceipts returns every committed receipt for a task, operation and
// request-key digest, whoever made it. It is the recovery reader's lookup;
// the caller must have authorized the recovery principal.
func (d *DB) RecoveryReceipts(taskID string, op ReservationOperation, keyDigest string) ([]RecoveryReceipt, error) {
	if err := d.taskScopeOK(); err != nil {
		return nil, err
	}
	if !taskIDRE.MatchString(taskID) {
		return nil, ErrTaskNotFound
	}
	rows, err := d.sql.Query(`SELECT principal,key,result FROM task_reservation_requests WHERE task_id=? AND operation=? ORDER BY principal`,
		taskID, string(op))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecoveryReceipt{}
	for rows.Next() {
		var principal, key, result string
		if err := rows.Scan(&principal, &key, &result); err != nil {
			return nil, err
		}
		if RequestKeyDigest(key) != keyDigest {
			continue
		}
		var outcome TaskReservationOutcome
		if err := json.Unmarshal([]byte(result), &outcome); err != nil {
			return nil, err
		}
		out = append(out, RecoveryReceipt{Principal: principal, Outcome: outcome})
	}
	return out, rows.Err()
}
