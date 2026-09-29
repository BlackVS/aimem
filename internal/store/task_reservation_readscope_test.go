package store

import (
	"strings"
	"testing"
	"time"
)

// A coordinated claim's receipt is visible to the service that answered its
// fact, by proof and by key, and to no other service. A receipt from before
// schema 23 (its read-scope columns at their defaults) reads as none.
func TestServiceReceiptsAreScopedToTheAnsweringService(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	task, offer := offerClaim(t, r, db, "offer-k")

	rc, found, err := db.ServiceReceiptByProof("aicrew-example", coordProof)
	if err != nil || !found || rc.Operation != ReservationClaim || rc.TaskID != task.ID || rc.ReservationID != offer.Reservation.ID ||
		rc.Fence != 1 || rc.TaskRevision != offer.Task.Revision || rc.MemberUserID != aliceActor.UserID || rc.VerifiedMode != "team" ||
		rc.RequestKeyDigest != RequestKeyDigest("offer-k") || rc.ID != ReservationReceiptID(aliceActor.UserID, ReservationClaim, task.ID, "offer-k") {
		t.Fatalf("receipt by proof: %+v %v %v", rc, found, err)
	}
	if at, err := time.Parse(time.RFC3339, rc.CommittedAt); err != nil || time.Since(at) > time.Hour {
		t.Fatalf("committed_at %q", rc.CommittedAt)
	}
	if again, found, err := r.ServiceReceiptByProof("aicrew-example", coordProof); err != nil || !found || again != rc {
		t.Fatalf("the registry's proof search: %+v %v %v", again, found, err)
	}
	if byKey, found, err := db.ServiceReceiptByKey("aicrew-example", task.ID, ReservationClaim, RequestKeyDigest("offer-k")); err != nil || !found || byKey != rc {
		t.Fatalf("receipt by key: %+v %v %v", byKey, found, err)
	}
	for what, got := range map[string]func() (ServiceReceipt, bool, error){
		"another service by proof": func() (ServiceReceipt, bool, error) { return db.ServiceReceiptByProof("aicrew-other", coordProof) },
		"another service by key": func() (ServiceReceipt, bool, error) {
			return db.ServiceReceiptByKey("aicrew-other", task.ID, ReservationClaim, RequestKeyDigest("offer-k"))
		},
		"another operation by key": func() (ServiceReceipt, bool, error) {
			return db.ServiceReceiptByKey("aicrew-example", task.ID, ReservationUpdate, RequestKeyDigest("offer-k"))
		},
	} {
		if _, found, err := got(); err != nil || found {
			t.Fatalf("%s: found %v, %v", what, found, err)
		}
	}

	// The migration gives an older receipt empty read-scope columns.
	if _, err := db.sql.Exec(`UPDATE task_reservation_requests SET service_id='',proof_digest='',reservation_id='',committed_at=''`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ServiceReceiptByProof("aicrew-example", coordProof); err != nil || found {
		t.Fatalf("a pre-migration receipt by proof: %v %v", found, err)
	}
	if _, found, err := db.ServiceReceiptByKey("aicrew-example", task.ID, ReservationClaim, RequestKeyDigest("offer-k")); err != nil || found {
		t.Fatalf("a pre-migration receipt by key: %v %v", found, err)
	}
}

// Two members' receipts sharing a key on one of the service's reservations
// cannot be told apart: the read is refused rather than answered with either.
func TestServiceReceiptByKeyRefusesAnAmbiguousKey(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	task, _ := offerClaim(t, r, db, "offer-dup")
	if _, err := db.sql.Exec(`INSERT INTO task_reservation_requests(principal,operation,task_id,key,digest,result,service_id,proof_digest,reservation_id,committed_at)
		SELECT 'user/someone-else',operation,task_id,key,digest,result,service_id,proof_digest,reservation_id,committed_at
		FROM task_reservation_requests WHERE key='offer-dup'`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ServiceReceiptByKey("aicrew-example", task.ID, ReservationClaim, RequestKeyDigest("offer-dup")); err == nil || found {
		t.Fatalf("an ambiguous key: found %v, %v", found, err)
	}
}

// Only a claim or transfer under the service's proof puts a reservation in
// the by-key scope. A later proof-backed step of the service on a
// reservation it did not establish is readable by its proof only.
func TestServiceReceiptByKeyNeedsTheEstablishingProof(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	task, offer := offerClaim(t, r, db, "offer-est")
	never := &ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "never_accepted", ProofDigest: "p1_" + strings.Repeat("N", 43)},
		WorkRef: offer.Reservation.Holder.Ref, Coordinator: true}
	in := TaskReservationInput{TaskID: task.ID, ID: offer.Reservation.ID, Fence: offer.Reservation.Fence, ExpectedRevision: offer.Task.Revision,
		Content: &TaskContent{Title: task.Title, State: "READY"}, Reason: "never accepted"}
	if _, err := db.ApplyTaskReservation(ReservationRelease, in, aliceActor, coordinator(aliceActor.UserID, "sess-coord"), "release-est", never, allowReservation); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ServiceReceiptByKey("aicrew-example", task.ID, ReservationRelease, RequestKeyDigest("release-est")); err != nil || !found {
		t.Fatalf("the release on the service's reservation: %v %v", found, err)
	}
	// The claim that established it predates the read scope.
	if _, err := db.sql.Exec(`UPDATE task_reservation_requests SET service_id='',proof_digest='' WHERE key='offer-est'`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ServiceReceiptByKey("aicrew-example", task.ID, ReservationRelease, RequestKeyDigest("release-est")); err != nil || found {
		t.Fatalf("a reservation the service's proof did not establish: %v %v", found, err)
	}
	if _, found, err := db.ServiceReceiptByProof("aicrew-example", never.ProofDigest); err != nil || !found {
		t.Fatalf("the release by its own proof: %v %v", found, err)
	}
}

// A project whose read fails makes the proof search an error, never none.
func TestServiceReceiptByProofRefusesAnIncompleteSearch(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	offerClaim(t, r, db, "offer-inc")
	other, err := r.Open("other-project")
	if err != nil {
		t.Fatal(err)
	}
	other.sql.Close()
	if _, found, err := r.ServiceReceiptByProof("aicrew-example", "p1_"+strings.Repeat("Z", 43)); err == nil || found {
		t.Fatalf("a search past an unreadable project: %v %v", found, err)
	}
}

// The proof search reads every project, so each project's lookup must use
// the index, not scan the receipts. (A partial index on proof_digest<>”
// would not serve a bound digest.)
func TestServiceReceiptByProofUsesTheIndex(t *testing.T) {
	_, db := taskDB(t)
	var id, parent, unused int
	var plan string
	if err := db.sql.QueryRow(`EXPLAIN QUERY PLAN SELECT operation,task_id,key,result,reservation_id,committed_at FROM task_reservation_requests
		WHERE service_id=? AND proof_digest=?`, "aicrew-example", "p1_x").Scan(&id, &parent, &unused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "USING INDEX idx_task_reservation_requests_proof") {
		t.Fatalf("the proof lookup plan: %s", plan)
	}
}
