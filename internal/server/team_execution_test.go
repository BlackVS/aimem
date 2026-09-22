package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/store"
)

func (f *assignmentFixture) execute(t *testing.T, id, op, token, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return taskReq(t, f.h, "POST", f.base+"/assignments/"+id+"/"+op, token, key, assignmentJSON(t, body))
}

func (f *assignmentFixture) reservedURL(h store.TeamSessionHandle) string {
	return fmt.Sprintf("%s/assignments/reserved?session_id=%s&generation=%d", f.base, h.SessionID, h.Generation)
}

func (f *assignmentFixture) taskState(t *testing.T) store.Task {
	t.Helper()
	w := taskReq(t, f.h, "GET", "/v1/tasks/"+f.task.ID, f.alice, "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var task store.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *assignmentFixture) running(t *testing.T) store.TeamAssignment {
	t.Helper()
	offer := assignmentResult(t, f.offerRequest(t, "offer"), 201)
	return assignmentResult(t, f.command(t, offer.ID, "accept", f.peer, "accept", store.TeamAssignmentCommand{TeamSessionHandle: f.recipient}), 200)
}

func workBody(h store.TeamSessionHandle, coordinatorGeneration, revision int64, reason string) store.TeamWorkCommand {
	return store.TeamWorkCommand{TeamSessionHandle: h, CoordinatorGeneration: coordinatorGeneration, ExpectedRevision: revision, Reason: reason}
}

func submission(h store.TeamSessionHandle, revision int64) store.TeamResultSubmission {
	return store.TeamResultSubmission{TeamSessionHandle: h, ExpectedRevision: revision, TeamResultContent: store.TeamResultContent{
		BaseCommit: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Summary: "Bounded change ready for review",
		Validation: "go test ./... passed; candidate needs review and human merge", EvidenceRefs: []store.TaskRef{{Kind: "text", Ref: "Disposable test run: all packages passed"}},
	}}
}

func TestExecutionHTTPWorkAndResult(t *testing.T) {
	f := newAssignmentFixture(t)
	run := f.running(t)
	// The worker reads its outstanding attempt; the coordinator holds none.
	if got := assignmentResult(t, taskReq(t, f.h, "GET", f.reservedURL(f.recipient), f.peer, "", ""), 200); got.ID != run.ID || got.State != "RUNNING" {
		t.Fatal(got)
	}
	if w := taskReq(t, f.h, "GET", f.reservedURL(f.sender), f.alice, "", ""); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	stale := f.recipient
	stale.Generation++
	if w := taskReq(t, f.h, "GET", f.reservedURL(stale), f.peer, "", ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	for _, q := range []string{"?session_id=" + f.recipient.SessionID, "?session_id=" + f.recipient.SessionID + "&generation=0", f.reservedURL(f.recipient)[len(f.base+"/assignments/reserved"):] + "&extra=1"} {
		if w := taskReq(t, f.h, "GET", f.base+"/assignments/reserved"+q, f.peer, "", ""); w.Code != 400 {
			t.Fatal(q, w.Code, w.Body)
		}
	}
	// Block and resume through HTTP move the task with the attempt.
	blocked := assignmentResult(t, f.execute(t, run.ID, "block", f.peer, "block", workBody(f.recipient, 0, 2, "waiting for an answer")), 200)
	if blocked.State != "BLOCKED" || f.taskState(t).State != "BLOCKED" || f.taskState(t).Blocker != "waiting for an answer" {
		t.Fatal(blocked, f.taskState(t))
	}
	if again := assignmentResult(t, f.execute(t, run.ID, "block", f.peer, "block", workBody(f.recipient, 0, 2, "waiting for an answer")), 200); again.State != "BLOCKED" {
		t.Fatal("replay", again)
	}
	if w := f.execute(t, run.ID, "block", f.peer, "block", workBody(f.recipient, 0, 2, "different")); w.Code != 409 {
		t.Fatal("retry mismatch", w.Code, w.Body)
	}
	resumed := assignmentResult(t, f.execute(t, run.ID, "resume-work", f.peer, "resume-work", workBody(f.recipient, 0, 3, "answered")), 200)
	if resumed.State != "RUNNING" || f.taskState(t).State != "IN_PROGRESS" {
		t.Fatal(resumed)
	}
	// Only the assigned worker submits; the coordinator is refused.
	if w := f.execute(t, run.ID, "submit", f.alice, "co-submit", submission(f.sender, 4)); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	submitted := assignmentResult(t, f.execute(t, run.ID, "submit", f.peer, "submit", submission(f.recipient, 4)), 200)
	if submitted.State != "SUBMITTED" || submitted.Result == nil || submitted.Result.Worker != f.recipient || f.taskState(t).State != "REVIEW" {
		t.Fatal(submitted)
	}
	if replay := assignmentResult(t, f.execute(t, run.ID, "submit", f.peer, "submit", submission(f.recipient, 4)), 200); replay.Result.ID != submitted.Result.ID {
		t.Fatal("submit replay", replay)
	}
	// Only the current coordinator reviews; a worker handle is stale for it.
	decision := store.TeamResultDecision{TeamSessionHandle: f.recipient, ExpectedRevision: 5, ResultID: submitted.Result.ID, Decision: "accept", Reason: "Evidence assessed"}
	if w := f.execute(t, run.ID, "review", f.peer, "worker-review", decision); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	decision.TeamSessionHandle, decision.CoordinatorGeneration = f.sender, 1
	accepted := assignmentResult(t, f.execute(t, run.ID, "review", f.alice, "review", decision), 200)
	if accepted.State != "ACCEPTED" || accepted.Review == nil || accepted.Review.Decision != "accept" {
		t.Fatal(accepted)
	}
	task := f.taskState(t)
	if task.State != "REVIEW" || task.Coordination == nil || task.Coordination.AttemptID != "" {
		t.Fatal("acceptance must leave REVIEW without a reservation", task)
	}
	if w := taskReq(t, f.h, "GET", f.reservedURL(f.recipient), f.peer, "", ""); w.Code != 404 {
		t.Fatal("released attempt still reserved", w.Code, w.Body)
	}
}

func TestExecutionHTTPStopAndClose(t *testing.T) {
	f := newAssignmentFixture(t)
	run := f.running(t)
	// A worker cannot cancel; the coordinator cannot acknowledge.
	if w := f.execute(t, run.ID, "cancel", f.peer, "worker-cancel", workBody(f.recipient, 0, 2, "stop")); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	requested := assignmentResult(t, f.execute(t, run.ID, "cancel", f.alice, "cancel", workBody(f.sender, 1, 2, "scope changed")), 200)
	if requested.State != "STOP_REQUESTED" || f.taskState(t).State != "BLOCKED" {
		t.Fatal(requested)
	}
	if w := f.execute(t, run.ID, "stopped", f.alice, "co-stopped", workBody(f.sender, 1, 3, "stopped")); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	// Resume rebinds the worker's attempt: the old handle is stale, the new one acknowledges.
	w := taskReq(t, f.h, "POST", f.base+"/resume", f.peer, "resume", assignmentJSON(t, f.recipient))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.execute(t, run.ID, "stopped", f.peer, "old-stopped", workBody(f.recipient, 0, 3, "stopped")); w.Code != 409 {
		t.Fatal("old handle acknowledged after resume", w.Code, w.Body)
	}
	current := store.TeamSessionHandle{SessionID: f.recipient.SessionID, Generation: 2}
	if got := assignmentResult(t, taskReq(t, f.h, "GET", f.reservedURL(current), f.peer, "", ""), 200); got.ID != run.ID || got.Worker != current || got.RebindCount != 1 {
		t.Fatal(got)
	}
	stopped := assignmentResult(t, f.execute(t, run.ID, "stopped", f.peer, "stopped", workBody(current, 0, 3, "local command finished")), 200)
	if stopped.State != "STOPPED" {
		t.Fatal(stopped)
	}
	closed := assignmentResult(t, f.execute(t, run.ID, "close-stop", f.alice, "close", workBody(f.sender, 1, 4, "requeue")), 200)
	task := f.taskState(t)
	if closed.State != "CANCELLED" || task.State != "READY" || task.Blocker != "" || task.Coordination.AttemptID != "" {
		t.Fatal(closed, task)
	}
	if w := taskReq(t, f.h, "GET", f.reservedURL(current), f.peer, "", ""); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestExecutionHTTPRefusals(t *testing.T) {
	f := newAssignmentFixture(t)
	run := f.running(t)
	body := assignmentJSON(t, workBody(f.recipient, 0, 2, "waiting"))
	for _, tc := range []struct {
		name         string
		method, path string
		token, key   string
		body         string
		want         int
	}{
		{"admin token", "POST", f.base + "/assignments/" + run.ID + "/block", f.admin, "k1", body, 403},
		{"missing key", "POST", f.base + "/assignments/" + run.ID + "/block", f.peer, "", body, 400},
		{"invalid json", "POST", f.base + "/assignments/" + run.ID + "/block", f.peer, "k2", "{", 400},
		{"unknown field", "POST", f.base + "/assignments/" + run.ID + "/block", f.peer, "k3", `{"session_id":"x","generation":1,"expected_revision":2,"reason":"r","role":"coordinator"}`, 400},
		{"trailing value", "POST", f.base + "/assignments/" + run.ID + "/block", f.peer, "k4", body + " {}", 400},
		{"unknown attempt", "POST", f.base + "/assignments/0195f000-0000-7000-8000-000000000000/block", f.peer, "k5", body, 404},
		{"stale generation", "POST", f.base + "/assignments/" + run.ID + "/block", f.peer, "k6", assignmentJSON(t, workBody(store.TeamSessionHandle{SessionID: f.recipient.SessionID, Generation: 9}, 0, 2, "waiting")), 409},
		{"wrong revision", "POST", f.base + "/assignments/" + run.ID + "/block", f.peer, "k7", assignmentJSON(t, workBody(f.recipient, 0, 7, "waiting")), 409},
		{"unsupported transition", "POST", f.base + "/assignments/" + run.ID + "/resume-work", f.peer, "k8", body, 409},
		{"invalid submission", "POST", f.base + "/assignments/" + run.ID + "/submit", f.peer, "k9", `{"session_id":"x","generation":1,"expected_revision":2,"base_commit":"short","commit":"short","summary":"s","validation":"v","evidence_refs":[]}`, 400},
		{"admin reserved read", "GET", f.reservedURL(f.recipient), f.admin, "", "", 403},
		{"no credential", "GET", f.reservedURL(f.recipient), "", "", "", 401},
	} {
		w := taskReq(t, f.h, tc.method, tc.path, tc.token, tc.key, tc.body)
		if w.Code != tc.want {
			t.Fatal(tc.name, w.Code, w.Body)
		}
		for _, field := range []string{`"token_id"`, `"user_id"`} {
			if strings.Contains(w.Body.String(), field) {
				t.Fatal(tc.name, "internal identity in refusal", field)
			}
		}
	}
	// A revision conflict carries the current task, not just an error.
	w := f.execute(t, run.ID, "block", f.peer, "k10", workBody(f.recipient, 0, 7, "waiting"))
	var conflict struct {
		Error   string     `json:"error"`
		Current store.Task `json:"current"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil || w.Code != 409 || conflict.Current.ID != f.task.ID || conflict.Current.Coordination == nil {
		t.Fatal(w.Code, w.Body, err)
	}
	// Nothing above changed the attempt.
	if got := assignmentResult(t, taskReq(t, f.h, "GET", f.reservedURL(f.recipient), f.peer, "", ""), 200); got.State != "RUNNING" {
		t.Fatal(got)
	}
}
