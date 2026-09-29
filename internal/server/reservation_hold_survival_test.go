package server

// Task 01a0ed2f-2fd8: a reservation hold is released only by an explicit,
// evidenced transition. It survives the holder's disconnect or session end,
// an expired pre-work offer fact, and tasks being disabled and re-enabled on
// its project. (Mutations while tasks are off are refused with
// tasks_disabled: reservation_tasks_disabled_test.go.)

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"aimem/internal/introspect/introspecttest"
	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

// heldAs asserts the task's hold is exactly the reservation want, with its
// fence and holder reference.
func heldAs(t *testing.T, db *store.DB, taskID string, want store.TaskReservation) {
	t.Helper()
	got, err := db.GetTaskReservation(taskID)
	if err != nil || got.ID != want.ID || got.Fence != want.Fence || got.Holder != want.Holder || got.Binding != want.Binding {
		t.Fatalf("the hold changed: %+v, want %+v (%v)", got, want, err)
	}
}

// historyLen counts the task's history entries.
func historyLen(t *testing.T, db *store.DB, taskID string) int {
	t.Helper()
	page, err := db.TaskHistory(taskID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Changes)
}

// (a) The holder's session ends (a disconnect, or aicrew closing it, which is
// what `aimem team-session close` waits for): the hub refuses that context,
// the hold stays exactly as it was, nobody else can take it, and the same
// member in its next session updates it.
func TestHoldSurvivesTheHoldersSessionEnding(t *testing.T) {
	g := newReadRig(t)
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	task := g.readyTask(t, g.alpha)
	held, _ := g.claim(t, indep, task.ID, "attempt-s")
	before := historyLen(t, g.alpha, task.ID)

	// aicrew now answers the session as ended: the hub refuses the context.
	handle := g.teamAs(t, "independent", "agent-indep", "sess-i")
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
	r := g.call(t, g.tls, "GET", g.rpath(task.ID, ""), g.alice, rhdr("", handle), "", true)
	if e := decodeRefusal(t, r); e.Code != "context_stale" {
		t.Fatalf("the ended session's request: %d %s", r.status, r.body)
	}
	heldAs(t, g.alpha, task.ID, held.Reservation)

	// The hold is not free: another member cannot take it.
	bob := g.personal(g.bobIdentity)
	current, _ := g.alpha.GetTask(task.ID)
	_, err := g.s.reserve(bob, store.ReservationClaim, claimOf(current), "bob-"+uuidv7.New(), "")
	expectRefusal(t, "a claim of the ended session's hold", err, "reservation_conflict")
	heldAs(t, g.alpha, task.ID, held.Reservation)
	if n := historyLen(t, g.alpha, task.ID); n != before {
		t.Fatalf("the session's end wrote history: %d, then %d", before, n)
	}

	// The same member, in its next session, updates its hold.
	resumed := g.member(t, "independent", "agent-indep", "sess-i-2")
	out, err := g.s.reserve(resumed, store.ReservationUpdate, nextOf(task, held, "IN_PROGRESS"), "resume-"+uuidv7.New(), "")
	if err != nil || out.Reservation.ID != held.Reservation.ID || out.Reservation.Fence != held.Reservation.Fence+1 {
		t.Fatalf("the holder's update in its next session: %+v %v", out, err)
	}
}

// (a) A pre-work offer whose fact has expired: the coordinator's release on
// that fact is refused (coordination_rejected) and applies nothing; the
// offer's hold stays with the coordinator; a fresh fact then releases it.
func TestHoldSurvivesAnExpiredOfferFact(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	coord := g.member(t, "coordinator", "agent-coord", "sess-c")
	task := g.readyTask(t, g.alpha)
	g.answerFact(offerFact(coord, task.ID, "offer-x", "offer-9"))
	offer, err := g.s.reserve(coord, store.ReservationClaim, externalClaim(task, "offer-9"), "offer-x", testProof(t))
	if err != nil {
		t.Fatal(err)
	}
	before := historyLen(t, g.alpha, task.ID)
	expired := fact(coord, "never_accepted", "release", task.ID, "never-x")
	expired["offer_ref"] = "offer-9"
	expired["expires_at"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	g.answerFact(expired)
	_, err = g.s.reserve(coord, store.ReservationRelease, nextOf(task, offer, "READY"), "never-x", testProof(t))
	expectRefusal(t, "a release on an expired offer fact", err, "coordination_rejected")
	heldAs(t, g.alpha, task.ID, offer.Reservation)
	if n := historyLen(t, g.alpha, task.ID); n != before {
		t.Fatalf("the refused release wrote history: %d, then %d", before, n)
	}
	// A transfer to the worker on an expired acceptance is refused the same
	// way, and the offer stays the coordinator's.
	worker := g.member(t, "worker", "agent-worker", "sess-w")
	accepted := fact(worker, "accepted_attempt", "transfer", task.ID, "tr-x")
	accepted["offer_ref"], accepted["attempt_ref"], accepted["process"] = "offer-9", "attempt-9", pinOf(testPin)
	accepted["expires_at"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	g.answerFact(accepted)
	transfer := store.TaskReservationInput{TaskID: task.ID, ID: offer.Reservation.ID, Fence: offer.Reservation.Fence,
		ExpectedRevision: offer.Task.Revision, Holder: store.ReservationHolder{Mode: "external", Ref: "attempt-9"}}
	_, err = g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-x", testProof(t))
	expectRefusal(t, "a transfer on an expired acceptance", err, "coordination_rejected")
	heldAs(t, g.alpha, task.ID, offer.Reservation)
	// Only a current fact releases it.
	fresh := fact(coord, "never_accepted", "release", task.ID, "never-y")
	fresh["offer_ref"] = "offer-9"
	g.answerFact(fresh)
	if out, err := g.s.reserve(coord, store.ReservationRelease, nextOf(task, offer, "READY"), "never-y", testProof(t)); err != nil || out.Reservation.ID != "" {
		t.Fatalf("a release on a current fact: %+v %v", out, err)
	}
}

// (b) Tasks disabled and re-enabled on the hold's project: the hold, its
// fence and history survive; reads are still served while tasks are off;
// after re-enabling, the same hold is usable by its holder.
func TestHoldSurvivesTasksDisabledAndReenabled(t *testing.T) {
	g := newReadRig(t)
	// A team hold and a personal hold on the same project.
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	teamTask := g.readyTask(t, g.alpha)
	teamHeld, _ := g.claim(t, indep, teamTask.ID, "attempt-d")
	alice := g.personal(g.aliceIdentity)
	personalTask := g.readyTask(t, g.alpha)
	personalHeld, err := g.s.reserve(alice, store.ReservationClaim, claimOf(personalTask), "p-"+uuidv7.New(), "")
	if err != nil {
		t.Fatal(err)
	}
	teamHist, personalHist := historyLen(t, g.alpha, teamTask.ID), historyLen(t, g.alpha, personalTask.ID)

	if err := g.alpha.SetMeta(store.TasksMetaKey, "off"); err != nil {
		t.Fatal(err)
	}
	heldAs(t, g.alpha, teamTask.ID, teamHeld.Reservation)
	heldAs(t, g.alpha, personalTask.ID, personalHeld.Reservation)
	// Reads are served while tasks are off: the holder's status, and
	// aicrew's read scope.
	r := g.call(t, g.tls, "GET", g.rpath(personalTask.ID, ""), g.alice, rhdr("", nil), "", true)
	var st wireStatus
	if r.status != 200 || json.Unmarshal(r.body, &st) != nil || st.State != "held" || st.ReservationID != personalHeld.Reservation.ID {
		t.Fatalf("the holder's status while tasks are off: %d %s", r.status, r.body)
	}
	if h := g.answer(t, holdOf(teamTask.ID)); h["state"] != "held" || h["reservation_id"] != teamHeld.Reservation.ID {
		t.Fatalf("the read scope while tasks are off: %v", h)
	}
	if historyLen(t, g.alpha, teamTask.ID) != teamHist || historyLen(t, g.alpha, personalTask.ID) != personalHist {
		t.Fatal("disabling tasks changed a held task's history")
	}

	if err := g.alpha.SetMeta(store.TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	heldAs(t, g.alpha, teamTask.ID, teamHeld.Reservation)
	heldAs(t, g.alpha, personalTask.ID, personalHeld.Reservation)
	// The same holds, by the same ID and fence, are usable again.
	out, err := g.s.reserve(alice, store.ReservationUpdate, nextOf(personalTask, personalHeld, "IN_PROGRESS"), "p-upd-"+uuidv7.New(), "")
	if err != nil || out.Reservation.ID != personalHeld.Reservation.ID || out.Reservation.Fence != personalHeld.Reservation.Fence+1 {
		t.Fatalf("the personal holder after re-enabling: %+v %v", out, err)
	}
	out, err = g.s.reserve(indep, store.ReservationUpdate, nextOf(teamTask, teamHeld, "IN_PROGRESS"), "t-upd-"+uuidv7.New(), "")
	if err != nil || out.Reservation.ID != teamHeld.Reservation.ID || out.Reservation.Fence != teamHeld.Reservation.Fence+1 {
		t.Fatalf("the team holder after re-enabling: %+v %v", out, err)
	}
	if historyLen(t, g.alpha, personalTask.ID) != personalHist+1 {
		t.Fatal("the update after re-enabling did not extend the kept history")
	}
}
