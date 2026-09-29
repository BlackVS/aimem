package server

// Task 01a0eda3: every reservation transition, operator recovery included,
// needs the owner project's tasks enabled, read in the committing
// transaction. While tasks are off each one is refused with tasks_disabled
// and applies nothing; reads and replays of committed receipts are still
// answered, and the holds are usable again once tasks are back on.

import (
	"encoding/json"
	"net/http"
	"testing"

	"aimem/internal/introspect/introspecttest"
	"aimem/internal/store"
)

func (g *reservationRig) setTasks(t *testing.T, value string) {
	t.Helper()
	if err := g.alpha.SetMeta(store.TasksMetaKey, value); err != nil {
		t.Fatal(err)
	}
}

// wantTasksDisabled checks the whole envelope: 403, final, and the one next
// action the code names.
func wantTasksDisabled(t *testing.T, what string, r identityResp, mode string) {
	t.Helper()
	wantRefusal(t, what, r, http.StatusForbidden, "tasks_disabled", mode)
	e := decodeRefusal(t, r)
	if e.Retryable || e.NextAction != reservationRefusals["tasks_disabled"].next {
		t.Fatalf("%s: %s", what, r.body)
	}
}

// unheld asserts the task has never been held.
func unheld(t *testing.T, db *store.DB, taskID string) {
	t.Helper()
	if got, err := db.GetTaskReservation(taskID); err != nil || got.ID != "" || got.Fence != 0 {
		t.Fatalf("a refused claim left a hold: %+v %v", got, err)
	}
}

// A personal holder, through the real routes: claim, update, release and
// finalize are refused and apply nothing; the status and receipt reads and a
// replay of a committed update are answered; a claim behind an unresolved
// dependency names the disabled tasks, not the dependency.
func TestTasksDisabledRefusesPersonalTransitions(t *testing.T) {
	g := newReservationRig(t)
	task, free, dep := g.readyTask(t, g.alpha), g.readyTask(t, g.alpha), g.readyTask(t, g.alpha)
	blocked, err := g.alpha.CreateTask(store.TaskContent{Title: "blocked", State: "READY", Dependencies: []string{dep.ID}},
		store.TaskActor{Kind: "admin", Name: "admin"}, "blocked")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, taskID, suffix, key, b string) identityResp {
		return g.call(t, g.tls, method, g.rpath(taskID, suffix), g.alice, rhdr(key, nil), b, true)
	}
	claim := func(tk store.Task) string {
		return body(t, map[string]any{"expected_revision": tk.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "own"}})
	}
	o := decodeOutcome(t, call("POST", task.ID, "/claim", "c1", claim(task)))
	next := func(fence string, rev int64, state string, extra map[string]any) string {
		m := map[string]any{"expected_revision": rev, "reservation_id": o.Reservation.ID, "fence": fence,
			"content": map[string]any{"title": task.Title, "state": state}}
		for k, v := range extra {
			m[k] = v
		}
		return body(t, m)
	}
	u1 := next("1", o.TaskRevision, "IN_PROGRESS", nil)
	upd := decodeOutcome(t, call("POST", task.ID, "/update", "u1", u1))
	before, _ := g.alpha.GetTaskReservation(task.ID)
	hist := historyLen(t, g.alpha, task.ID)

	g.setTasks(t, "off")
	wantTasksDisabled(t, "a claim", call("POST", free.ID, "/claim", "c2", claim(free)), "personal")
	wantTasksDisabled(t, "a claim behind an unresolved dependency", call("POST", blocked.ID, "/claim", "c3", claim(blocked)), "personal")
	wantTasksDisabled(t, "an update", call("POST", task.ID, "/update", "u2", next("2", upd.TaskRevision, "BLOCKED", nil)), "personal")
	wantTasksDisabled(t, "a release", call("POST", task.ID, "/release", "r1", next("2", upd.TaskRevision, "READY", map[string]any{"reason": "handing it back"})), "personal")
	wantTasksDisabled(t, "a finalize", call("POST", task.ID, "/finalize", "f1", next("2", upd.TaskRevision, "DONE",
		map[string]any{"reason": "delivered", "terminal_evidence": []string{"review-head", "human-merge"}})), "personal")
	g.held(t, task.ID, before)
	unheld(t, g.alpha, free.ID)
	unheld(t, g.alpha, blocked.ID)
	if n := historyLen(t, g.alpha, task.ID); n != hist {
		t.Fatalf("refused transitions wrote history: %d, then %d", hist, n)
	}
	// Reads, and a replay of what committed before, are answered.
	var st wireStatus
	if r := call("GET", task.ID, "", "", ""); r.status != 200 || json.Unmarshal(r.body, &st) != nil || st.State != "held" || st.Fence != "2" {
		t.Fatalf("status while tasks are off: %d %s", r.status, r.body)
	}
	var rs wireReceiptStatus
	if r := call("GET", task.ID, "/receipts/update/u1", "", ""); r.status != 200 || json.Unmarshal(r.body, &rs) != nil || rs.State != "committed" {
		t.Fatalf("receipt while tasks are off: %d %s", r.status, r.body)
	}
	if again := decodeOutcome(t, call("POST", task.ID, "/update", "u1", u1)); !again.Receipt.Replayed || again.Receipt.ID != upd.Receipt.ID {
		t.Fatalf("a replay while tasks are off: %+v", again)
	}

	// Back on: the same hold moves on, and the dependency answers for itself.
	g.setTasks(t, "on")
	if o := decodeOutcome(t, call("POST", task.ID, "/update", "u3", next("2", upd.TaskRevision, "BLOCKED", nil))); o.Reservation.Fence != "3" {
		t.Fatalf("the hold after re-enabling: %+v", o)
	}
	wantRefusal(t, "the blocked claim after re-enabling", call("POST", blocked.ID, "/claim", "c4", claim(blocked)), http.StatusNotFound, "dependency_unresolved", "personal")
}

// Team members, each with the coordination fact its step needs: the
// independent claim, the worker's transfer, the coordinator's release and
// finalize, and the holder's update are all refused and apply nothing; the
// read scope still answers; the holds are usable once tasks are back on.
func TestTasksDisabledRefusesTeamTransitions(t *testing.T) {
	g := newReadRig(t)
	coord := g.member(t, "coordinator", "agent-coord", "sess-c")
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	offered, held, free := g.readyTask(t, g.alpha), g.readyTask(t, g.alpha), g.readyTask(t, g.alpha)
	g.answerFact(offerFact(coord, offered.ID, "offer-1", "offer-7"))
	offer, err := g.s.reserve(coord, store.ReservationClaim, externalClaim(offered, "offer-7"), "offer-1", testProof(t))
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := g.claim(t, indep, held.ID, "attempt-i")
	offerHist, heldHist := historyLen(t, g.alpha, offered.ID), historyLen(t, g.alpha, held.ID)

	g.setTasks(t, "off")
	f := fact(indep, "independent_claim", "claim", free.ID, "c-off")
	f["attempt_ref"], f["process"] = "attempt-f", pinOf(testPin)
	g.answerFact(f)
	_, err = g.s.reserve(indep, store.ReservationClaim, externalClaim(free, "attempt-f"), "c-off", testProof(t))
	expectRefusal(t, "an independent claim", err, "tasks_disabled")

	worker := g.member(t, "worker", "agent-worker", "sess-w")
	f = fact(worker, "accepted_attempt", "transfer", offered.ID, "tr-off")
	f["offer_ref"], f["attempt_ref"], f["process"] = "offer-7", "attempt-7", pinOf(testPin)
	g.answerFact(f)
	transfer := store.TaskReservationInput{TaskID: offered.ID, ID: offer.Reservation.ID, Fence: offer.Reservation.Fence,
		ExpectedRevision: offer.Task.Revision, Holder: store.ReservationHolder{Mode: "external", Ref: "attempt-7"}}
	_, err = g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-off", testProof(t))
	expectRefusal(t, "the worker's transfer", err, "tasks_disabled")

	f = fact(coord, "never_accepted", "release", offered.ID, "rel-off")
	f["offer_ref"] = "offer-7"
	g.answerFact(f)
	_, err = g.s.reserve(coord, store.ReservationRelease, nextOf(offered, offer, "READY"), "rel-off", testProof(t))
	expectRefusal(t, "the coordinator's release", err, "tasks_disabled")

	_, err = g.s.reserve(indep, store.ReservationUpdate, nextOf(held, claimed, "IN_PROGRESS"), "upd-off", "")
	expectRefusal(t, "the holder's update", err, "tasks_disabled")

	review := g.member(t, "coordinator", "agent-coord", "sess-review")
	evidence := []string{"review-head-example", "human-merge-example"}
	final := nextOf(held, claimed, "DONE")
	final.TerminalEvidence = evidence
	f = fact(review, "accepted_for_finalization", "finalize", held.ID, "fin-off")
	f["attempt_ref"], f["evidence_digest"] = "attempt-i", store.EvidenceDigest(evidence)
	g.answerFact(f)
	_, err = g.s.reserve(review, store.ReservationFinalize, final, "fin-off", testProof(t))
	expectRefusal(t, "the coordinator's finalize", err, "tasks_disabled")

	// The team envelope through the route.
	upd := body(t, map[string]any{"expected_revision": claimed.Task.Revision, "reservation_id": claimed.Reservation.ID,
		"fence": strconvFence(claimed.Reservation.Fence), "content": map[string]any{"title": held.Title, "state": "IN_PROGRESS"}})
	r := g.call(t, g.tls, "POST", g.rpath(held.ID, "/update"), g.alice, rhdr("upd-route", g.teamAs(t, "independent", "agent-indep", "sess-i")), upd, true)
	wantTasksDisabled(t, "the holder's update through the route", r, "team")

	heldAs(t, g.alpha, offered.ID, offer.Reservation)
	heldAs(t, g.alpha, held.ID, claimed.Reservation)
	unheld(t, g.alpha, free.ID)
	if historyLen(t, g.alpha, offered.ID) != offerHist || historyLen(t, g.alpha, held.ID) != heldHist {
		t.Fatal("refused transitions wrote history")
	}
	if h := g.answer(t, holdOf(held.ID)); h["state"] != "held" || h["reservation_id"] != claimed.Reservation.ID {
		t.Fatalf("the read scope while tasks are off: %v", h)
	}

	g.setTasks(t, "on")
	out, err := g.s.reserve(indep, store.ReservationUpdate, nextOf(held, claimed, "IN_PROGRESS"), "upd-on", "")
	if err != nil || out.Reservation.ID != claimed.Reservation.ID || out.Reservation.Fence != claimed.Reservation.Fence+1 {
		t.Fatalf("the holder after re-enabling: %+v %v", out, err)
	}
}

// Operator recovery is no exception: release and cancel are refused and
// close nothing; the recovery reader still answers.
func TestTasksDisabledRefusesRecovery(t *testing.T) {
	g := newRecoveryRig(t)
	g.setTasks(t, "off")
	for _, c := range []struct{ op, state string }{{"release", "READY"}, {"cancel", "CANCELLED"}} {
		r := g.recover(t, c.op, g.env, "rec-"+c.op, g.body(c.state, attestationEvidence()))
		if r.status != http.StatusForbidden || recoveryCode(t, r) != "tasks_disabled" {
			t.Fatalf("recovery %s while tasks are off: %d %s", c.op, r.status, r.body)
		}
	}
	g.held(t, g.task.ID, g.hold.Reservation)
	r := g.call(t, g.tls, "GET", "/v1/admin/reservations/"+g.task.ID+"/recovery", g.env, nil, "", true)
	var hold recoveryHold
	if r.status != http.StatusOK || json.Unmarshal(r.body, &hold) != nil || hold.State != "held" || hold.ReservationID != g.hold.Reservation.ID {
		t.Fatalf("the recovery reader while tasks are off: %d %s", r.status, r.body)
	}
	g.setTasks(t, "on")
	if r := g.recover(t, "release", g.env, "rec-release", g.body("READY", attestationEvidence())); r.status != http.StatusOK {
		t.Fatalf("recovery after re-enabling: %d %s", r.status, r.body)
	}
}

// Tasks switched off after the request was authorized and its coordination
// fact verified, but before the ledger commits: the check in the committing
// transaction still refuses it.
func TestTasksDisabledWhileTheClaimIsVerified(t *testing.T) {
	g := newReadRig(t)
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	task := g.readyTask(t, g.alpha)
	f := fact(indep, "independent_claim", "claim", task.ID, "c-race")
	f["attempt_ref"], f["process"] = "attempt-r", pinOf(testPin)
	g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		if err := g.alpha.SetMeta(store.TasksMetaKey, "off"); err != nil {
			t.Error(err)
		}
		introspecttest.WriteJSON(w, introspecttest.FactReply(got.Nonce, "aicrew-example", g.hub, f))
	})
	_, err := g.s.reserve(indep, store.ReservationClaim, externalClaim(task, "attempt-r"), "c-race", testProof(t))
	expectRefusal(t, "a claim whose project was disabled during verification", err, "tasks_disabled")
	unheld(t, g.alpha, task.ID)
}

// The code is part of reservation.v1: the fixture maps it to the hub's
// status and carries the hub's next action.
func TestTasksDisabledIsInTheV1Contract(t *testing.T) {
	var spec struct {
		RefusalStatus map[string]json.RawMessage `json:"x-refusal-status"`
	}
	readReservationFixture(t, "openapi-proposal.json", &spec)
	var examples struct {
		Refusals []struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
			Retryable  bool   `json:"retryable"`
			NextAction string `json:"next_action"`
		} `json:"refusals"`
	}
	readReservationFixture(t, "examples.json", &examples)
	hub := reservationRefusals["tasks_disabled"]
	if string(spec.RefusalStatus["tasks_disabled"]) != "403" || hub.status != http.StatusForbidden {
		t.Fatalf("status: fixture %s, hub %d", spec.RefusalStatus["tasks_disabled"], hub.status)
	}
	for _, ex := range examples.Refusals {
		if ex.Code == "tasks_disabled" {
			if ex.HTTPStatus != 403 || ex.Retryable || ex.NextAction != hub.next {
				t.Fatalf("the example disagrees with the hub: %+v", ex)
			}
			return
		}
	}
	t.Fatal("reservation.v1 has no tasks_disabled example")
}
