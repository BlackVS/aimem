package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/store"
)

func (f *assignmentFixture) joinWorker(t *testing.T, token, key string) store.TeamSessionHandle {
	t.Helper()
	w := taskReq(t, f.h, "POST", f.base+"/join", token, key, `{"role":"worker","profile":{"label":"agent","platform":"fixture","platform_version":"1"}}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Session teamMemberView `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return store.TeamSessionHandle{SessionID: out.Session.ID, Generation: out.Session.Generation}
}

func sessionView(t *testing.T, w *httptest.ResponseRecorder) teamMemberView {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Version int            `json:"protocol_version"`
		Session teamMemberView `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Version != 1 || out.Session.ID == "" {
		t.Fatal(w.Body, err)
	}
	for _, field := range []string{`"token_id"`, `"user_id"`, `"actor"`} {
		if strings.Contains(w.Body.String(), field) {
			t.Fatal("internal identity in response", field)
		}
	}
	return out.Session
}

func joinedSession(t *testing.T, w *httptest.ResponseRecorder) teamMemberView {
	t.Helper()
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Session teamMemberView `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Session.ID == "" {
		t.Fatal(w.Body, err)
	}
	return out.Session
}

func managedTaskResult(t *testing.T, w *httptest.ResponseRecorder) store.Task {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Version int        `json:"protocol_version"`
		Task    store.Task `json:"task"`
		Ready   *bool      `json:"workflow_ready"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Version != 1 || out.Ready == nil || *out.Ready || out.Task.ID == "" {
		t.Fatal(w.Body, err)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Header())
	}
	for _, field := range []string{`"token_id"`, `"user_id"`, `"actor"`} {
		if strings.Contains(w.Body.String(), field) {
			t.Fatal("internal identity in response", field)
		}
	}
	return out.Task
}

func handoffBody(from store.TeamSessionHandle, coordinatorGeneration int64, to store.TeamSessionHandle) store.TeamHandoffCommand {
	return store.TeamHandoffCommand{TeamSessionHandle: from, CoordinatorGeneration: coordinatorGeneration, Target: to, Reason: "Shift ends"}
}

func withReconciliation(c store.TeamHandoffCommand) store.TeamHandoffCommand {
	c.Reconciliation = &store.TeamHandoffReconciliation{CoordinatorStopped: true, LivenessCheck: "Backend session closed; no pending command", EvidenceRefs: []store.TaskRef{{Kind: "text", Ref: "Operator checked the host"}}}
	return c
}

func TestManagementHTTPHandoff(t *testing.T) {
	f := newAssignmentFixture(t)
	run := f.running(t)
	successor := f.joinWorker(t, f.alice, "successor")
	// Reconciliation is refused from the coordinator; a legacy writer token has no path.
	if w := taskReq(t, f.h, "POST", f.base+"/handoff", f.alice, "bad", assignmentJSON(t, withReconciliation(handoffBody(f.sender, 1, successor)))); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "POST", f.base+"/handoff", f.writer, "legacy", assignmentJSON(t, handoffBody(f.sender, 1, successor))); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	// A worker cannot hand off; the coordinator transfers to the idle designated session.
	if w := taskReq(t, f.h, "POST", f.base+"/handoff", f.peer, "worker", assignmentJSON(t, handoffBody(f.recipient, 1, successor))); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	to := sessionView(t, taskReq(t, f.h, "POST", f.base+"/handoff", f.alice, "handoff", assignmentJSON(t, handoffBody(f.sender, 1, successor))))
	if to.ID != successor.SessionID || to.Role != "coordinator" || to.CoordinatorGeneration != 2 || to.Generation != 1 {
		t.Fatalf("handoff: %+v", to)
	}
	if again := sessionView(t, taskReq(t, f.h, "POST", f.base+"/handoff", f.alice, "handoff", assignmentJSON(t, handoffBody(f.sender, 1, successor)))); again.ID != to.ID {
		t.Fatal("replay", again)
	}
	// The old coordinator handle is stale; the successor commands the surviving attempt.
	if w := f.offerRequest(t, "stale-offer"); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.execute(t, run.ID, "cancel", f.alice, "old-cancel", workBody(f.sender, 1, 2, "stop")); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	next := store.TeamSessionHandle{SessionID: to.ID, Generation: to.Generation}
	if got := assignmentResult(t, f.execute(t, run.ID, "cancel", f.alice, "new-cancel", workBody(next, 2, 2, "stop")), 200); got.State != "STOP_REQUESTED" {
		t.Fatal(got)
	}
	// An admin transfers a lost coordinator with reconciliation; without it the request is invalid.
	third := f.joinWorker(t, f.alice, "third")
	if w := taskReq(t, f.h, "POST", f.base+"/handoff", f.admin, "admin-bare", assignmentJSON(t, handoffBody(next, 2, third))); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	admin := sessionView(t, taskReq(t, f.h, "POST", f.base+"/handoff", f.admin, "admin-handoff", assignmentJSON(t, withReconciliation(handoffBody(next, 2, third)))))
	if admin.ID != third.SessionID || admin.Role != "coordinator" || admin.CoordinatorGeneration != 3 {
		t.Fatalf("admin handoff: %+v", admin)
	}
	// The audit records the admin actor for that transfer.
	w := taskReq(t, f.h, "GET", f.base+"/events?limit=100", f.admin, "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var events struct {
		Events []store.TeamEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, e := range events.Events {
		if e.Operation == "team.coordinator.handoff" {
			kinds[e.Handoff.To.SessionID] = e.Actor.Kind
		}
	}
	if kinds[successor.SessionID] != "user" || kinds[third.SessionID] != "admin" {
		t.Fatal(kinds)
	}
}

func TestManagementHTTPEditAndFinalize(t *testing.T) {
	f := newAssignmentFixture(t)
	// Edit between attempts: offer then withdraw leaves a managed READY task.
	offer := assignmentResult(t, f.offerRequest(t, "offer"), 201)
	assignmentResult(t, f.command(t, offer.ID, "withdraw", f.alice, "withdraw", store.TeamAssignmentCommand{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, Reason: "reassess"}), 200)
	task := f.taskState(t)
	content := task.TaskContent
	content.Title = "Re-scoped bounded change"
	edit := store.TeamManagedEditCommand{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, ExpectedRevision: task.Revision, Content: content, Reason: "Scope refined"}
	if w := taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/edit", f.admin, "admin-edit", assignmentJSON(t, edit)); w.Code != 403 {
		t.Fatal("admin edited a managed task", w.Code, w.Body)
	}
	edited := managedTaskResult(t, taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/edit", f.alice, "edit", assignmentJSON(t, edit)))
	if edited.Title != content.Title || edited.Revision != task.Revision+1 || edited.State != "READY" || edited.Coordination == nil {
		t.Fatal(edited)
	}
	bad := edit
	bad.ExpectedRevision, bad.Content.State = edited.Revision, "DONE"
	if w := taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/edit", f.alice, "state", assignmentJSON(t, bad)); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "PUT", "/v1/tasks/"+task.ID, f.alice, "generic", assignmentJSON(t, struct {
		store.TaskContent
		ExpectedRevision int64 `json:"expected_revision"`
	}{edited.TaskContent, edited.Revision})); w.Code != 409 || !strings.Contains(w.Body.String(), "managed_task") {
		t.Fatal(w.Code, w.Body)
	}
	// A reserved attempt refuses edits; finalize needs acceptance and evidence.
	f.offer.ExpectedRevision = edited.Revision
	run := f.running(t)
	edit.ExpectedRevision = edited.Revision
	if w := taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/edit", f.alice, "reserved", assignmentJSON(t, edit)); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	submitted := assignmentResult(t, f.execute(t, run.ID, "submit", f.peer, "submit", submission(f.recipient, edited.Revision+1)), 200)
	finalize := store.TeamFinalizeCommand{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, ExpectedRevision: edited.Revision + 2, AttemptID: run.ID, Reason: "Merged by the owner", Evidence: []store.TaskRef{{Kind: "commit", Ref: "https://example.invalid/repo/commit/" + strings.Repeat("c", 40)}}}
	if w := taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/finalize", f.alice, "early", assignmentJSON(t, finalize)); w.Code != 409 {
		t.Fatal("finalized before acceptance", w.Code, w.Body)
	}
	decision := store.TeamResultDecision{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, ExpectedRevision: edited.Revision + 2, ResultID: submitted.Result.ID, Decision: "accept", Reason: "Evidence assessed"}
	assignmentResult(t, f.execute(t, run.ID, "review", f.alice, "review", decision), 200)
	finalize.ExpectedRevision = edited.Revision + 3
	noEvidence := finalize
	noEvidence.Evidence = nil
	if w := taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/finalize", f.alice, "no-evidence", assignmentJSON(t, noEvidence)); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	done := managedTaskResult(t, taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/finalize", f.alice, "finalize", assignmentJSON(t, finalize)))
	if done.State != "DONE" || done.Revision != finalize.ExpectedRevision+1 || len(done.EvidenceRefs) == 0 || done.Coordination == nil {
		t.Fatal(done)
	}
	if replay := managedTaskResult(t, taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/finalize", f.alice, "finalize", assignmentJSON(t, finalize))); replay.Revision != done.Revision {
		t.Fatal("replay", replay)
	}
}

func TestManagementHTTPAdminRecoverAndUnmanage(t *testing.T) {
	f := newAssignmentFixture(t)
	run := f.running(t)
	recover := store.TeamRecoveryCommand{ExpectedRevision: 2, ExpectedWorker: f.recipient, ExpectedSessionGeneration: 1, ExpectedCoordinatorGeneration: 1,
		Reconciliation: store.TeamRecoveryEvidence{ExecutionStopped: true, Reason: "Worker host lost", RuntimeCheck: "No surviving process", WorktreeCheck: "Worktree preserved for review", EvidenceRefs: []store.TaskRef{{Kind: "text", Ref: "Operator checked the host"}}}}
	path := f.base + "/assignments/" + run.ID + "/recover"
	// The gate refuses ordinary tokens before any handler runs; storage refuses bad evidence.
	if w := taskReq(t, f.h, "POST", path, f.alice, "ordinary", assignmentJSON(t, recover)); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	bare := recover
	bare.Reconciliation.ExecutionStopped = false
	if w := taskReq(t, f.h, "POST", path, f.admin, "bare", assignmentJSON(t, bare)); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	recovered := assignmentResult(t, taskReq(t, f.h, "POST", path, f.admin, "recover", assignmentJSON(t, recover)), 200)
	task := f.taskState(t)
	if recovered.State != "RECOVERED" || recovered.Recovery == nil || task.State != "READY" || task.Coordination == nil || task.Coordination.AttemptID != "" {
		t.Fatal(recovered, task)
	}
	if w := taskReq(t, f.h, "GET", f.reservedURL(f.recipient), f.peer, "", ""); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	// Unmanage: another team's prefix is refused; ordinary tokens never reach it.
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/teams", f.admin, "other-team", fmt.Sprintf(`{"name":"Other","enrollment":[{"user_id":%q,"coordinator":true},{"user_id":%q}]}`, f.aliceUser, f.bobUser))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var other store.Team
	if err := json.Unmarshal(w.Body.Bytes(), &other); err != nil {
		t.Fatal(err)
	}
	unmanage := store.TeamUnmanageCommand{ExpectedRevision: task.Revision, Reason: "Back to the ordinary backlog"}
	if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/teams/"+other.ID+"/tasks/"+task.ID+"/unmanage", f.admin, "wrong-team", assignmentJSON(t, unmanage)); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/unmanage", f.alice, "ordinary-unmanage", assignmentJSON(t, unmanage)); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	released := managedTaskResult(t, taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/unmanage", f.admin, "unmanage", assignmentJSON(t, unmanage)))
	if released.Coordination != nil || released.Revision != task.Revision+1 {
		t.Fatal(released)
	}
	if replay := managedTaskResult(t, taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/unmanage", f.admin, "unmanage", assignmentJSON(t, unmanage))); replay.Revision != released.Revision {
		t.Fatal("replay", replay)
	}
	// Generic writes work again for the ordinary writer.
	body := assignmentJSON(t, struct {
		store.TaskContent
		ExpectedRevision int64 `json:"expected_revision"`
	}{released.TaskContent, released.Revision})
	if w := taskReq(t, f.h, "PUT", "/v1/tasks/"+task.ID, f.alice, "generic", body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	// The other team takes the task over (offer and withdraw leave the revision
	// unchanged). The first team's retry still replays its own receipt; a fresh
	// release through the first team is refused; the managing team may release.
	otherBase := "/v1/projects/alpha/teams/" + other.ID
	otherCo := joinedSession(t, taskReq(t, f.h, "POST", otherBase+"/join", f.alice, "other-co", `{"role":"coordinator","profile":{"label":"agent","platform":"fixture","platform_version":"1"}}`))
	otherWorker := joinedSession(t, taskReq(t, f.h, "POST", otherBase+"/join", f.peer, "other-worker", `{"role":"worker","profile":{"label":"agent","platform":"fixture","platform_version":"1"}}`))
	current := f.taskState(t)
	takeover := store.TeamOffer{TeamSessionHandle: store.TeamSessionHandle{SessionID: otherCo.ID, Generation: otherCo.Generation}, CoordinatorGeneration: otherCo.CoordinatorGeneration, TaskID: task.ID, ExpectedRevision: current.Revision, Worker: store.TeamSessionHandle{SessionID: otherWorker.ID, Generation: otherWorker.Generation}, SuitabilityRationale: "S task; fit confirmed", CostRationale: "Least costly suitable member"}
	taken := assignmentResult(t, taskReq(t, f.h, "POST", otherBase+"/assignments", f.alice, "take-over", assignmentJSON(t, takeover)), 201)
	assignmentResult(t, taskReq(t, f.h, "POST", otherBase+"/assignments/"+taken.ID+"/withdraw", f.alice, "take-back", assignmentJSON(t, store.TeamAssignmentCommand{TeamSessionHandle: takeover.TeamSessionHandle, CoordinatorGeneration: takeover.CoordinatorGeneration, Reason: "reassess"})), 200)
	if again := f.taskState(t); again.Revision != current.Revision || again.Coordination == nil || again.Coordination.TeamID != other.ID {
		t.Fatal("takeover fixture", again)
	}
	if replay := managedTaskResult(t, taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/unmanage", f.admin, "unmanage", assignmentJSON(t, unmanage))); replay.Revision != released.Revision {
		t.Fatal("replay after takeover", replay)
	}
	fresh := store.TeamUnmanageCommand{ExpectedRevision: current.Revision, Reason: "again"}
	if w := taskReq(t, f.h, "POST", f.base+"/tasks/"+task.ID+"/unmanage", f.admin, "fresh", assignmentJSON(t, fresh)); w.Code != 409 {
		t.Fatal("released another team's task", w.Code, w.Body)
	}
	if got := managedTaskResult(t, taskReq(t, f.h, "POST", otherBase+"/tasks/"+task.ID+"/unmanage", f.admin, "other-release", assignmentJSON(t, fresh))); got.Coordination != nil || got.Revision != current.Revision+1 {
		t.Fatal(got)
	}
	// Malformed query encodings are refused on session-scoped reads.
	if w := taskReq(t, f.h, "GET", f.reservedURL(f.recipient)+"&extra=%ZZ", f.peer, "", ""); w.Code != 400 {
		t.Fatal("malformed query accepted", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", f.attemptURL(run.ID, f.recipient)+"&extra=%ZZ", f.peer, "", ""); w.Code != 400 {
		t.Fatal("malformed query accepted", w.Code, w.Body)
	}
}
