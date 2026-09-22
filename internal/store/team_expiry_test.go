package store

import (
	"errors"
	"testing"
)

// The playbook's expiry fallback for accepted work is executable: a BLOCKED
// attempt is released by the coordinator's cancel, the worker's stopped and
// the coordinator's close-stop, returning the task to READY; decline is
// refused once an offer has been accepted.
func TestTeamEscalationExpiryReleasesAcceptedWork(t *testing.T) {
	_, d, team, a, _, c, run := runningResultFixture(t)
	revision := func() int64 {
		t.Helper()
		task, err := d.GetTask(c.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		return task.Revision
	}
	if got, err := d.ChangeTeamWork(team.ID, run.ID, "block", workCommand(c, "block", revision()), a, "block"); err != nil || got.State != "BLOCKED" {
		t.Fatal(got, err)
	}
	if _, err := d.ChangeTeamAssignment(team.ID, run.ID, "decline", TeamAssignmentCommand{TeamSessionHandle: c.Worker, Reason: "escalation expired"}, a, "decline", allowAssignmentWorker); !errors.Is(err, ErrTeamAssignmentConflict) {
		t.Fatal("decline of accepted work was not refused:", err)
	}
	for _, step := range []struct{ op, state, task string }{{"cancel", "STOP_REQUESTED", "BLOCKED"}, {"stopped", "STOPPED", "BLOCKED"}, {"close-stop", "CANCELLED", "READY"}} {
		got, err := d.ChangeTeamWork(team.ID, run.ID, step.op, workCommand(c, step.op, revision()), a, step.op)
		if err != nil || got.State != step.state {
			t.Fatalf("%s: %+v %v", step.op, got, err)
		}
		task, err := d.GetTask(c.TaskID)
		if err != nil || task.State != step.task {
			t.Fatalf("%s: task %s %v", step.op, task.State, err)
		}
	}
	// The released task is offerable again.
	c.ExpectedRevision = revision()
	if _, err := d.OfferTeamAssignment(team.ID, c, a, "offer-again", allowAssignmentWorker); err != nil {
		t.Fatal("released task not offerable:", err)
	}
}
