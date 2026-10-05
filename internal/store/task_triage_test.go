package store

import (
	"errors"
	"testing"
)

func strp(s string) *string { return &s }

// A reused key with a changed patch is refused, even when the change is an
// empty next_action that the receipt digest would otherwise drop; an
// identical retry replays the first result.
func TestTriageReceiptKeepsFieldPresence(t *testing.T) {
	_, db := taskDB(t)
	task, err := db.CreateTask(TaskContent{Title: "triage me", State: "BACKLOG", NextAction: "assess"}, aliceActor, "tri-create")
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.TriageTask(task.ID, TaskTriage{State: strp("READY")}, task.Revision, aliceActor, "tri-key")
	if err != nil || first.State != "READY" || first.NextAction != "assess" {
		t.Fatalf("first triage: %+v %v", first, err)
	}
	// The same key with next_action named, even as empty, is another request.
	if _, err := db.TriageTask(task.ID, TaskTriage{State: strp("READY"), NextAction: strp("")}, task.Revision, aliceActor, "tri-key"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatalf("changed patch under a used key: %v", err)
	}
	// The identical request replays the recorded result.
	again, err := db.TriageTask(task.ID, TaskTriage{State: strp("READY")}, task.Revision, aliceActor, "tri-key")
	if err != nil || again.Revision != first.Revision || again.NextAction != "assess" {
		t.Fatalf("identical retry: %+v %v", again, err)
	}
	// Clearing next_action under a new key is a write of its own.
	cleared, err := db.TriageTask(task.ID, TaskTriage{NextAction: strp("")}, first.Revision, aliceActor, "tri-clear")
	if err != nil || cleared.NextAction != "" || cleared.Revision != first.Revision+1 {
		t.Fatalf("clearing: %+v %v", cleared, err)
	}
	// And its own identical retry replays too.
	if r, err := db.TriageTask(task.ID, TaskTriage{NextAction: strp("")}, first.Revision, aliceActor, "tri-clear"); err != nil || r.Revision != cleared.Revision {
		t.Fatalf("clearing retry: %+v %v", r, err)
	}
}
