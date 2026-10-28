package server

// task.read: one task's text for aicrewd, served over the real TLS listener
// with real peer credentials, in the board rig's granted projects.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"aimem/internal/store"
)

type taskReadRig struct {
	*boardRig
	taskReadID, taskRead string
}

func newTaskReadRig(t *testing.T) *taskReadRig {
	t.Helper()
	g := &taskReadRig{boardRig: newBoardRig(t)}
	g.taskReadID, g.taskRead = g.issueCredentialFor(t, "aicrew-example", "task.read", time.Now().Add(24*time.Hour))
	return g
}

func (g *taskReadRig) readTask(t *testing.T, bearer, project, id string) identityResp {
	t.Helper()
	return g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/task-reads/"+project+"/"+id, bearer, v1, "", true)
}

func (g *taskReadRig) text(t *testing.T, project, id string) taskTextView {
	t.Helper()
	r := g.readTask(t, g.taskRead, project, id)
	var v taskTextView
	if r.status != 200 || json.Unmarshal(r.body, &v) != nil {
		t.Fatalf("read %s/%s: %d %s", project, id, r.status, r.body)
	}
	return v
}

func TestTaskRead(t *testing.T) {
	g := newTaskReadRig(t)
	db, err := g.s.reg.OpenExisting("beta")
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(store.TaskContent{Title: "freeze me", State: "READY", Objective: "the objective",
		AcceptanceCriteria: "the criteria", NonGoals: "the non-goals", NextAction: "private next", Blocker: "",
		EvidenceRefs: []store.TaskRef{{Kind: "text", Ref: "private evidence"}}}, boardActor, "tr-1")
	if err != nil {
		t.Fatal(err)
	}
	want := taskTextView{"beta", task.ID, 1, "READY", "freeze me", "the objective", "the criteria", "the non-goals"}
	if v := g.text(t, "beta", task.ID); v != want {
		t.Fatalf("answer %+v, want %+v", v, want)
	}
	r := g.readTask(t, g.taskRead, "beta", task.ID)
	if r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", r.header.Get("Cache-Control"))
	}
	for _, leak := range []string{"private next", "private evidence", "evidence_refs", "next_action", "blocker"} {
		if strings.Contains(string(r.body), leak) {
			t.Fatalf("the answer carries %q: %s", leak, r.body)
		}
	}
	// An edit is read at its new revision.
	edited := store.TaskContent{Title: "freeze me", State: "IN_PROGRESS", Objective: "a new objective"}
	if _, err := db.UpdateTask(task.ID, edited, task.Revision, boardActor, "tr-2"); err != nil {
		t.Fatal(err)
	}
	if v := g.text(t, "beta", task.ID); v.Revision != 2 || v.State != "IN_PROGRESS" || v.Objective != "a new objective" || v.AcceptanceCriteria != "" {
		t.Fatalf("after the edit: %+v", v)
	}
	g.auditHas(t, "task.read", "peer:aicrew-example", "service=aicrew-example", "project=beta", "task="+task.ID, "revision=2")

	// beta is granted through both teams: disabling teamB keeps it,
	// disabling teamA too hides it.
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamB, `{"disabled":true}`); r.status != 200 {
		t.Fatalf("disable: %d %s", r.status, r.body)
	}
	g.text(t, "beta", task.ID)
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamA, `{"disabled":true}`); r.status != 200 {
		t.Fatalf("disable: %d %s", r.status, r.body)
	}
	if r := g.readTask(t, g.taskRead, "beta", task.ID); r.status != 404 || r.code() != "task_not_found" {
		t.Fatalf("every granting profile disabled: %d %s", r.status, r.body)
	}
	g.auditHas(t, "task.read.refused.task_not_found", "peer:aicrew-example", "credential="+g.taskReadID, "reason=not_granted")
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamA, `{"disabled":false}`); r.status != 200 {
		t.Fatalf("enable: %d %s", r.status, r.body)
	}
	g.text(t, "beta", task.ID)
	g.assertNoSecretLeak(t)
}

// Every project the peer may not read and every unknown task get the same
// answer, byte for byte apart from the correlation ID; the audit keeps the
// reason.
func TestTaskReadRefusesAlikeOutsideTheGrant(t *testing.T) {
	g := newTaskReadRig(t)
	g.task(t, "alpha", "a1", "READY")
	g.task(t, "delta", "d1", "READY") // granted only to another peer's team
	if _, err := g.s.reg.Open("ungranted"); err != nil {
		t.Fatal(err)
	}
	g.task(t, "ungranted", "u1", "READY")
	const actor = "peer:aicrew-example"
	unknownTask := "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a60"
	cases := []struct{ name, project, id, reason string }{
		{"another peer's grant", "delta", g.tasks["d1"], "not_granted"},
		{"no grant", "ungranted", g.tasks["u1"], "not_granted"},
		{"unknown project", "nosuch", unknownTask, "unknown_project"},
		{"reserved project", store.UserScopeProject, unknownTask, "reserved_project"},
		{"knowledge group", "group-x", unknownTask, "reserved_project"},
		{"unknown task", "alpha", unknownTask, "unknown_task"},
		{"malformed task ID", "alpha", "not-a-task", "unknown_task"},
	}
	var first string
	for _, c := range cases {
		r := g.readTask(t, g.taskRead, c.project, c.id)
		if r.status != 404 || r.code() != "task_not_found" {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
		var body map[string]any
		if err := json.Unmarshal(r.body, &body); err != nil {
			t.Fatal(err)
		}
		delete(body, "correlation_id")
		b, _ := json.Marshal(body)
		if first == "" {
			first = string(b)
		} else if string(b) != first {
			t.Fatalf("%s answers %s, the first case %s", c.name, b, first)
		}
		g.auditHas(t, "task.read.refused.task_not_found", actor, "credential="+g.taskReadID, "reason="+c.reason)
	}
	// A revoked grant hides the project's tasks; given back, they read.
	g.text(t, "alpha", g.tasks["a1"])
	g.grant(t, teamA, "alpha", false)
	if r := g.readTask(t, g.taskRead, "alpha", g.tasks["a1"]); r.status != 404 || r.code() != "task_not_found" {
		t.Fatalf("revoked grant: %d %s", r.status, r.body)
	}
	g.grant(t, teamA, "alpha", true)
	g.text(t, "alpha", g.tasks["a1"])
	g.assertNoSecretLeak(t)
}

func TestTaskReadRefusals(t *testing.T) {
	g := newTaskReadRig(t)
	g.task(t, "alpha", "a1", "READY")
	id := g.tasks["a1"]
	const actor = "peer:aicrew-example"
	// Only a task.read credential of the path's own peer reads.
	for name, bearer := range map[string]string{"board.read": g.board, "team.read": g.teamRegRig.read,
		"team.register": g.register, "identity.redeem": g.redeem} {
		if r := g.readTask(t, bearer, "alpha", id); r.status != 403 || r.code() != "peer_forbidden" {
			t.Fatalf("%s credential: %d %s", name, r.status, r.body)
		}
	}
	g.auditHas(t, "task.read.refused.peer_forbidden", actor, "credential="+g.boardID)
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/other-peer/task-reads/alpha/"+id, g.taskRead, v1, "", true); r.status != 403 || r.code() != "peer_forbidden" {
		t.Fatalf("another peer's path: %d %s", r.status, r.body)
	}
	// The task.read credential reaches nothing else.
	for _, path := range []string{"/v1/identity/peers/aicrew-example/board-changes", "/v1/identity/peers/aicrew-example/team-reads",
		"/v1/projects/alpha/tasks/" + id, "/v1/identity/peers/aicrew-example/teams"} {
		if r := g.call(t, g.tls, "GET", path, g.taskRead, v1, "", true); r.status != 403 {
			t.Fatalf("task.read credential on %s: %d %s", path, r.status, r.body)
		}
	}
	if r := g.readTask(t, g.alice, "alpha", id); r.status != 401 || r.code() != "peer_unauthenticated" {
		t.Fatalf("individual bearer: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.plain, "GET", "/v1/identity/peers/aicrew-example/task-reads/alpha/"+id, g.taskRead, v1, "", true); r.status != 403 || r.code() != "tls_required" {
		t.Fatalf("plain HTTP: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/task-reads/alpha/"+id, g.taskRead, nil, "", true); r.status != 400 || r.code() != "unsupported_version" {
		t.Fatalf("no version: %d %s", r.status, r.body)
	}
	// The read scope's per-credential bound.
	now := time.Now()
	g.s.readLimit = &readScopeLimiter{now: func() time.Time { return now }}
	for i := 0; i < readScopePerMinute; i++ {
		if r := g.readTask(t, g.taskRead, "alpha", id); r.status != 200 {
			t.Fatalf("read %d: %d %s", i, r.status, r.body)
		}
	}
	if r := g.readTask(t, g.taskRead, "alpha", id); r.status != 429 || r.code() != "rate_limited" {
		t.Fatalf("over the bound: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

// The answer is bounded by the task's own write limits: the largest task
// the store takes, in characters JSON escapes the most, stays under 40 KiB.
func TestTaskReadAnswerIsBounded(t *testing.T) {
	g := newTaskReadRig(t)
	db, err := g.s.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	title := strings.Repeat("<", store.MaxTaskTitleBytes)
	c := store.TaskContent{Title: title, State: "READY"}
	// Grow the objective until the store refuses, then keep the last taken.
	var task store.Task
	for n := 1024; ; n += 64 {
		c.Objective = strings.Repeat("<", n)
		next, err := db.CreateTask(c, boardActor, "big-"+strconv.Itoa(n))
		if err != nil {
			break
		}
		task = next
	}
	if task.ID == "" {
		t.Fatal("no task was taken")
	}
	r := g.readTask(t, g.taskRead, "alpha", task.ID)
	if r.status != 200 || len(r.body) >= 40*1024 || len(r.body) < store.MaxTaskBytes-1024 {
		t.Fatalf("the largest task answers %d bytes (status %d)", len(r.body), r.status)
	}
}
