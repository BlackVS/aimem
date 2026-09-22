package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aimem/internal/store"
)

// Assignment commands are intentionally not routed yet. Seed through the
// storage boundary, then exercise the existing HTTP task surface and real
// access-store callback; this protects old clients as that boundary is added.
func TestManagedTaskHTTPGuardAndProjection(t *testing.T) {
	f := newMessageFixture(t)
	d, err := f.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	a := store.TeamAuditContext{Actor: store.TaskActor{Kind: "user", UserID: f.aliceUser, TokenID: f.aliceTokenID, Name: "coordinator"}, RequestID: "guard-test", ServerVersion: "test"}
	task, err := d.CreateTask(store.TaskContent{Title: "managed", State: "READY"}, a.Actor, "task")
	if err != nil {
		t.Fatal(err)
	}
	// Even a previously accepted generic update's receipt cannot bypass the guard.
	edit := fmt.Sprintf(`{"title":"managed","state":"READY","expected_revision":%d}`, task.Revision)
	w := taskReq(t, f.h, "PUT", "/v1/tasks/"+task.ID, f.alice, "old-update", edit)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	task, err = d.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := f.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	check := func(worker store.TeamSession) error {
		ok, err := acc.CanWriteToken(worker.UserID, worker.TokenID, f.alphaInstance)
		if err != nil {
			return err
		}
		if !ok {
			return store.ErrTeamSessionDenied
		}
		return nil
	}
	teamID := f.base[strings.LastIndex(f.base, "/")+1:]
	c := store.TeamOffer{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, TaskID: task.ID, ExpectedRevision: task.Revision, Worker: f.recipient, SuitabilityRationale: "Small task; suitable declared skills confirmed", CostRationale: "Least costly suitable worker"}
	out, err := d.OfferTeamAssignment(teamID, c, a, "offer", check)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/tasks/" + task.ID, "/v1/projects/alpha/tasks?state=READY"} {
		w = taskReq(t, f.h, "GET", path, f.alice, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"coordination"`) || !strings.Contains(w.Body.String(), out.ID) {
			t.Fatal(w.Code, w.Body)
		}
	}
	for _, token := range []string{f.alice, f.admin, f.env} {
		w = taskReq(t, f.h, "PUT", "/v1/tasks/"+task.ID, token, "archive", fmt.Sprintf(`{"title":"bypass","state":"DONE","archived":true,"expected_revision":%d}`, task.Revision))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "managed_task") {
			t.Fatal(w.Code, w.Body)
		}
	}
	w = taskReq(t, f.h, "PUT", "/v1/tasks/"+task.ID, f.alice, "old-update", edit)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "managed_task") {
		t.Fatal(w.Code, w.Body)
	}
	w = taskReq(t, f.h, "PUT", "/v1/tasks/"+task.ID, f.alice, "forged", fmt.Sprintf(`{"title":"bypass","state":"READY","expected_revision":%d,"coordination":null}`, task.Revision))
	if w.Code != 400 {
		t.Fatal("projection became editable", w.Code, w.Body)
	}
	// Removing the offered worker's real grant prevents acceptance, while the
	// coordinator can still withdraw the offer and preserve management.
	if err = acc.SetGrant("admin", f.alphaInstance, "user", f.bobUser, false); err != nil {
		t.Fatal(err)
	}
	id, err := acc.Authenticate(f.peer)
	if err != nil {
		t.Fatal(err)
	}
	workerAudit := a
	workerAudit.Actor = store.TaskActor{Kind: "user", UserID: f.bobUser, TokenID: id.TokenID, Name: "worker"}
	if _, err = d.ChangeTeamAssignment(teamID, out.ID, "accept", store.TeamAssignmentCommand{TeamSessionHandle: f.recipient}, workerAudit, "accept", check); err == nil {
		t.Fatal("accepted revoked grant")
	}
	if _, err = d.ChangeTeamAssignment(teamID, out.ID, "withdraw", store.TeamAssignmentCommand{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, Reason: "worker grant removed"}, a, "withdraw", nil); err != nil {
		t.Fatal(err)
	}
	w = taskReq(t, f.h, "GET", "/v1/tasks/"+task.ID, f.alice, "", "")
	var view store.Task
	if err = json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.Coordination == nil || view.Coordination.TeamID != teamID || view.Coordination.AttemptID != "" {
		t.Fatal(view, err)
	}
	// Capability advertisement must not suggest a complete execution workflow.
	for _, op := range teamSessionOperations {
		if op == "offer" || op == "accept" {
			t.Fatal("premature assignment capability")
		}
	}
}
