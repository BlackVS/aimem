package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/introspect/introspecttest"
	"aimem/internal/store"
)

func (g *reservationRig) rpath(taskID, suffix string) string {
	return "/v1/projects/alpha/tasks/" + taskID + "/reservation" + suffix
}

// rhdr is a member request's headers: the version, the request key and, in
// team mode, a freshly verified context handle.
func rhdr(key string, team map[string]string) map[string]string {
	h := map[string]string{reservationVersionHeader: "1"}
	if key != "" {
		h["Idempotency-Key"] = key
	}
	for k, v := range team {
		h[k] = v
	}
	return h
}

// teamAs verifies Alice's next team request in a role, agent and session,
// and returns its header.
func (g *reservationRig) teamAs(t *testing.T, role, agent, session string) map[string]string {
	t.Helper()
	g.answerActive(func(m map[string]any) {
		m["role"], m["agent_id"], m["session_id"] = role, agent, session
	})
	h := validHandle(t)
	g.secrets = append(g.secrets, h)
	return teamHeader(h)
}

// factAs is a coordination fact naming Alice in a role, agent and session,
// as the fake aicrew verified her context.
func (g *reservationRig) factAs(role, agent, session, kind, op, taskID, key string) map[string]any {
	return map[string]any{
		"kind": kind, "operation": op, "task_id": taskID, "request_key_digest": store.RequestKeyDigest(key),
		"member": map[string]any{"user_id": g.aliceID, "agent_id": agent, "team_id": "team-1", "role": role,
			"session_id": session, "generation": "4"},
		"expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
}

// body marshals a request body; the values are plain maps, so it cannot
// fail (goroutines call it without a *testing.T).
func body(_ *testing.T, v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeOutcome(t *testing.T, r identityResp) wireOutcome {
	t.Helper()
	var o wireOutcome
	if r.status != 200 || json.Unmarshal(r.body, &o) != nil {
		t.Fatalf("want an outcome: %d %s", r.status, r.body)
	}
	return o
}

// wantRefusal checks the reservation envelope: code, HTTP status, a next
// action, a correlation ID and, when given, the active mode.
func wantRefusal(t *testing.T, what string, r identityResp, status int, code, mode string) {
	t.Helper()
	var e identityRefusalBody
	if r.status != status || json.Unmarshal(r.body, &e) != nil || e.Code != code || e.NextAction == "" || e.CorrelationID == "" || e.ActiveMode != mode {
		t.Fatalf("%s: got %d %s, want %d %s (mode %q)", what, r.status, r.body, status, code, mode)
	}
}

// The personal lifecycle through the real routes: the wire's version, key
// and path checks, the outcome shape, replay, reconciliation and the
// refusals a consumer sees.
func TestReservationRoutesPersonalLifecycle(t *testing.T) {
	g := newReservationRig(t)
	task := g.readyTask(t, g.alpha)
	claim := body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "own-1"}})
	call := func(method, suffix, key, b string) identityResp {
		return g.call(t, g.tls, method, g.rpath(task.ID, suffix), g.alice, rhdr(key, nil), b, true)
	}
	// Gate and preamble refusals use the envelope, the reads included.
	r := g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, map[string]string{"Idempotency-Key": "c1"}, claim, true)
	wantRefusal(t, "no version", r, 400, "unsupported_version", "")
	for _, suffix := range []string{"", "/receipts/claim/c1"} {
		wantRefusal(t, "no credential on "+suffix, g.call(t, g.tls, "GET", g.rpath(task.ID, suffix), "", rhdr("", nil), "", true), 401, "invalid_credential", "")
		wantRefusal(t, "unknown credential on "+suffix, g.call(t, g.tls, "GET", g.rpath(task.ID, suffix), "aimem_no_such_token", rhdr("", nil), "", true), 401, "invalid_credential", "")
	}
	wantRefusal(t, "another project", g.call(t, g.tls, "POST", "/v1/projects/beta/tasks/"+task.ID+"/reservation/claim", g.alice, rhdr("c1", nil), claim, true),
		404, "task_unavailable", "personal")
	wantRefusal(t, "no key", call("POST", "/claim", "", claim), 400, "invalid_request", "")
	wantRefusal(t, "an actor field", call("POST", "/claim", "c1", strings.Replace(claim, "{", `{"actor_id":"someone",`, 1)), 400, "invalid_request", "")
	wantRefusal(t, "the admin", g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.env, rhdr("c1", nil), claim, true), 403, "grant_denied", "personal")
	// An authenticated token outside its scope is refused by the authorizer,
	// and the envelope still shows no active mode.
	beta, err := g.s.reg.Open("beta")
	if err != nil {
		t.Fatal(err)
	}
	bt := g.readyTask(t, beta)
	wantRefusal(t, "a token out of its scope", g.call(t, g.tls, "POST", "/v1/projects/beta/tasks/"+bt.ID+"/reservation/claim", g.project,
		rhdr("c-out", nil), body(t, map[string]any{"expected_revision": bt.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "x"}}), true),
		401, "invalid_credential", "")

	o := decodeOutcome(t, call("POST", "/claim", "c1", claim))
	if o.Receipt.State != "committed" || o.Receipt.Replayed || o.Receipt.ActorID != g.aliceID || o.Receipt.VerifiedMode != "personal" ||
		!strings.HasPrefix(o.Receipt.ID, "rcpt_") || !o.Reservation.Active || o.Reservation.Fence != "1" || o.Reservation.OwnWorkRef != "own-1" ||
		o.Reservation.HolderMode != "standalone" || o.TaskRevision != task.Revision {
		t.Fatalf("claim outcome: %+v", o)
	}
	again := decodeOutcome(t, call("POST", "/claim", "c1", claim))
	if !again.Receipt.Replayed || again.Receipt.ID != o.Receipt.ID {
		t.Fatalf("identical replay: %+v", again)
	}
	changed := strings.Replace(claim, "own-1", "own-2", 1)
	wantRefusal(t, "changed input under the key", call("POST", "/claim", "c1", changed), 409, "idempotency_conflict", "personal")
	// A generic task write cannot touch the held task.
	if r := g.call(t, g.tls, "PUT", "/v1/tasks/"+task.ID, g.alice, map[string]string{"Idempotency-Key": "put"},
		`{"title":"bypass","state":"IN_PROGRESS","expected_revision":`+itoa(task.Revision)+`}`, true); r.status == 200 {
		t.Fatalf("a generic write changed a held task: %s", r.body)
	}
	var st wireStatus
	if r := call("GET", "", "", ""); r.status != 200 || json.Unmarshal(r.body, &st) != nil || st.State != "held" || st.Fence != "1" ||
		st.ReservationID != o.Reservation.ID || st.OwnWorkRef != "own-1" {
		t.Fatalf("status: %d %s", r.status, r.body)
	}
	next := func(fence string, rev int64, state string, extra map[string]any) string {
		m := map[string]any{"expected_revision": rev, "reservation_id": o.Reservation.ID, "fence": fence,
			"content": map[string]any{"title": task.Title, "state": state}}
		for k, v := range extra {
			m[k] = v
		}
		return body(t, m)
	}
	for name, b := range map[string]string{
		"no content":          body(t, map[string]any{"expected_revision": task.Revision, "reservation_id": o.Reservation.ID, "fence": "1"}),
		"a non-decimal fence": next("01", task.Revision, "IN_PROGRESS", nil),
		"an unknown intent":   next("1", task.Revision, "IN_PROGRESS", map[string]any{"intent": "ship"}),
		"a holder":            next("1", task.Revision, "IN_PROGRESS", map[string]any{"holder": map[string]any{"mode": "standalone", "work_ref": "x"}}),
	} {
		wantRefusal(t, "update with "+name, call("POST", "/update", "u-"+name, b), 400, "invalid_request", "")
	}
	upd := decodeOutcome(t, call("POST", "/update", "u1", next("1", task.Revision, "IN_PROGRESS", map[string]any{"intent": "submit"})))
	if upd.Reservation.Fence != "2" || upd.TaskRevision != task.Revision+1 {
		t.Fatalf("update: %+v", upd)
	}
	wantRefusal(t, "a late write on the old fence", call("POST", "/update", "u2", next("1", upd.TaskRevision, "IN_PROGRESS", nil)),
		409, "stale_fence", "personal")
	// Reconciliation by key: committed with the recorded outcome, or not.
	var rs wireReceiptStatus
	if r := call("GET", "/receipts/update/u1", "", ""); r.status != 200 || json.Unmarshal(r.body, &rs) != nil || rs.State != "committed" ||
		rs.Outcome == nil || rs.Outcome.Reservation.Fence != "2" || rs.Outcome.Receipt.ID != upd.Receipt.ID {
		t.Fatalf("receipt: %d %s", r.status, r.body)
	}
	rs = wireReceiptStatus{}
	if r := call("GET", "/receipts/update/never-sent", "", ""); r.status != 200 || json.Unmarshal(r.body, &rs) != nil || rs.State != "not_committed" || rs.Outcome != nil {
		t.Fatalf("unknown key: %d %s", r.status, r.body)
	}
	wantRefusal(t, "an unknown operation", call("GET", "/receipts/steal/u1", "", ""), 400, "invalid_request", "")
	// DONE needs terminal evidence; with it the hold closes.
	fin := next("2", upd.TaskRevision, "DONE", map[string]any{"reason": "delivered"})
	wantRefusal(t, "DONE without evidence", call("POST", "/finalize", "f1", fin), 400, "invalid_request", "")
	done := decodeOutcome(t, call("POST", "/finalize", "f1", next("2", upd.TaskRevision, "DONE",
		map[string]any{"reason": "delivered", "terminal_evidence": []string{"review-head", "human-merge"}})))
	if done.Reservation.Active || done.Reservation.ID != "" || done.Reservation.OwnWorkRef != "" || done.Reservation.Fence != "3" {
		t.Fatalf("finalize: %+v", done)
	}
	if r := call("GET", "", "", ""); r.status != 200 || json.Unmarshal(r.body, &st) != nil || st.State != "none" {
		t.Fatalf("status after finalize: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

func itoa(n int64) string { return strings.TrimSpace(body(nil, n)) }

// The team flow through the real routes: the coordinator's offer, the
// intended worker's transfer and its replay, a stale write, the process
// pin, isolation from personal mode, revocation, and no proof anywhere.
func TestReservationRoutesTeamFlow(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	task := g.readyTask(t, g.alpha)
	proofs := map[string]string{}
	proof := func(name string) string {
		p := testProof(t)
		proofs[name] = p
		g.secrets = append(g.secrets, p)
		return p
	}
	team := func(role, agent, session, method, suffix, key, b string) identityResp {
		return g.call(t, g.tls, method, g.rpath(task.ID, suffix), g.alice, rhdr(key, g.teamAs(t, role, agent, session)), b, true)
	}
	offer := g.factAs("coordinator", "agent-coord", "sess-c", "offer", "claim", task.ID, "offer-1")
	offer["offer_ref"], offer["process"] = "offer-7", pinOf(testPin)
	offer["intended_worker"] = map[string]any{"user_id": g.aliceID, "agent_id": "agent-worker"}
	g.answerFact(offer)
	claim := func(p string) string {
		m := map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "external", "work_ref": "offer-7"}}
		if p != "" {
			m["coordination_proof"] = p
		}
		return body(t, m)
	}
	wantRefusal(t, "a team claim without its proof", team("coordinator", "agent-coord", "sess-c", "POST", "/claim", "offer-1", claim("")),
		400, "invalid_request", "")
	o := decodeOutcome(t, team("coordinator", "agent-coord", "sess-c", "POST", "/claim", "offer-1", claim(proof("offer"))))
	if o.Receipt.VerifiedMode != "team" || o.Reservation.HolderMode != "external" || o.Reservation.OwnWorkRef != "offer-7" {
		t.Fatalf("offer claim: %+v", o)
	}
	// Personal mode does not see the team hold.
	var st wireStatus
	if r := g.call(t, g.tls, "GET", g.rpath(task.ID, ""), g.alice, rhdr("", nil), "", true); r.status != 200 ||
		json.Unmarshal(r.body, &st) != nil || st.State != "none" {
		t.Fatalf("personal status of a team hold: %d %s", r.status, r.body)
	}
	accepted := g.factAs("worker", "agent-worker", "sess-w", "accepted_attempt", "transfer", task.ID, "tr-1")
	accepted["offer_ref"], accepted["attempt_ref"], accepted["process"] = "offer-7", "attempt-7", pinOf(testPin)
	g.answerFact(accepted)
	transfer := body(t, map[string]any{"expected_revision": o.TaskRevision, "reservation_id": o.Reservation.ID, "fence": o.Reservation.Fence,
		"holder": map[string]any{"mode": "external", "work_ref": "attempt-7"}, "coordination_proof": proof("accepted")})
	moved := decodeOutcome(t, team("worker", "agent-worker", "sess-w", "POST", "/transfer", "tr-1", transfer))
	if moved.Reservation.OwnWorkRef != "attempt-7" || moved.Reservation.Fence != "2" {
		t.Fatalf("transfer: %+v", moved)
	}
	// Its replay after aicrew settled the proof asks aicrew only for the
	// team context, never for the fact again.
	g.answerInactive()
	calls := g.fake.Calls()
	if again := decodeOutcome(t, team("worker", "agent-worker", "sess-w", "POST", "/transfer", "tr-1", transfer)); !again.Receipt.Replayed {
		t.Fatalf("transfer replay: %+v", again)
	}
	if n := g.fake.Calls() - calls; n != 1 {
		t.Fatalf("the replay made %d aicrew calls; only the context introspection is expected", n)
	}
	// The old coordinator cannot write after the transfer.
	coordUpdate := body(t, map[string]any{"expected_revision": moved.TaskRevision, "reservation_id": o.Reservation.ID, "fence": "1",
		"content": map[string]any{"title": task.Title, "state": "IN_PROGRESS"}})
	wantRefusal(t, "the old coordinator's update", team("coordinator", "agent-coord", "sess-c", "POST", "/update", "cu", coordUpdate),
		403, "role_forbidden", "team")
	// A worker update on the old fence is stale.
	stale := body(t, map[string]any{"expected_revision": moved.TaskRevision, "reservation_id": o.Reservation.ID, "fence": "1",
		"content": map[string]any{"title": task.Title, "state": "IN_PROGRESS"}})
	wantRefusal(t, "an update on the offer's fence", team("worker", "agent-worker", "sess-w", "POST", "/update", "wu", stale),
		409, "stale_fence", "team")
	// A second task: the process pin refuses a claim when the selection moved.
	other := g.readyTask(t, g.alpha)
	newer := testPin
	newer.Commit = strings.Repeat("9b", 20)
	g.selectProcess(t, newer)
	pinFact := g.factAs("coordinator", "agent-coord", "sess-c", "offer", "claim", other.ID, "offer-p")
	pinFact["offer_ref"], pinFact["process"] = "offer-p", pinOf(testPin)
	pinFact["intended_worker"] = map[string]any{"user_id": g.aliceID, "agent_id": "agent-worker"}
	g.answerFact(pinFact)
	r := g.call(t, g.tls, "POST", g.rpath(other.ID, "/claim"), g.alice, rhdr("offer-p", g.teamAs(t, "coordinator", "agent-coord", "sess-c")),
		body(t, map[string]any{"expected_revision": other.Revision, "holder": map[string]any{"mode": "external", "work_ref": "offer-p"},
			"coordination_proof": proof("pinned")}), true)
	wantRefusal(t, "a stale process pin", r, 409, "process_mismatch", "team")
	// Revoking the team grant refuses new effects and keeps the hold.
	if err := g.db.SetTeamGrant("admin", g.alphaInstance, g.profile.ID, false); err != nil {
		t.Fatal(err)
	}
	upd := body(t, map[string]any{"expected_revision": moved.TaskRevision, "reservation_id": moved.Reservation.ID, "fence": moved.Reservation.Fence,
		"content": map[string]any{"title": task.Title, "state": "IN_PROGRESS"}})
	wantRefusal(t, "after revocation", team("worker", "agent-worker", "sess-w", "POST", "/update", "after", upd), 403, "grant_denied", "team")
	if hold, _ := g.alpha.GetTaskReservation(task.ID); hold.ID != moved.Reservation.ID || hold.Holder.Ref != "attempt-7" {
		t.Fatalf("revocation changed the hold: %+v", hold)
	}
	// No proof in any response body or the hub's log.
	g.assertNoSecretLeak(t)
	for name, p := range proofs {
		if p == "" {
			t.Fatalf("proof %s unset", name)
		}
	}
}

// A personal standalone claim and a team offer claim race for one task:
// exactly one holds it, and the other is refused.
func TestReservationRoutesRacingClaims(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	task := g.readyTask(t, g.alpha)
	offer := g.factAs("coordinator", "agent-coord", "sess-c", "offer", "claim", task.ID, "race-team")
	offer["offer_ref"], offer["process"] = "offer-r", pinOf(testPin)
	offer["intended_worker"] = map[string]any{"user_id": g.aliceID, "agent_id": "agent-worker"}
	g.answerFact(offer)
	teamHdr := rhdr("race-team", g.teamAs(t, "coordinator", "agent-coord", "sess-c"))
	p := testProof(t)
	g.secrets = append(g.secrets, p)
	var wg sync.WaitGroup
	results := make([]identityResp, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i == 0 {
				results[i] = g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("race-own", nil),
					body(nil, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "own"}}), true)
				return
			}
			results[i] = g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, teamHdr,
				body(nil, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "external", "work_ref": "offer-r"},
					"coordination_proof": p}), true)
		}(i)
	}
	wg.Wait()
	won := 0
	for _, r := range results {
		switch {
		case r.status == 200:
			won++
		case r.status == 409 && r.code() == "reservation_conflict":
		default:
			t.Fatalf("racing claim: %d %s", r.status, r.body)
		}
	}
	if won != 1 {
		t.Fatalf("%d racing claims won: %d %s / %d %s", won, results[0].status, results[0].body, results[1].status, results[1].body)
	}
	g.assertNoSecretLeak(t)
}

// A claim through the route proves its dependencies: an unfinished one
// refuses it; once done, the claim commits.
func TestReservationRoutesClaimDependencies(t *testing.T) {
	g := newReservationRig(t)
	admin := store.TaskActor{Kind: "admin", Name: "admin"}
	dep, err := g.alpha.CreateTask(store.TaskContent{Title: "dependency", State: "IN_PROGRESS"}, admin, "dep")
	if err != nil {
		t.Fatal(err)
	}
	task, err := g.alpha.CreateTask(store.TaskContent{Title: "dependent", State: "READY", Dependencies: []string{dep.ID}}, admin, "dependent")
	if err != nil {
		t.Fatal(err)
	}
	claim := body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "own"}})
	r := g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("dep-1", nil), claim, true)
	wantRefusal(t, "an unfinished dependency", r, 404, "dependency_unresolved", "personal")
	if _, err := g.alpha.UpdateTask(dep.ID, store.TaskContent{Title: "dependency", State: "DONE"}, dep.Revision, admin, "dep-done"); err != nil {
		t.Fatal(err)
	}
	decodeOutcome(t, g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("dep-2", nil), claim, true))
}

// Aicrew unreachable: a team transition is refused as retryable, and the
// hold is untouched.
func TestReservationRoutesCoordinationOutageKeepsTheHold(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	task := g.readyTask(t, g.alpha)
	offer := g.factAs("coordinator", "agent-coord", "sess-c", "offer", "claim", task.ID, "o")
	offer["offer_ref"], offer["process"] = "offer-o", pinOf(testPin)
	offer["intended_worker"] = map[string]any{"user_id": g.aliceID, "agent_id": "agent-worker"}
	g.answerFact(offer)
	p := testProof(t)
	g.secrets = append(g.secrets, p)
	o := decodeOutcome(t, g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("o", g.teamAs(t, "coordinator", "agent-coord", "sess-c")),
		body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "external", "work_ref": "offer-o"},
			"coordination_proof": p}), true))
	g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	p2 := testProof(t)
	g.secrets = append(g.secrets, p2)
	r := g.call(t, g.tls, "POST", g.rpath(task.ID, "/release"), g.alice, rhdr("rel", g.teamAs(t, "coordinator", "agent-coord", "sess-c")),
		body(t, map[string]any{"expected_revision": o.TaskRevision, "reservation_id": o.Reservation.ID, "fence": o.Reservation.Fence,
			"content": map[string]any{"title": task.Title, "state": "READY"}, "reason": "never accepted", "coordination_proof": p2}), true)
	wantRefusal(t, "aicrew unreachable", r, 503, "context_unavailable", "team")
	var e identityRefusalBody
	if json.Unmarshal(r.body, &e); !e.Retryable {
		t.Fatalf("an outage is retryable: %s", r.body)
	}
	if hold, _ := g.alpha.GetTaskReservation(task.ID); hold.ID != o.Reservation.ID {
		t.Fatalf("an outage released the hold: %+v", hold)
	}
	g.assertNoSecretLeak(t)
}

// A receipt read the store cannot answer is unresolved: the caller must
// stop and reconcile again, never take it as not committed.
func TestReservationRoutesUnresolvedReceipt(t *testing.T) {
	g := newReservationRig(t)
	task := g.readyTask(t, g.alpha)
	reservationReceiptFault = func() error { return errors.New("storage unavailable") }
	t.Cleanup(func() { reservationReceiptFault = nil })
	r := g.call(t, g.tls, "GET", g.rpath(task.ID, "/receipts/claim/k"), g.alice, rhdr("", nil), "", true)
	wantRefusal(t, "an unanswerable receipt read", r, 503, "receipt_unresolved", "personal")
	var e identityRefusalBody
	if json.Unmarshal(r.body, &e); !e.Retryable {
		t.Fatalf("an unresolved receipt is retryable: %s", r.body)
	}
}

// A project rename keeps the hold: the old path no longer reaches the task,
// and under the new name the holder still holds it and continues.
func TestReservationRoutesProjectRenameKeepsTheHold(t *testing.T) {
	g := newReservationRig(t)
	task := g.readyTask(t, g.alpha)
	o := decodeOutcome(t, g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("rn-claim", nil),
		body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "rn"}}), true))
	if err := g.s.reg.Rename("alpha", "alpha-next"); err != nil {
		t.Fatal(err)
	}
	old := g.call(t, g.tls, "GET", g.rpath(task.ID, ""), g.alice, rhdr("", nil), "", true)
	wantRefusal(t, "the old project path", old, 404, "task_unavailable", "personal")
	path := "/v1/projects/alpha-next/tasks/" + task.ID + "/reservation"
	var st wireStatus
	if r := g.call(t, g.tls, "GET", path, g.alice, rhdr("", nil), "", true); r.status != 200 || json.Unmarshal(r.body, &st) != nil ||
		st.State != "held" || st.ReservationID != o.Reservation.ID || st.Fence != o.Reservation.Fence {
		t.Fatalf("status after rename: %d %s", r.status, r.body)
	}
	upd := decodeOutcome(t, g.call(t, g.tls, "POST", path+"/update", g.alice, rhdr("rn-upd", nil),
		body(t, map[string]any{"expected_revision": o.TaskRevision, "reservation_id": o.Reservation.ID, "fence": o.Reservation.Fence,
			"content": map[string]any{"title": task.Title, "state": "IN_PROGRESS"}}), true))
	if upd.Reservation.ID != o.Reservation.ID || upd.Reservation.Fence != "2" {
		t.Fatalf("update after rename: %+v", upd)
	}
}
