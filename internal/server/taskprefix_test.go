package server

// A task or comment id may be named by a unique prefix of at least 13
// characters (01a17718-4ca9): the routes resolve it to the full id before
// anything reads it, refuse a shorter one with the rule, and refuse one
// that starts two tasks with the ids the caller may read.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/store"
)

// cloneTask copies a task row of one project into another under id,
// through a second connection to the target's store: two tasks whose ids
// share a prefix cannot be made through the API, which mints the ids.
func cloneTask(t *testing.T, reg *store.Registry, from, to, src, id string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(reg.Root(), "projects", to, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema := "main"
	if from != to {
		schema = "src"
		if _, err := db.Exec(`ATTACH DATABASE '` + filepath.Join(reg.Root(), "projects", from, "journal.db") + `' AS src`); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`CREATE TEMP TABLE clone AS SELECT * FROM ` + schema + `.tasks WHERE id = '` + src + `'`,
		`UPDATE clone SET id = '` + id + `'`,
		`INSERT INTO main.tasks SELECT * FROM clone`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// sibling is another valid id with the same first 35 characters.
func sibling(id string) string {
	last := "0"
	if id[35] == '0' {
		last = "1"
	}
	return id[:35] + last
}

func TestTaskRoutesResolveAUniqueIDPrefix(t *testing.T) {
	f := newTaskFixture(t)
	h := f.h
	task := decodeTask(t, taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", taskBody))
	short := task.ID[:store.TaskIDPrefixMin]

	// Every task route takes the prefix and answers for the full id.
	if w := taskReq(t, h, "GET", "/v1/tasks/"+short, f.alice, "", ""); w.Code != 200 || decodeTask(t, w).ID != task.ID || decodeTask(t, w).Links.Self != "/v1/tasks/"+task.ID {
		t.Fatalf("get by prefix: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+short, f.alice, "u1", `{"title":"v2","state":"READY","expected_revision":1}`); w.Code != 200 || decodeTask(t, w).Revision != 2 {
		t.Fatalf("update by prefix: %d %s", w.Code, w.Body)
	}
	// The revision check runs against the full id: a stale revision by
	// prefix is the same conflict.
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+short, f.alice, "u2", `{"title":"stale","state":"READY","expected_revision":1}`); w.Code != 409 {
		t.Fatalf("stale update by prefix: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "POST", "/v1/tasks/"+short+"/triage", f.alice, "t1", `{"state":"BACKLOG","expected_revision":2}`); w.Code != 200 {
		t.Fatalf("triage by prefix: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+short+"/history", f.alice, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), task.ID) {
		t.Fatalf("history by prefix: %d %s", w.Code, w.Body)
	}
	// A retry with the prefix and a retry with the full id are one request.
	w := taskReq(t, h, "POST", "/v1/tasks/"+short+"/comments", f.alice, "c1", `{"body":"first"}`)
	if w.Code != 201 {
		t.Fatalf("comment by prefix: %d %s", w.Code, w.Body)
	}
	var c1, c2 struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &c1)
	w = taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "c1", `{"body":"first"}`)
	json.Unmarshal(w.Body.Bytes(), &c2)
	if w.Code != 201 || c1.ID == "" || c2.ID != c1.ID || c1.TaskID != task.ID {
		t.Fatalf("retry with the full id: %d %s (first %+v)", w.Code, w.Body, c1)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+short+"/comments", f.alice, "", ""); w.Code != 200 || strings.Count(w.Body.String(), `"sequence"`) != 1 {
		t.Fatalf("comments by prefix, one comment: %d %s", w.Code, w.Body)
	}
	// A comment id takes a prefix too, among the task's comments.
	if w := taskReq(t, h, "GET", "/v1/tasks/"+short+"/comments/"+c1.ID[:store.TaskIDPrefixMin], f.alice, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"`+c1.ID+`"`) {
		t.Fatalf("comment by prefix: %d %s", w.Code, w.Body)
	}

	// Shorter than the rule: refused with the rule. A prefix that starts
	// no task: not found.
	if w := taskReq(t, h, "GET", "/v1/tasks/"+short[:12], f.alice, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "at least 13 characters") {
		t.Fatalf("short prefix: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/00000000-0000", f.alice, "", ""); w.Code != 404 {
		t.Fatalf("unknown prefix: %d %s", w.Code, w.Body)
	}
	// Uppercase is not the id as written: not a prefix, not found.
	if w := taskReq(t, h, "GET", "/v1/tasks/"+strings.ToUpper(short), f.alice, "", ""); w.Code != 404 {
		t.Fatalf("uppercase prefix: %d %s", w.Code, w.Body)
	}

	// Two tasks, in two projects, under one prefix: refused with both ids;
	// the longer prefix that tells them apart still resolves.
	twin := sibling(task.ID)
	cloneTask(t, f.reg, "alpha", "beta", task.ID, twin)
	w = taskReq(t, h, "GET", "/v1/tasks/"+short, f.alice, "", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "ambiguous id prefix "+short) || !strings.Contains(w.Body.String(), task.ID) || !strings.Contains(w.Body.String(), twin) || strings.Contains(w.Body.String(), "cannot read") {
		t.Fatalf("ambiguous prefix: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+task.ID, f.alice, "", ""); w.Code != 200 {
		t.Fatalf("the full id after the clone: %d %s", w.Code, w.Body)
	}
}

// A team session is told only the ids in projects its profile is granted;
// the others are counted.
func TestTeamAmbiguousPrefixNamesOnlyGrantedIDs(t *testing.T) {
	g := newTeamRig(t)
	// The rig makes its alpha and beta tasks in one breath, often in one
	// millisecond, so their 13-character prefixes may well be one: a
	// longer prefix tells them apart.
	short := g.alphaTask[:30]
	if r := g.team(t, "GET", "/v1/tasks/"+short); r.status != 200 || !strings.Contains(string(r.body), g.alphaTask) {
		t.Fatalf("team, unique prefix: %d %s", r.status, r.body)
	}
	twin := sibling(g.alphaTask)
	cloneTask(t, g.s.reg, "alpha", "beta", g.alphaTask, twin)
	r := g.team(t, "GET", "/v1/tasks/"+short)
	if r.status != 409 || !strings.Contains(string(r.body), g.alphaTask) || strings.Contains(string(r.body), twin) || !strings.Contains(string(r.body), "and 1 more in projects this credential cannot read") {
		t.Fatalf("team, ambiguous prefix: %d %s", r.status, r.body)
	}
	// The beta task's own prefix, unique, is beta's and stays refused.
	if r := g.team(t, "GET", "/v1/tasks/"+g.betaTk[:30]); r.status != 403 || r.code() != "grant_denied" {
		t.Fatalf("team, prefix of an ungranted task: %d %s", r.status, r.body)
	}
}

// The reservation routes take a unique prefix inside the route's project
// and resolve it before anything reads the task id: a coordination proof
// made for the full id vouches for a request by prefix, and one made for
// another task is still refused.
func TestReservationRoutesTakeAUniqueTaskIDPrefix(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	task := g.readyTask(t, g.alpha)
	other := g.readyTask(t, g.alpha)
	// The two tasks may share their first 13 characters (one millisecond);
	// 30 tell them apart.
	pfx, otherPfx := task.ID[:30], other.ID[:30]

	// Personal mode: a claim and a status read by prefix.
	claim := body(t, map[string]any{"expected_revision": other.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "own-1"}})
	o := decodeOutcome(t, g.call(t, g.tls, "POST", g.rpath(otherPfx, "/claim"), g.alice, rhdr("p1", nil), claim, true))
	if !o.Reservation.Active || o.Reservation.OwnWorkRef != "own-1" {
		t.Fatalf("personal claim by prefix: %+v", o)
	}
	if hold, _ := g.alpha.GetTaskReservation(other.ID); hold.ID != o.Reservation.ID {
		t.Fatalf("the claim by prefix holds another task: %+v", hold)
	}
	if r := g.call(t, g.tls, "GET", g.rpath(otherPfx, ""), g.alice, rhdr("", nil), "", true); r.status != 200 || !strings.Contains(string(r.body), `"state":"held"`) {
		t.Fatalf("status by prefix: %d %s", r.status, r.body)
	}
	// Too short, in another project, or shared by two tasks: unavailable.
	wantRefusal(t, "a short prefix", g.call(t, g.tls, "POST", g.rpath(task.ID[:12], "/claim"), g.alice, rhdr("p2", nil), claim, true), 404, "task_unavailable", "personal")
	wantRefusal(t, "a prefix of another project", g.call(t, g.tls, "POST", "/v1/projects/beta/tasks/"+pfx+"/reservation/claim", g.alice, rhdr("p3", nil), claim, true), 404, "task_unavailable", "personal")

	// Team mode: the proof's fact names the full id; the request names
	// the prefix.
	offer := func(taskID, key string) map[string]any {
		f := g.factAs("coordinator", "agent-coord", "sess-c", "offer", "claim", taskID, key)
		f["offer_ref"], f["process"] = "offer-7", pinOf(testPin)
		f["intended_worker"] = map[string]any{"user_id": g.aliceID, "agent_id": "agent-worker"}
		return f
	}
	teamClaim := func(path, key string) identityResp {
		p := testProof(t)
		g.secrets = append(g.secrets, p)
		b := body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "external", "work_ref": "offer-7"}, "coordination_proof": p})
		return g.call(t, g.tls, "POST", path, g.alice, rhdr(key, g.teamAs(t, "coordinator", "agent-coord", "sess-c")), b, true)
	}
	// A proof for this task, sent to the other task's prefix: refused.
	g.answerFact(offer(task.ID, "offer-x"))
	wantRefusal(t, "a proof for another task", teamClaim(g.rpath(otherPfx, "/claim"), "offer-x"), 403, "coordination_rejected", "team")
	// The same kind of proof sent to its own task's prefix: accepted.
	g.answerFact(offer(task.ID, "offer-1"))
	if o := decodeOutcome(t, teamClaim(g.rpath(pfx, "/claim"), "offer-1")); o.Receipt.VerifiedMode != "team" || o.Reservation.OwnWorkRef != "offer-7" {
		t.Fatalf("team claim by prefix: %+v", o)
	}
	if hold, _ := g.alpha.GetTaskReservation(task.ID); hold.ID == "" {
		t.Fatal("the team claim by prefix holds nothing")
	}

	// Two tasks under one prefix in the project: unavailable.
	cloneTask(t, g.s.reg, "alpha", "alpha", task.ID, sibling(task.ID))
	wantRefusal(t, "a shared prefix", g.call(t, g.tls, "GET", g.rpath(pfx, ""), g.alice, rhdr("", nil), "", true), 404, "task_unavailable", "personal")
}
