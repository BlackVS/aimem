package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"aimem/internal/introspect/introspecttest"
	"aimem/internal/process"
	"aimem/internal/store"
)

var testPin = process.Ref{Repo: "https://git.example/team/process.git", Commit: strings.Repeat("3f", 20), Manifest: "process/manifest.json"}

func testProof(t *testing.T) string {
	t.Helper()
	p, err := introspecttestProof()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// selectProcess stores alpha's selection as the hub's process route does.
func (g *reservationRig) selectProcess(t *testing.T, ref process.Ref) {
	t.Helper()
	ref.Ref, ref.SelectedAt, ref.SelectedBy = "main", "2026-09-28T06:00:00Z", "admin"
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.alpha.SetMeta(processMetaKey, string(b)); err != nil {
		t.Fatal(err)
	}
}

// member is Alice's verified team context in a role, from one agent and
// session; aicrew runs several agents of one user.
func (g *reservationRig) member(t *testing.T, role, agent, session string) context.Context {
	t.Helper()
	return g.team(t, g.aliceIdentity, func(m map[string]any) {
		m["role"], m["agent_id"], m["session_id"] = role, agent, session
	})
}

func pinOf(ref process.Ref) map[string]any {
	return map[string]any{"repo": ref.Repo, "commit": ref.Commit, "manifest": ref.Manifest}
}

// fact is a coordination.v1 fact naming the caller in ctx, for this task
// and request key; the kind's references are added by the caller.
func fact(ctx context.Context, kind, op, taskID, key string) map[string]any {
	tc, _ := teamContextFrom(ctx)
	return map[string]any{
		"kind": kind, "operation": op, "task_id": taskID, "request_key_digest": store.RequestKeyDigest(key),
		"member": map[string]any{"user_id": tc.UserID, "agent_id": tc.AgentID, "team_id": tc.TeamID, "role": tc.Role,
			"session_id": tc.SessionID, "generation": tc.Generation},
		"expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
}

// answerFact scripts the fake aicrew's next coordination answers.
func (g *reservationRig) answerFact(f map[string]any) {
	g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.FactReply(got.Nonce, "aicrew-example", g.hub, f))
	})
}

func (g *reservationRig) answerInactive() {
	g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
}

func offerFact(ctx context.Context, taskID, key, offerRef string) map[string]any {
	f := fact(ctx, "offer", "claim", taskID, key)
	f["offer_ref"] = offerRef
	f["intended_worker"] = map[string]any{"user_id": teamUser(ctx), "agent_id": "agent-worker"}
	f["process"] = pinOf(testPin)
	return f
}

func teamUser(ctx context.Context) string {
	tc, _ := teamContextFrom(ctx)
	return tc.UserID
}

func externalClaim(task store.Task, ref string) store.TaskReservationInput {
	return store.TaskReservationInput{TaskID: task.ID, ExpectedRevision: task.Revision, Holder: store.ReservationHolder{Mode: "external", Ref: ref}}
}

// The whole worker-held flow: the coordinator's offer, the intended worker's
// transfer, the holder's update, and the reviewing coordinator's finalize.
func TestCoordinatedReservationFlow(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	coord := g.member(t, "coordinator", "agent-coord", "sess-c")
	task := g.readyTask(t, g.alpha)
	g.answerFact(offerFact(coord, task.ID, "offer-1", "offer-7"))
	offer, err := g.s.reserve(coord, store.ReservationClaim, externalClaim(task, "offer-7"), "offer-1", testProof(t))
	if err != nil || offer.Reservation.IntendedWorker == nil || offer.Reservation.IntendedWorker.AgentID != "agent-worker" ||
		offer.Coordination == nil || offer.Coordination.Kind != "offer" || !strings.HasPrefix(offer.Coordination.ProofDigest, "p1_") {
		t.Fatalf("offer claim: %+v %v", offer, err)
	}
	// Another agent of the intended user is not the intended worker.
	other := g.member(t, "worker", "agent-2", "sess-w2")
	transfer := store.TaskReservationInput{TaskID: task.ID, ID: offer.Reservation.ID, Fence: offer.Reservation.Fence,
		ExpectedRevision: offer.Task.Revision, Holder: store.ReservationHolder{Mode: "external", Ref: "attempt-7"}}
	accepted := func(ctx context.Context, key string) map[string]any {
		f := fact(ctx, "accepted_attempt", "transfer", task.ID, key)
		f["offer_ref"], f["attempt_ref"], f["process"] = "offer-7", "attempt-7", pinOf(testPin)
		return f
	}
	g.answerFact(accepted(other, "tr-other"))
	_, err = g.s.reserve(other, store.ReservationTransfer, transfer, "tr-other", testProof(t))
	expectRefusal(t, "another agent's transfer", err, "coordination_rejected")
	worker := g.member(t, "worker", "agent-worker", "sess-w")
	g.answerFact(accepted(worker, "tr-1"))
	proof := testProof(t)
	moved, err := g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-1", proof)
	if err != nil || moved.Reservation.Holder.Ref != "attempt-7" || moved.Reservation.Binding.Role != "worker" || moved.Reservation.IntendedWorker != nil {
		t.Fatalf("transfer: %+v %v", moved, err)
	}
	// The replay rule: after aicrew settled the proof, the same request is
	// answered from its receipt without asking aicrew again.
	g.answerInactive()
	calls := g.fake.Calls()
	again, err := g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-1", proof)
	if err != nil || !again.Replayed || again.Reservation.Fence != moved.Reservation.Fence || g.fake.Calls() != calls {
		t.Fatalf("transfer replay: %+v %v (aicrew calls %d -> %d)", again, err, calls, g.fake.Calls())
	}
	// The holder updates without a fact.
	updated, err := g.s.reserve(worker, store.ReservationUpdate, nextOf(task, moved, "IN_PROGRESS"), "upd", "")
	if err != nil {
		t.Fatal(err)
	}
	// The reviewing coordinator finalizes on a fresh accepted-for-finalization
	// fact, with terminal evidence.
	review := g.member(t, "coordinator", "agent-coord", "sess-review")
	final := nextOf(task, updated, "DONE")
	finalFact := fact(review, "accepted_for_finalization", "finalize", task.ID, "fin")
	finalFact["attempt_ref"] = "attempt-7"
	finalFact["evidence_digest"] = store.EvidenceDigest([]string{"review-head-example", "human-merge-example", "post-merge-ci-example"})
	g.answerFact(finalFact)
	_, err = g.s.reserve(review, store.ReservationFinalize, final, "fin", testProof(t))
	expectRefusal(t, "DONE without evidence", err, "invalid_request")
	final.TerminalEvidence = []string{"review-head-example", "human-merge-example", "post-merge-ci-example"}
	done, err := g.s.reserve(review, store.ReservationFinalize, final, "fin", testProof(t))
	if err != nil || done.Task.State != "DONE" || done.Reservation.ID != "" || done.Coordination.Kind != "accepted_for_finalization" {
		t.Fatalf("coordinator finalize: %+v %v", done, err)
	}
	// Only the reviewing coordinator's session replays that finalize.
	later := g.member(t, "coordinator", "agent-coord", "sess-later")
	_, err = g.s.reserve(later, store.ReservationFinalize, final, "fin", testProof(t))
	expectRefusal(t, "another session's finalize replay", err, "reservation_conflict")
}

// Every way a fact can fail to describe this request is refused, and
// nothing is applied.
func TestCoordinatedFactBinding(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	coord := g.member(t, "coordinator", "agent-coord", "sess-c")
	task := g.readyTask(t, g.alpha)
	in := externalClaim(task, "offer-7")
	key := "offer-bind"
	for name, c := range map[string]struct {
		edit func(f map[string]any)
		in   store.TaskReservationInput
		want string
	}{
		"another kind": {func(f map[string]any) {
			f["kind"], f["member"].(map[string]any)["role"] = "independent_claim", "coordinator"
			delete(f, "offer_ref")
			delete(f, "intended_worker")
			f["attempt_ref"] = "offer-7"
		}, in, "coordination_rejected"},
		"another task":       {func(f map[string]any) { f["task_id"] = g.betaTk }, in, "coordination_rejected"},
		"another key":        {func(f map[string]any) { f["request_key_digest"] = store.RequestKeyDigest("other") }, in, "coordination_rejected"},
		"another session":    {func(f map[string]any) { f["member"].(map[string]any)["session_id"] = "sess-x" }, in, "coordination_rejected"},
		"another agent":      {func(f map[string]any) { f["member"].(map[string]any)["agent_id"] = "agent-x" }, in, "coordination_rejected"},
		"another generation": {func(f map[string]any) { f["member"].(map[string]any)["generation"] = "5" }, in, "coordination_rejected"},
		"another holder":     {nil, externalClaim(task, "offer-8"), "coordination_rejected"},
		"a standalone hold":  {nil, store.TaskReservationInput{TaskID: task.ID, ExpectedRevision: task.Revision, Holder: store.ReservationHolder{Mode: "standalone", Ref: "offer-7"}}, "coordination_rejected"},
		"another commit": {func(f map[string]any) {
			p := testPin
			p.Commit = strings.Repeat("9b", 20)
			f["process"] = pinOf(p)
		}, in, "process_mismatch"},
		"a malformed pin":  {func(f map[string]any) { f["process"].(map[string]any)["commit"] = "3f2a" }, in, "context_unavailable"},
		"a pin with a ref": {func(f map[string]any) { f["process"].(map[string]any)["ref"] = "main" }, in, "context_unavailable"},
		"no pin":           {func(f map[string]any) { delete(f, "process") }, in, "context_unavailable"},
		"expired": {func(f map[string]any) {
			f["expires_at"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
		}, in, "coordination_rejected"},
	} {
		f := offerFact(coord, task.ID, key, "offer-7")
		if c.edit != nil {
			c.edit(f)
		}
		g.answerFact(f)
		_, err := g.s.reserve(coord, store.ReservationClaim, c.in, key, testProof(t))
		expectRefusal(t, name, err, c.want)
	}
	g.answerInactive()
	_, err := g.s.reserve(coord, store.ReservationClaim, in, key, testProof(t))
	expectRefusal(t, "inactive", err, "coordination_rejected")
	_, err = g.s.reserve(coord, store.ReservationClaim, in, key, "acp1_short")
	expectRefusal(t, "a malformed proof", err, "coordination_rejected")
	_, err = g.s.reserve(coord, store.ReservationClaim, in, key, "")
	expectRefusal(t, "no proof", err, "invalid_request")
	_, err = g.s.reserve(g.personal(g.aliceIdentity), store.ReservationClaim, claimOf(task), key, testProof(t))
	expectRefusal(t, "a personal claim with a proof", err, "invalid_request")
	worker := g.member(t, "worker", "agent-worker", "sess-w")
	_, err = g.s.reserve(worker, store.ReservationClaim, in, key, testProof(t))
	expectRefusal(t, "a worker's claim", err, "role_forbidden")
	// No selection: nothing to match.
	if err := g.alpha.SetMeta(processMetaKey, ""); err != nil {
		t.Fatal(err)
	}
	g.answerFact(offerFact(coord, task.ID, key, "offer-7"))
	_, err = g.s.reserve(coord, store.ReservationClaim, in, key, testProof(t))
	expectRefusal(t, "no selection", err, "process_mismatch")
	if hold, _ := g.alpha.GetTaskReservation(task.ID); hold.ID != "" {
		t.Fatalf("a refused claim left a hold: %+v", hold)
	}
}

// An unaccepted offer is released by a successor coordinator; an attempt
// by its holder on a stop; an independent worker claims its own attempt.
func TestCoordinatedReleasesAndIndependentClaim(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	coord := g.member(t, "coordinator", "agent-coord", "sess-c")
	task := g.readyTask(t, g.alpha)
	g.answerFact(offerFact(coord, task.ID, "offer-r", "offer-9"))
	offer, err := g.s.reserve(coord, store.ReservationClaim, externalClaim(task, "offer-9"), "offer-r", testProof(t))
	if err != nil {
		t.Fatal(err)
	}
	successor := g.member(t, "coordinator", "agent-successor", "sess-s")
	never := fact(successor, "never_accepted", "release", task.ID, "never")
	never["offer_ref"] = "offer-9"
	g.answerFact(never)
	released, err := g.s.reserve(successor, store.ReservationRelease, nextOf(task, offer, "READY"), "never", testProof(t))
	if err != nil || released.Reservation.ID != "" || released.Task.State != "READY" {
		t.Fatalf("successor release: %+v %v", released, err)
	}
	// An independent worker claims its own attempt, pinned.
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	claimFact := fact(indep, "independent_claim", "claim", task.ID, "indep")
	claimFact["attempt_ref"], claimFact["process"] = "attempt-i", pinOf(testPin)
	g.answerFact(claimFact)
	claimTask, _ := g.alpha.GetTask(task.ID)
	held, err := g.s.reserve(indep, store.ReservationClaim, externalClaim(claimTask, "attempt-i"), "indep", testProof(t))
	if err != nil || held.Reservation.Holder.Ref != "attempt-i" || held.Reservation.Binding.Role != "independent" {
		t.Fatalf("independent claim: %+v %v", held, err)
	}
	// Its holder releases it on a stop fact; nobody else can.
	stopFor := func(ctx context.Context, key string) map[string]any {
		f := fact(ctx, "stopped", "release", task.ID, key)
		f["attempt_ref"] = "attempt-i"
		return f
	}
	intruder := g.member(t, "worker", "agent-worker", "sess-w")
	g.answerFact(stopFor(intruder, "stop-x"))
	_, err = g.s.reserve(intruder, store.ReservationRelease, nextOf(task, held, "READY"), "stop-x", testProof(t))
	expectRefusal(t, "a stop by another member", err, "reservation_conflict")
	g.answerFact(stopFor(indep, "stop"))
	if out, err := g.s.reserve(indep, store.ReservationRelease, nextOf(task, held, "READY"), "stop", testProof(t)); err != nil || out.Reservation.ID != "" {
		t.Fatalf("holder stop release: %+v %v", out, err)
	}
}

// The pin is compared in the committing transaction: a selection changed
// after aicrew answered refuses the transition, and so does a transfer after
// the selection moved on since the offer.
func TestCoordinatedPinAndAnswerAge(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	coord := g.member(t, "coordinator", "agent-coord", "sess-c")
	task := g.readyTask(t, g.alpha)
	newer := testPin
	newer.Commit = strings.Repeat("9b", 20)
	g.answerFact(offerFact(coord, task.ID, "offer-p", "offer-p"))
	beforeReservationRecheck = func() { beforeReservationRecheck = nil; g.selectProcess(t, newer) }
	t.Cleanup(func() { beforeReservationRecheck = nil })
	_, err := g.s.reserve(coord, store.ReservationClaim, externalClaim(task, "offer-p"), "offer-p", testProof(t))
	expectRefusal(t, "selection changed before commit", err, "process_mismatch")
	// With the selection back, the offer commits; a change before the
	// acceptance refuses the transfer.
	g.selectProcess(t, testPin)
	offer, err := g.s.reserve(coord, store.ReservationClaim, externalClaim(task, "offer-p"), "offer-p2", testProof(t))
	if err == nil {
		t.Fatal("a fact for another key committed")
	}
	g.answerFact(offerFact(coord, task.ID, "offer-p2", "offer-p"))
	if offer, err = g.s.reserve(coord, store.ReservationClaim, externalClaim(task, "offer-p"), "offer-p2", testProof(t)); err != nil {
		t.Fatal(err)
	}
	g.selectProcess(t, newer)
	worker := g.member(t, "worker", "agent-worker", "sess-w")
	acc := fact(worker, "accepted_attempt", "transfer", task.ID, "tr-p")
	acc["offer_ref"], acc["attempt_ref"], acc["process"] = "offer-p", "attempt-p", pinOf(testPin)
	g.answerFact(acc)
	transfer := store.TaskReservationInput{TaskID: task.ID, ID: offer.Reservation.ID, Fence: offer.Reservation.Fence,
		ExpectedRevision: offer.Task.Revision, Holder: store.ReservationHolder{Mode: "external", Ref: "attempt-p"}}
	_, err = g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-p", testProof(t))
	expectRefusal(t, "transfer after the selection moved", err, "process_mismatch")
	// An answer older than the bound at commit is not used.
	g.selectProcess(t, testPin)
	old := coordinationAnswerMaxAge
	coordinationAnswerMaxAge = time.Millisecond
	t.Cleanup(func() { coordinationAnswerMaxAge = old })
	beforeReservationRecheck = func() { time.Sleep(5 * time.Millisecond) }
	_, err = g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-p2", testProof(t))
	if err == nil {
		t.Fatal("a fact for another key committed")
	}
	acc["request_key_digest"] = store.RequestKeyDigest("tr-p2")
	g.answerFact(acc)
	_, err = g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-p2", testProof(t))
	expectRefusal(t, "an answer too old at commit", err, "context_unavailable")
	beforeReservationRecheck = nil
	coordinationAnswerMaxAge = old
	// Revocation between authorization and commit refuses and keeps the hold.
	beforeReservationRecheck = func() {
		beforeReservationRecheck = nil
		if err := g.db.SetTeamGrant("admin", g.alphaInstance, g.profile.ID, false); err != nil {
			t.Error(err)
		}
	}
	acc["request_key_digest"] = store.RequestKeyDigest("tr-p3")
	g.answerFact(acc)
	_, err = g.s.reserve(worker, store.ReservationTransfer, transfer, "tr-p3", testProof(t))
	expectRefusal(t, "a grant revoked before commit", err, "grant_denied")
	g.held(t, task.ID, offer.Reservation)
}

// seq209: a recovery on stop evidence closes only the hold it verified. A
// transfer that lands while aicrew is being asked keeps the reservation ID
// and moves the fence to the one the request names; the recovery must
// refuse rather than close the new attempt.
func TestRecoveryRefusesAHoldTransferredDuringVerification(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	coord := g.member(t, "coordinator", "agent-coord", "sess-c")
	task := g.readyTask(t, g.alpha)
	g.answerFact(offerFact(coord, task.ID, "offer-race", "offer-race"))
	offer, err := g.s.reserve(coord, store.ReservationClaim, externalClaim(task, "offer-race"), "offer-race", testProof(t))
	if err != nil {
		t.Fatal(err)
	}
	worker := g.member(t, "worker", "agent-worker", "sess-w")
	wc, err := reservationCallerFrom(worker)
	if err != nil {
		t.Fatal(err)
	}
	key := "recover-race"
	var moved store.TaskReservationOutcome
	g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		// The transfer commits while the recovery waits for this answer.
		pin := store.ProcessPin{Repo: testPin.Repo, Commit: testPin.Commit, Manifest: testPin.Manifest}
		transfer := store.TaskReservationInput{TaskID: task.ID, ID: offer.Reservation.ID, Fence: offer.Reservation.Fence,
			ExpectedRevision: offer.Task.Revision, Holder: store.ReservationHolder{Mode: "external", Ref: "attempt-race"}}
		var terr error
		moved, terr = g.alpha.ApplyTaskReservation(store.ReservationTransfer, transfer, wc.actor, wc.binding, "tr-race",
			&store.ReservationCoordination{CoordinationRecord: store.CoordinationRecord{Kind: "accepted_attempt", ProofDigest: "p1_" + strings.Repeat("C", 43)},
				WorkRef: "offer-race", Worker: &store.ReservationWorker{UserID: wc.binding.UserID, AgentID: "agent-worker"}, Process: &pin},
			func() error { return nil })
		if terr != nil {
			t.Error(terr)
		}
		stop := fact(coord, "stopped", "release", task.ID, key)
		stop["attempt_ref"] = "offer-race"
		introspecttest.WriteJSON(w, introspecttest.FactReply(got.Nonce, "aicrew-example", g.hub, stop))
	})
	body, _ := json.Marshal(map[string]any{
		"reservation_id": offer.Reservation.ID, "fence": strconvFence(offer.Reservation.Fence + 1),
		"expected_revision": offer.Task.Revision, "reason": "the coordinator is gone",
		"content":  map[string]any{"title": task.Title, "state": "READY"},
		"evidence": map[string]any{"kind": "stop_evidence", "proof": testProof(t)},
	})
	rg := &recoveryRig{reservationRig: g, task: task, hold: offer}
	r := rg.recover(t, "release", g.env, key, string(body))
	if r.status != http.StatusConflict || recoveryCode(t, r) != "stale_fence" {
		t.Fatalf("a recovery closed a hold transferred during verification: %d %s", r.status, r.body)
	}
	g.held(t, task.ID, moved.Reservation)
}

func strconvFence(f int64) string { return strconv.FormatInt(f, 10) }
