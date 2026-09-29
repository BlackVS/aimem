package store

// Aicrew's read scope (task C6b; coordination wire §2): the receipts one
// aicrew service may read. It sees a transition committed under one of its
// own proofs, and a transition made on a reservation that one of its proofs
// established. Everything else, including a receipt from before schema 23,
// reads as none, the same as a missing one. Hold status is ServiceHoldStatus.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ServiceReceipt is a committed receipt as the read scope serves it. It
// names the acting member but carries no task content, holder reference,
// request key or proof.
type ServiceReceipt struct {
	ID               string
	Operation        ReservationOperation
	TaskID           string
	RequestKeyDigest string
	ReservationID    string
	Fence            int64
	TaskRevision     int64
	MemberUserID     string
	VerifiedMode     string
	CommittedAt      string
}

// serviceReceipt builds the read scope's receipt from one stored row.
func serviceReceipt(op, taskID, key, result, reservationID, committedAt string) (ServiceReceipt, error) {
	var out TaskReservationOutcome
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return ServiceReceipt{}, err
	}
	b := out.Binding
	return ServiceReceipt{
		ID: ReservationReceiptID(b.UserID, ReservationOperation(op), taskID, key), Operation: ReservationOperation(op),
		TaskID: taskID, RequestKeyDigest: RequestKeyDigest(key), ReservationID: reservationID, Fence: out.Reservation.Fence,
		TaskRevision: out.Task.Revision, MemberUserID: b.UserID, VerifiedMode: b.Mode, CommittedAt: committedAt,
	}, nil
}

// ServiceReceiptByProof returns the transition committed under the proof
// with that p1_ digest, if serviceID answered it.
func (d *DB) ServiceReceiptByProof(serviceID, proofDigest string) (ServiceReceipt, bool, error) {
	if err := d.taskScopeOK(); err != nil {
		return ServiceReceipt{}, false, err
	}
	if serviceID == "" || !proofDigestRE.MatchString(proofDigest) {
		return ServiceReceipt{}, false, nil
	}
	var op, taskID, key, result, reservationID, committedAt string
	err := d.sql.QueryRow(`SELECT operation,task_id,key,result,reservation_id,committed_at FROM task_reservation_requests
		WHERE service_id=? AND proof_digest=?`, serviceID, proofDigest).Scan(&op, &taskID, &key, &result, &reservationID, &committedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceReceipt{}, false, nil
	}
	if err != nil {
		return ServiceReceipt{}, false, err
	}
	rc, err := serviceReceipt(op, taskID, key, result, reservationID, committedAt)
	return rc, err == nil, err
}

// ServiceReceiptByKey returns the member transition on taskID with that
// operation and k1_ request-key digest, if it was made on a reservation that
// a claim or transfer under serviceID's proof established. A receipt names a
// service only when its transition was made on that service's proof. This is how aicrew
// confirms a holder's update, which carries no proof.
func (d *DB) ServiceReceiptByKey(serviceID, taskID string, op ReservationOperation, keyDigest string) (ServiceReceipt, bool, error) {
	if err := d.taskScopeOK(); err != nil {
		return ServiceReceipt{}, false, err
	}
	if serviceID == "" || !taskIDRE.MatchString(taskID) || !memberOperation(op) {
		return ServiceReceipt{}, false, nil
	}
	rows, err := d.sql.Query(`SELECT r.key,r.result,r.reservation_id,r.committed_at FROM task_reservation_requests r
		WHERE r.task_id=? AND r.operation=? AND EXISTS(
			SELECT 1 FROM task_reservation_requests e WHERE e.task_id=r.task_id AND e.reservation_id=r.reservation_id
			AND e.service_id=? AND e.operation IN ('claim','transfer'))`,
		taskID, string(op), serviceID)
	if err != nil {
		return ServiceReceipt{}, false, err
	}
	defer rows.Close()
	var found []ServiceReceipt
	for rows.Next() {
		var key, result, reservationID, committedAt string
		if err := rows.Scan(&key, &result, &reservationID, &committedAt); err != nil {
			return ServiceReceipt{}, false, err
		}
		if RequestKeyDigest(key) != keyDigest {
			continue
		}
		rc, err := serviceReceipt(string(op), taskID, key, result, reservationID, committedAt)
		if err != nil {
			return ServiceReceipt{}, false, err
		}
		if rc.VerifiedMode == "team" {
			found = append(found, rc)
		}
	}
	if err := rows.Err(); err != nil {
		return ServiceReceipt{}, false, err
	}
	switch len(found) {
	case 0:
		return ServiceReceipt{}, false, nil
	case 1:
		return found[0], true, nil
	}
	// Two members' receipts share a key on one of this service's
	// reservations. Aicrew chose that key, so the read cannot tell which
	// step it meant; it is refused rather than answered with either.
	return ServiceReceipt{}, false, fmt.Errorf("%d receipts share this request key on the service's reservation", len(found))
}

func memberOperation(op ReservationOperation) bool {
	switch op {
	case ReservationClaim, ReservationTransfer, ReservationUpdate, ReservationRelease, ReservationFinalize:
		return true
	}
	return false
}

// ServiceReceiptByProof searches every project for the transition committed
// under that proof, which the path does not locate. A project that cannot be
// read makes the answer an error, never none: none must be reliable.
func (r *Registry) ServiceReceiptByProof(serviceID, proofDigest string) (ServiceReceipt, bool, error) {
	projects, err := r.Projects()
	if err != nil {
		return ServiceReceipt{}, false, err
	}
	var unreadable error
	for _, p := range projects {
		if IsReservedProject(p) {
			continue
		}
		db, err := r.OpenExisting(p)
		if err != nil {
			unreadable = fmt.Errorf("project %q could not be opened: %w", p, err)
			continue
		}
		rc, found, err := db.ServiceReceiptByProof(serviceID, proofDigest)
		switch {
		case err != nil:
			unreadable = fmt.Errorf("project %q could not be read: %w", p, err)
		case found:
			return rc, true, nil
		}
	}
	if unreadable != nil {
		return ServiceReceipt{}, false, fmt.Errorf("receipt lookup incomplete: %w", unreadable)
	}
	return ServiceReceipt{}, false, nil
}
