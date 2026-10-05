package server

// Coordinator triage (docs/DESIGN-AIFORGE-PILOT-1.md §4) through the real
// routes: the pilot's path from a BACKLOG task to a coordinated claim with
// no operator edit, and every refusal on the way.

import (
	"encoding/json"
	"strings"
	"testing"

	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

func (g *reservationRig) backlogTask(t *testing.T) store.Task {
	t.Helper()
	task, err := g.alpha.CreateTask(store.TaskContent{
		Title: "triage me", State: "BACKLOG", NextAction: "assess",
		CandidateRefs: []store.TaskRef{{Kind: "text", Ref: "kept as is"}},
	}, store.TaskActor{Kind: "admin", Name: "admin"}, "backlog-"+uuidv7.New())
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (g *reservationRig) triage(t *testing.T, bearer string, team map[string]string, id string, b map[string]any) identityResp {
	t.Helper()
	h := map[string]string{"Idempotency-Key": "triage-" + uuidv7.New()}
	for k, v := range team {
		h[k] = v
	}
	return g.call(t, g.tls, "POST", "/v1/tasks/"+id+"/triage", bearer, h, body(t, b), true)
}

func TestTriageTheCoordinatorPathToAClaim(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	task := g.backlogTask(t)
	coord := func() map[string]string { return g.teamAs(t, "coordinator", "agent-coord", "sess-c") }
	worker := func() map[string]string { return g.teamAs(t, "worker", "agent-worker", "sess-w") }

	// The report says what each role may write.
	for role, want := range map[string]any{"coordinator": "triage", "worker": false} {
		var report map[string]any
		r := g.call(t, g.tls, "GET", "/v1/access/identity", g.alice, g.teamAs(t, role, "agent-x", "sess-x"), "", true)
		if r.status != 200 || json.Unmarshal(r.body, &report) != nil || report["task_write"] != want {
			t.Fatalf("%s report: %d %s", role, r.status, r.body)
		}
	}

	// A coordinated claim on the BACKLOG task: task_not_ready, not retryable.
	claimBody := func(revision int64, proof string) string {
		return body(t, map[string]any{"expected_revision": revision, "holder": map[string]any{"mode": "external", "work_ref": "offer-7"},
			"coordination_proof": proof})
	}
	offer := func(key string) {
		f := g.factAs("coordinator", "agent-coord", "sess-c", "offer", "claim", task.ID, key)
		f["offer_ref"], f["process"] = "offer-7", pinOf(testPin)
		f["intended_worker"] = map[string]any{"user_id": g.aliceID, "agent_id": "agent-worker"}
		g.answerFact(f)
	}
	offer("offer-1")
	p := testProof(t)
	g.secrets = append(g.secrets, p)
	r := g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("offer-1", coord()), claimBody(task.Revision, p), true)
	wantRefusal(t, "a claim on a BACKLOG task", r, 409, "task_not_ready", "team")
	var env identityRefusalBody
	json.Unmarshal(r.body, &env)
	if env.Retryable || !strings.Contains(env.NextAction, "triages the task to READY") {
		t.Fatalf("task_not_ready envelope: %s", r.body)
	}

	// A worker may not triage or comment; the coordinator may.
	if r := g.triage(t, g.alice, worker(), task.ID, map[string]any{"expected_revision": task.Revision, "state": "READY"}); r.status != 403 || r.code() != "role_forbidden" {
		t.Fatalf("worker triage: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/tasks/"+task.ID+"/comments", g.alice, merge(worker(), map[string]string{"Idempotency-Key": "c-w"}), `{"body":"not mine to say"}`, true); r.status != 403 || r.code() != "role_forbidden" {
		t.Fatalf("worker comment: %d %s", r.status, r.body)
	}
	var triaged taskResponse
	r = g.triage(t, g.alice, coord(), task.ID, map[string]any{"expected_revision": task.Revision, "state": "READY", "next_action": "offer to a worker"})
	if r.status != 200 || json.Unmarshal(r.body, &triaged) != nil || triaged.State != "READY" || triaged.NextAction != "offer to a worker" {
		t.Fatalf("coordinator triage: %d %s", r.status, r.body)
	}
	// Partial: every field it did not name is kept.
	if len(triaged.CandidateRefs) != 1 || triaged.CandidateRefs[0].Ref != "kept as is" || triaged.Title != "triage me" || triaged.Revision != task.Revision+1 {
		t.Fatalf("triage changed fields it did not name: %+v", triaged.Task)
	}
	hist, err := g.alpha.TaskHistory(task.ID, 0, 10)
	if err != nil || len(hist.Changes) != 2 || hist.Changes[1].Actor.Kind != "user" || hist.Changes[1].Actor.UserID != g.aliceID {
		t.Fatalf("history actor: %+v %v", hist.Changes, err)
	}
	if r := g.call(t, g.tls, "POST", "/v1/tasks/"+task.ID+"/comments", g.alice, merge(coord(), map[string]string{"Idempotency-Key": "c-1"}), `{"body":"Assessed READY: acceptance criteria are concrete."}`, true); r.status != 201 {
		t.Fatalf("coordinator comment: %d %s", r.status, r.body)
	}

	// The step begins again: a new offer, a new proof, a new key; the claim succeeds.
	offer("offer-2")
	p2 := testProof(t)
	g.secrets = append(g.secrets, p2)
	o := decodeOutcome(t, g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("offer-2", coord()), claimBody(triaged.Revision, p2), true))
	if o.Reservation.OwnWorkRef != "offer-7" {
		t.Fatalf("claim after triage: %+v", o)
	}

	// Under the hold, triage and comments are refused with task_held.
	if r := g.triage(t, g.alice, coord(), task.ID, map[string]any{"expected_revision": o.TaskRevision, "next_action": "x"}); r.status != 409 || r.code() != "task_held" {
		t.Fatalf("triage under a hold: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/tasks/"+task.ID+"/comments", g.alice, merge(coord(), map[string]string{"Idempotency-Key": "c-2"}), `{"body":"while held"}`, true); r.status != 409 || r.code() != "task_held" {
		t.Fatalf("comment under a hold: %d %s", r.status, r.body)
	}

	// A coordinator on a project the profile is not granted.
	beta, err := g.s.reg.Open("beta")
	if err != nil {
		t.Fatal(err)
	}
	other, err := beta.CreateTask(store.TaskContent{Title: "elsewhere", State: "BACKLOG"}, store.TaskActor{Kind: "admin", Name: "admin"}, "b-1")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.triage(t, g.alice, coord(), other.ID, map[string]any{"expected_revision": other.Revision, "state": "READY"}); r.status != 403 || r.code() != "grant_denied" {
		t.Fatalf("ungranted project: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

// Personal mode: the ordinary task-write check, a partial update, and the
// BACKLOG/READY bounds.
func TestTriagePersonal(t *testing.T) {
	g := newReservationRig(t)
	task := g.backlogTask(t)
	for _, b := range []map[string]any{
		{"expected_revision": task.Revision},
		{"expected_revision": task.Revision, "state": "DONE"},
		{"expected_revision": task.Revision, "state": "READY", "title": "not a triage field"},
		{"state": "READY"},
	} {
		if r := g.triage(t, g.alice, nil, task.ID, b); r.status != 400 {
			t.Fatalf("%v: %d %s", b, r.status, r.body)
		}
	}
	if r := g.triage(t, g.alice, nil, task.ID, map[string]any{"expected_revision": task.Revision + 5, "state": "READY"}); r.status != 409 || !strings.Contains(string(r.body), `"current"`) {
		t.Fatalf("stale revision: %d %s", r.status, r.body)
	}
	var out taskResponse
	r := g.triage(t, g.alice, nil, task.ID, map[string]any{"expected_revision": task.Revision, "next_action": "only this"})
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil || out.State != "BACKLOG" || out.NextAction != "only this" || len(out.CandidateRefs) != 1 {
		t.Fatalf("next_action only: %d %s", r.status, r.body)
	}
	// A task outside BACKLOG and READY is not triaged into them.
	inProgress, err := g.alpha.UpdateTask(task.ID, store.TaskContent{Title: "triage me", State: "IN_PROGRESS"}, out.Revision, store.TaskActor{Kind: "admin", Name: "admin"}, "ip-1")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.triage(t, g.alice, nil, task.ID, map[string]any{"expected_revision": inProgress.Revision, "state": "READY"}); r.status != 400 || !strings.Contains(string(r.body), "this task is IN_PROGRESS") {
		t.Fatalf("triage of an IN_PROGRESS task: %d %s", r.status, r.body)
	}
	// The hub admin may triage too.
	if r := g.triage(t, g.env, nil, task.ID, map[string]any{"expected_revision": inProgress.Revision, "next_action": "admin may"}); r.status != 200 {
		t.Fatalf("admin triage: %d %s", r.status, r.body)
	}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
