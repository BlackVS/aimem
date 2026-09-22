package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

// acceptedFixture drives one attempt to ACCEPTED: task REVIEW at revision 4,
// no reserved attempt, still managed.
func acceptedFixture(t *testing.T) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext, TeamOffer, TeamAssignment) {
	t.Helper()
	r, d, team, a, admin, c, run := runningResultFixture(t)
	out, err := d.SubmitTeamResult(team.ID, run.ID, resultSubmission(c), a, "submit")
	if err != nil {
		t.Fatal(err)
	}
	out, err = d.ReviewTeamResult(team.ID, out.ID, resultDecision(c, out, "accept"), a, "review")
	if err != nil || out.State != "ACCEPTED" {
		t.Fatal(out, err)
	}
	return r, d, team, a, admin, c, out
}

func editCommand(c TeamOffer, task Task) TeamManagedEditCommand {
	content := task.TaskContent
	content.Title = "Re-scoped bounded change"
	content.NextAction = "Coordinator refined scope between attempts"
	return TeamManagedEditCommand{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: c.CoordinatorGeneration, ExpectedRevision: task.Revision, Content: content, Reason: "Scope refined after review"}
}

func finalizeCommand(c TeamOffer, task Task, attempt string) TeamFinalizeCommand {
	return TeamFinalizeCommand{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: c.CoordinatorGeneration, ExpectedRevision: task.Revision, AttemptID: attempt, Reason: "Candidate merged by the owner after review",
		Evidence: []TaskRef{{Kind: "commit", Ref: "https://example.invalid/repo/commit/" + strings.Repeat("c", 40), Note: "merge commit"}}}
}

func managedEvents(t *testing.T, d *DB, teamID, operation string) []TeamEvent {
	t.Helper()
	events, err := d.TeamEvents(teamID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := []TeamEvent{}
	for _, e := range events {
		if e.Operation == operation {
			out = append(out, e)
		}
	}
	return out
}

func TestManagedTaskEdit(t *testing.T) {
	// Rework returns the task to READY with no reserved attempt.
	_, d, team, a, _, c, run := runningResultFixture(t)
	sub, err := d.SubmitTeamResult(team.ID, run.ID, resultSubmission(c), a, "submit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReviewTeamResult(team.ID, sub.ID, resultDecision(c, sub, "rework"), a, "rework"); err != nil {
		t.Fatal(err)
	}
	task, err := d.GetTask(c.TaskID)
	if err != nil || task.State != "READY" || task.Coordination == nil || task.Coordination.AttemptID != "" {
		t.Fatal(task, err)
	}
	history, err := d.TaskHistory(task.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	cmd := editCommand(c, task)
	got, err := d.EditManagedTask(team.ID, task.ID, cmd, a, "edit")
	if err != nil || got.Revision != task.Revision+1 || got.Title != cmd.Content.Title || got.State != "READY" || got.Coordination == nil || got.Coordination.TeamID != team.ID {
		t.Fatalf("edit: %+v %v", got, err)
	}
	after, err := d.TaskHistory(task.ID, 0, 50)
	if err != nil || len(after.Changes) != len(history.Changes)+1 || after.Changes[len(after.Changes)-1].Task.Title != cmd.Content.Title {
		t.Fatal("history", err)
	}
	events := managedEvents(t, d, team.ID, "team.task.edit")
	if len(events) != 1 || events[0].Managed == nil || events[0].Managed.TaskID != task.ID || events[0].Managed.PreviousRevision != task.Revision || events[0].Managed.Revision != got.Revision || events[0].Managed.PreviousState != "READY" || events[0].Managed.State != "READY" || events[0].Managed.Reason != cmd.Reason || events[0].Session == nil || events[0].Session.ID != c.SessionID || events[0].RequestID != a.RequestID {
		t.Fatalf("edit audit: %+v", events)
	}
	// Generic writes stay refused; the edit is the only path.
	if _, err := d.UpdateTask(task.ID, got.TaskContent, got.Revision, a.Actor, "generic"); !errors.Is(err, ErrManagedTask) {
		t.Fatal(err)
	}
	// Replay returns the original; changed content under the same key conflicts.
	if replay, err := d.EditManagedTask(team.ID, task.ID, cmd, a, "edit"); err != nil || !reflect.DeepEqual(replay, got) {
		t.Fatal(replay, err)
	}
	changed := cmd
	changed.Reason = "other"
	if _, err := d.EditManagedTask(team.ID, task.ID, changed, a, "edit"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatal(err)
	}
	// Stale revision conflicts with the current projection; the edited task is
	// offered again with a fresh attempt.
	if _, err := d.EditManagedTask(team.ID, task.ID, cmd, a, "stale"); err == nil {
		t.Fatal("stale revision accepted")
	} else {
		var conflict *TaskConflict
		if !errors.As(err, &conflict) || conflict.Current.Coordination == nil {
			t.Fatal(err)
		}
	}
	c.ExpectedRevision = got.Revision
	next := mustOffer(t, d, team, a, c, "re-offer")
	if next.Requirements.Title != cmd.Content.Title {
		t.Fatal("offer did not snapshot edited content", next.Requirements)
	}
}

func TestManagedTaskEditFences(t *testing.T) {
	for _, scenario := range []string{"offered", "running", "submitted", "blocked", "stale-generation", "wrong-session", "state-change", "archived-content", "unmanaged", "other-team", "empty-reason", "invalid-content", "retired-epic", "disabled", "unenrolled"} {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c := assignmentFixture(t)
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			// Manage the task through one closed attempt so it is READY between attempts.
			if scenario != "unmanaged" {
				offer := mustOffer(t, d, team, a, c, "first")
				if scenario == "offered" {
					task, _ = d.GetTask(c.TaskID)
				} else {
					if _, err := d.ChangeTeamAssignment(team.ID, offer.ID, "withdraw", TeamAssignmentCommand{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: 1, Reason: "reassess"}, a, "withdraw", nil); err != nil {
						t.Fatal(err)
					}
					task, _ = d.GetTask(c.TaskID)
				}
			}
			cmd := editCommand(c, task)
			wantInvalid := false
			switch scenario {
			case "running", "submitted", "blocked":
				offer := mustOffer(t, d, team, a, c, "second")
				run, err := d.ChangeTeamAssignment(team.ID, offer.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker)
				if err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "submitted":
					sub := resultSubmission(c)
					sub.ExpectedRevision = 2
					if _, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit"); err != nil {
						t.Fatal(err)
					}
				case "blocked":
					if _, err := d.ChangeTeamWork(team.ID, run.ID, "block", workCommand(c, "block", 2), a, "block"); err != nil {
						t.Fatal(err)
					}
				}
				task, _ = d.GetTask(c.TaskID)
				cmd = editCommand(c, task)
			case "stale-generation":
				cmd.CoordinatorGeneration++
			case "wrong-session":
				cmd.TeamSessionHandle = c.Worker
			case "state-change":
				cmd.Content.State, wantInvalid = "DONE", true
			case "archived-content":
				cmd.Content.State, cmd.Content.Archived, wantInvalid = "DONE", true, true
			case "other-team":
				other, err := r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "other", Enrollment: []TeamEnrollment{{UserID: a.Actor.UserID, Coordinator: true}}}, admin, "other")
				if err != nil {
					t.Fatal(err)
				}
				co, err := d.JoinTeam(other.ID, "coordinator", testProfile(), a, "other-co")
				if err != nil {
					t.Fatal(err)
				}
				team.ID = other.ID
				cmd.TeamSessionHandle, cmd.CoordinatorGeneration = TeamSessionHandle{co.ID, co.Generation}, co.CoordinatorGeneration
			case "empty-reason":
				cmd.Reason, wantInvalid = " ", true
			case "invalid-content":
				cmd.Content.Title, wantInvalid = "", true
			case "retired-epic":
				epic, err := d.CreateEpic(EpicContent{Title: "old", State: "RETIRED"}, a.Actor, "epic")
				if err != nil {
					t.Fatal(err)
				}
				cmd.Content.Epic, wantInvalid = epic.ID, true
			case "disabled":
				if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "unenrolled":
				content := team.TeamContent
				content.Enrollment = nil
				if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, content, admin, "unenroll"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			counts := assignmentCounts(t, d)
			_, err = d.EditManagedTask(team.ID, c.TaskID, cmd, a, "edit")
			if err == nil || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("invalid edit accepted", err)
			}
			if wantInvalid != errors.Is(err, ErrTaskInvalid) {
				t.Fatal(scenario, err)
			}
			if after, err := d.GetTask(c.TaskID); err != nil || !reflect.DeepEqual(after, before) {
				t.Fatal("task changed", after, err)
			}
		})
	}
}

func TestManagedTaskFinalize(t *testing.T) {
	_, d, team, a, _, c, accepted := acceptedFixture(t)
	task, err := d.GetTask(c.TaskID)
	if err != nil || task.State != "REVIEW" || task.Coordination.AttemptID != "" {
		t.Fatal(task, err)
	}
	cmd := finalizeCommand(c, task, accepted.ID)
	got, err := d.FinalizeManagedTask(team.ID, task.ID, cmd, a, "finalize")
	if err != nil || got.State != "DONE" || got.Revision != task.Revision+1 || len(got.EvidenceRefs) != len(task.EvidenceRefs)+1 || got.EvidenceRefs[len(got.EvidenceRefs)-1] != cmd.Evidence[0] || got.Coordination == nil || got.Coordination.TeamID != team.ID {
		t.Fatalf("finalize: %+v %v", got, err)
	}
	content := got.TaskContent
	content.State, content.EvidenceRefs = task.State, task.EvidenceRefs
	if !reflect.DeepEqual(content, task.TaskContent) {
		t.Fatal("finalize changed unrelated content")
	}
	events := managedEvents(t, d, team.ID, "team.task.finalize")
	if len(events) != 1 || events[0].Managed.AttemptID != accepted.ID || events[0].Managed.PreviousState != "REVIEW" || events[0].Managed.State != "DONE" || !reflect.DeepEqual(events[0].Managed.Evidence, cmd.Evidence) || events[0].Session.ID != c.SessionID {
		t.Fatalf("finalize audit: %+v", events)
	}
	// Still managed: generic writes refused, no new offer for a DONE task,
	// the accepted attempt is unchanged and readable.
	if _, err := d.UpdateTask(task.ID, got.TaskContent, got.Revision, a.Actor, "generic"); !errors.Is(err, ErrManagedTask) {
		t.Fatal(err)
	}
	c.ExpectedRevision = got.Revision
	if _, err := d.OfferTeamAssignment(team.ID, c, a, "offer-done", allowAssignmentWorker); !errors.Is(err, ErrTeamAssignmentConflict) {
		t.Fatal(err)
	}
	if again, err := d.GetTeamAssignment(team.ID, accepted.ID, c.TeamSessionHandle, a.Actor); err != nil || !reflect.DeepEqual(again, accepted) {
		t.Fatal(again, err)
	}
	if replay, err := d.FinalizeManagedTask(team.ID, task.ID, cmd, a, "finalize"); err != nil || !reflect.DeepEqual(replay, got) || len(managedEvents(t, d, team.ID, "team.task.finalize")) != 1 {
		t.Fatal(replay, err)
	}
	if _, err := d.FinalizeManagedTask(team.ID, task.ID, cmd, a, "again"); err == nil {
		t.Fatal("finalized twice")
	}
}

func TestManagedTaskFinalizeFences(t *testing.T) {
	for _, scenario := range []string{"submitted", "returned", "running", "wrong-attempt", "missing-attempt", "no-evidence", "bad-evidence", "empty-reason", "stale-generation", "wrong-session", "revision", "unmanaged", "disabled", "evidence-overflow"} {
		t.Run(scenario, func(t *testing.T) {
			var d *DB
			var team Team
			var a TeamAuditContext
			var c TeamOffer
			var attempt TeamAssignment
			wantInvalid := false
			switch scenario {
			case "submitted", "running":
				_, d, team, a, _, c, attempt = runningResultFixture(t)
				if scenario == "submitted" {
					var err error
					if attempt, err = d.SubmitTeamResult(team.ID, attempt.ID, resultSubmission(c), a, "submit"); err != nil {
						t.Fatal(err)
					}
				}
			case "returned":
				_, d, team, a, _, c, attempt = runningResultFixture(t)
				sub, err := d.SubmitTeamResult(team.ID, attempt.ID, resultSubmission(c), a, "submit")
				if err != nil {
					t.Fatal(err)
				}
				if attempt, err = d.ReviewTeamResult(team.ID, sub.ID, resultDecision(c, sub, "rework"), a, "rework"); err != nil {
					t.Fatal(err)
				}
			case "unmanaged":
				_, d, team, a, _, c = assignmentFixture(t)
				attempt.ID = uuidv7.New()
			default:
				_, d, team, a, _, c, attempt = acceptedFixture(t)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			cmd := finalizeCommand(c, task, attempt.ID)
			switch scenario {
			case "wrong-attempt":
				other, err := d.CreateTask(TaskContent{Title: "other", State: "READY"}, a.Actor, "other")
				if err != nil {
					t.Fatal(err)
				}
				offer := c
				offer.TaskID, offer.ExpectedRevision = other.ID, other.Revision
				offer.Worker = messageMember(t, d, team, a, "spare")
				cmd.AttemptID = mustOffer(t, d, team, a, offer, "spare-offer").ID
			case "missing-attempt":
				cmd.AttemptID = uuidv7.New()
			case "no-evidence":
				cmd.Evidence, wantInvalid = nil, true
			case "bad-evidence":
				cmd.Evidence[0], wantInvalid = TaskRef{Kind: "ci", Ref: "invalid"}, true
			case "empty-reason":
				cmd.Reason, wantInvalid = "", true
			case "stale-generation":
				cmd.CoordinatorGeneration++
			case "wrong-session":
				cmd.TeamSessionHandle = c.Worker
			case "revision":
				cmd.ExpectedRevision++
			case "disabled":
				if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "evidence-overflow":
				cmd.Evidence, wantInvalid = make([]TaskRef, MaxTaskListEntries+1), true
				for i := range cmd.Evidence {
					cmd.Evidence[i] = TaskRef{Kind: "text", Ref: fmt.Sprint("evidence ", i)}
				}
			}
			counts := assignmentCounts(t, d)
			_, err = d.FinalizeManagedTask(team.ID, c.TaskID, cmd, a, "finalize")
			if err == nil || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("invalid finalize accepted", err)
			}
			if wantInvalid != errors.Is(err, ErrTaskInvalid) {
				t.Fatal(scenario, err)
			}
			if after, err := d.GetTask(c.TaskID); err != nil || !reflect.DeepEqual(after, task) {
				t.Fatal("task changed", after, err)
			}
		})
	}
}

func TestManagedTaskUnmanage(t *testing.T) {
	_, d, team, a, admin, c, accepted := acceptedFixture(t)
	task, err := d.GetTask(c.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := TeamUnmanageCommand{ExpectedRevision: task.Revision, Reason: "Owner takes the task back to the ordinary backlog"}
	got, err := d.UnmanageTask(task.ID, cmd, admin, "unmanage")
	if err != nil || got.Coordination != nil || got.Revision != task.Revision+1 || got.State != task.State {
		t.Fatalf("unmanage: %+v %v", got, err)
	}
	content := got.TaskContent
	if !reflect.DeepEqual(content, task.TaskContent) {
		t.Fatal("unmanage changed content")
	}
	events := managedEvents(t, d, team.ID, "team.task.unmanage")
	if len(events) != 1 || events[0].Managed.TaskID != task.ID || events[0].Managed.PreviousRevision != task.Revision || events[0].Managed.Revision != got.Revision || events[0].Session != nil || events[0].Actor.Kind != "admin" || events[0].Managed.Reason != cmd.Reason {
		t.Fatalf("unmanage audit: %+v", events)
	}
	// Generic writes work again; the projection is gone; old attempts stay readable.
	if read, err := d.GetTask(task.ID); err != nil || read.Coordination != nil {
		t.Fatal(read, err)
	}
	updated, err := d.UpdateTask(task.ID, got.TaskContent, got.Revision, a.Actor, "generic")
	if err != nil || updated.Revision != got.Revision+1 {
		t.Fatal(updated, err)
	}
	if again, err := d.GetTeamAssignment(team.ID, accepted.ID, c.TeamSessionHandle, a.Actor); err != nil || !reflect.DeepEqual(again, accepted) {
		t.Fatal(again, err)
	}
	// Coordinator operations no longer apply to a released task.
	if _, err := d.FinalizeManagedTask(team.ID, task.ID, finalizeCommand(c, updated, accepted.ID), a, "late-finalize"); !errors.Is(err, ErrTeamAssignmentConflict) {
		t.Fatal(err)
	}
	if _, err := d.UnmanageTask(task.ID, TeamUnmanageCommand{ExpectedRevision: updated.Revision, Reason: "twice"}, admin, "twice"); !errors.Is(err, ErrTeamAssignmentConflict) {
		t.Fatal(err)
	}
	if replay, err := d.UnmanageTask(task.ID, cmd, admin, "unmanage"); err != nil || !reflect.DeepEqual(replay, got) {
		t.Fatal(replay, err)
	}
	// A fresh offer manages it again with a new attempt; the old one is intact.
	content.State = "READY"
	ready, err := d.UpdateTask(task.ID, content, updated.Revision, a.Actor, "ready")
	if err != nil {
		t.Fatal(err)
	}
	c.ExpectedRevision = ready.Revision
	c.Worker = messageMember(t, d, team, a, "fresh-worker")
	next := mustOffer(t, d, team, a, c, "re-manage")
	read, err := d.GetTask(task.ID)
	if err != nil || read.Coordination == nil || read.Coordination.TeamID != team.ID || read.Coordination.AttemptID != next.ID {
		t.Fatal(read, err)
	}
	if _, err := d.UpdateTask(task.ID, read.TaskContent, read.Revision, a.Actor, "generic-again"); !errors.Is(err, ErrManagedTask) {
		t.Fatal(err)
	}
	if again, err := d.GetTeamAssignment(team.ID, accepted.ID, c.TeamSessionHandle, a.Actor); err != nil || !reflect.DeepEqual(again, accepted) {
		t.Fatal(again, err)
	}
}

func TestManagedTaskUnmanageFences(t *testing.T) {
	for _, scenario := range []string{"ordinary", "forged-admin", "not-managed", "revision", "disabled", "empty-reason", "missing-task"} {
		t.Run(scenario, func(t *testing.T) {
			_, d, _, a, admin, c, _ := acceptedFixture(t)
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			cmd, actor, id, wantInvalid := TeamUnmanageCommand{ExpectedRevision: task.Revision, Reason: "release"}, admin, task.ID, false
			switch scenario {
			case "ordinary":
				actor = a
			case "forged-admin":
				actor.Actor.UserID = a.Actor.UserID
			case "not-managed":
				plain, err := d.CreateTask(TaskContent{Title: "plain", State: "READY"}, a.Actor, "plain")
				if err != nil {
					t.Fatal(err)
				}
				id, cmd.ExpectedRevision = plain.ID, plain.Revision
			case "revision":
				cmd.ExpectedRevision++
			case "disabled":
				if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "empty-reason":
				cmd.Reason, wantInvalid = "", true
			case "missing-task":
				id = uuidv7.New()
			}
			counts := assignmentCounts(t, d)
			_, err = d.UnmanageTask(id, cmd, actor, "unmanage")
			if err == nil || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("invalid unmanage accepted", err)
			}
			if wantInvalid != errors.Is(err, ErrTaskInvalid) {
				t.Fatal(scenario, err)
			}
			if after, err := d.GetTask(task.ID); err != nil || !reflect.DeepEqual(after, task) {
				t.Fatal("task changed", after, err)
			}
		})
	}
}

func TestManagedTaskUnmanageReservedStates(t *testing.T) {
	for _, state := range []string{"OFFERED", "RUNNING", "BLOCKED", "STOP_REQUESTED", "STOPPED", "SUBMITTED"} {
		t.Run(state, func(t *testing.T) {
			var d *DB
			var admin TeamAuditContext
			var c TeamOffer
			if state == "OFFERED" {
				var team Team
				var a TeamAuditContext
				_, d, team, a, admin, c = assignmentFixture(t)
				mustOffer(t, d, team, a, c, "offer")
			} else {
				_, d, _, _, admin, c, _, _ = rebindFixture(t, state)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil || task.Coordination.AttemptID == "" {
				t.Fatal(task, err)
			}
			if _, err := d.UnmanageTask(task.ID, TeamUnmanageCommand{ExpectedRevision: task.Revision, Reason: "release"}, admin, "unmanage"); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal(state, err)
			}
		})
	}
}

func TestManagedTaskRollback(t *testing.T) {
	for _, op := range []string{"edit", "finalize", "unmanage"} {
		for _, table := range []string{"tasks", "task_history", "team_events", "task_requests", "team_managed_tasks"} {
			if table == "team_managed_tasks" && op != "unmanage" {
				continue
			}
			t.Run(op+"/"+table, func(t *testing.T) {
				_, d, team, a, admin, c, accepted := acceptedFixture(t)
				task, err := d.GetTask(c.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				if op == "edit" {
					// Edit needs a non-REVIEW managed task; rework returns it to READY.
					_, d, team, a, admin, c, accepted = runningResultFixture(t)
					sub, err := d.SubmitTeamResult(team.ID, accepted.ID, resultSubmission(c), a, "submit")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := d.ReviewTeamResult(team.ID, sub.ID, resultDecision(c, sub, "rework"), a, "rework"); err != nil {
						t.Fatal(err)
					}
					task, _ = d.GetTask(c.TaskID)
				}
				run := func(key string) error {
					var err error
					switch op {
					case "edit":
						_, err = d.EditManagedTask(team.ID, task.ID, editCommand(c, task), a, key)
					case "finalize":
						_, err = d.FinalizeManagedTask(team.ID, task.ID, finalizeCommand(c, task, accepted.ID), a, key)
					case "unmanage":
						_, err = d.UnmanageTask(task.ID, TeamUnmanageCommand{ExpectedRevision: task.Revision, Reason: "release"}, admin, key)
					}
					return err
				}
				counts := assignmentCounts(t, d)
				verb := "INSERT"
				if table == "tasks" || table == "team_managed_tasks" {
					verb = "UPDATE"
				}
				if _, err := d.sql.Exec(`CREATE TRIGGER fail_managed BEFORE ` + verb + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				if err := run("effect"); err == nil {
					t.Fatal("injection did not fail")
				}
				if after, err := d.GetTask(task.ID); err != nil || !reflect.DeepEqual(after, task) || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
					t.Fatal("partial write", after, err)
				}
				if _, err := d.sql.Exec(`DROP TRIGGER fail_managed`); err != nil {
					t.Fatal(err)
				}
				if err := run("effect"); err != nil {
					t.Fatal("rollback consumed key", err)
				}
			})
		}
	}
}

func TestManagedTaskRaces(t *testing.T) {
	for _, scenario := range []string{"edit-offer", "finalize-unmanage", "unmanage-offer"} {
		t.Run(scenario, func(t *testing.T) {
			var r *Registry
			var d *DB
			var team Team
			var a, admin TeamAuditContext
			var c TeamOffer
			var accepted TeamAssignment
			if scenario == "finalize-unmanage" {
				r, d, team, a, admin, c, accepted = acceptedFixture(t)
			} else {
				r, d, team, a, admin, c, accepted = runningResultFixture(t)
				sub, err := d.SubmitTeamResult(team.ID, accepted.ID, resultSubmission(c), a, "submit")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.ReviewTeamResult(team.ID, sub.ID, resultDecision(c, sub, "rework"), a, "rework"); err != nil {
					t.Fatal(err)
				}
			}
			peer, err := NewRegistry(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			d2, err := peer.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			offer := c
			offer.ExpectedRevision = task.Revision
			offer.Worker = messageMember(t, d, team, a, "fresh")
			var first, second func() error
			switch scenario {
			case "edit-offer":
				first = func() error {
					_, err := d.EditManagedTask(team.ID, task.ID, editCommand(c, task), a, "edit")
					return err
				}
				second = func() error {
					_, err := d2.OfferTeamAssignment(team.ID, offer, a, "race-offer", allowAssignmentWorker)
					return err
				}
			case "finalize-unmanage":
				first = func() error {
					_, err := d.FinalizeManagedTask(team.ID, task.ID, finalizeCommand(c, task, accepted.ID), a, "finalize")
					return err
				}
				second = func() error {
					_, err := d2.UnmanageTask(task.ID, TeamUnmanageCommand{ExpectedRevision: task.Revision, Reason: "release"}, admin, "unmanage")
					return err
				}
			case "unmanage-offer":
				first = func() error {
					_, err := d.UnmanageTask(task.ID, TeamUnmanageCommand{ExpectedRevision: task.Revision, Reason: "release"}, admin, "unmanage")
					return err
				}
				second = func() error {
					_, err := d2.OfferTeamAssignment(team.ID, offer, a, "race-offer", allowAssignmentWorker)
					return err
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			for i, call := range []func() error{first, second} {
				wg.Add(1)
				go func(i int, call func() error) { defer wg.Done(); <-start; errs[i] = call() }(i, call)
			}
			close(start)
			wg.Wait()
			for _, err := range errs {
				var conflict *TaskConflict
				if err != nil && !errors.Is(err, ErrTeamAssignmentConflict) && !errors.As(err, &conflict) {
					t.Fatal(err)
				}
			}
			wins := 0
			for _, err := range errs {
				if err == nil {
					wins++
				}
			}
			got, err := d.GetTask(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "edit-offer":
				// Both revision-checked at the same revision: exactly one lands.
				// An offer does not advance the task revision; an edit does.
				offered := errs[1] == nil
				if wins != 1 || offered != (got.Coordination.AttemptID != "") || offered != (got.Revision == task.Revision) {
					t.Fatal("edit/offer race", errs, got)
				}
			case "finalize-unmanage":
				// Unmanage first refuses finalize; finalize first still permits unmanage
				// only at the new revision, so at most one lands here.
				if wins != 1 || got.Revision != task.Revision+1 || (errs[0] == nil) != (got.State == "DONE") || (errs[1] == nil) != (got.Coordination == nil) {
					t.Fatal("finalize/unmanage race", errs, got)
				}
			case "unmanage-offer":
				// Offer first reserves and refuses unmanage; unmanage first bumps the
				// revision so the offer conflicts. Exactly one lands.
				offered := errs[1] == nil
				if wins != 1 || offered != (got.Coordination != nil && got.Coordination.AttemptID != "") || offered != (got.Revision == task.Revision) || (errs[0] == nil) != (got.Coordination == nil) {
					t.Fatal("unmanage/offer race", errs, got)
				}
			}
		})
	}
}

func TestManagedTaskMigrationAndReopen(t *testing.T) {
	r, d, team, a, admin, c, accepted := acceptedFixture(t)
	// Roll the flag column back to the schema 17 shape and migrate forward.
	for _, q := range []string{`ALTER TABLE team_managed_tasks DROP COLUMN managed`, `UPDATE meta SET value='17' WHERE key='schema_version'`} {
		if _, err := d.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.GetMeta("schema_version"); v != fmt.Sprint(currentSchema) {
		t.Fatal(v)
	}
	task, err := d.GetTask(c.TaskID)
	if err != nil || task.Coordination == nil || task.Coordination.TeamID != team.ID {
		t.Fatal("existing rows lost management", task, err)
	}
	if _, err := d.UpdateTask(task.ID, task.TaskContent, task.Revision, a.Actor, "generic"); !errors.Is(err, ErrManagedTask) {
		t.Fatal(err)
	}
	done, err := d.FinalizeManagedTask(team.ID, task.ID, finalizeCommand(c, task, accepted.ID), a, "finalize")
	if err != nil {
		t.Fatal(err)
	}
	released, err := d.UnmanageTask(task.ID, TeamUnmanageCommand{ExpectedRevision: done.Revision, Reason: "release"}, admin, "unmanage")
	if err != nil {
		t.Fatal(err)
	}
	root := r.root
	r.Close()
	r2, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	d2, err := r2.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d2.GetTask(task.ID)
	if err != nil || got.Coordination != nil || got.State != "DONE" || got.Revision != released.Revision {
		t.Fatal(got, err)
	}
	if replay, err := d2.UnmanageTask(task.ID, TeamUnmanageCommand{ExpectedRevision: done.Revision, Reason: "release"}, admin, "unmanage"); err != nil || !reflect.DeepEqual(replay, released) {
		t.Fatal(replay, err)
	}
	if events := managedEvents(t, d2, team.ID, "team.task.unmanage"); len(events) != 1 {
		t.Fatal(events)
	}
	if _, err := d2.UpdateTask(task.ID, got.TaskContent, got.Revision, a.Actor, "generic"); err != nil {
		t.Fatal("generic write after reopen", err)
	}
}
