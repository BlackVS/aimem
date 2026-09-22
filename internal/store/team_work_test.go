package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/uuidv7"
)

func workCommand(c TeamOffer, op string, revision int64) TeamWorkCommand {
	out := TeamWorkCommand{TeamSessionHandle: c.Worker, ExpectedRevision: revision, Reason: op + ": fixture explanation"}
	if op == "cancel" || op == "close-stop" {
		out.TeamSessionHandle, out.CoordinatorGeneration = c.TeamSessionHandle, c.CoordinatorGeneration
	}
	return out
}

func workFixture(t *testing.T, op string) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext, TeamOffer, TeamAssignment, TeamWorkCommand) {
	t.Helper()
	r, d, team, a, admin, c, out := runningResultFixture(t)
	pre := map[string][]string{"resume-work": {"block"}, "stopped": {"cancel"}, "close-stop": {"cancel", "stopped"}}[op]
	rev := int64(2)
	for _, before := range pre {
		var err error
		out, err = d.ChangeTeamWork(team.ID, out.ID, before, workCommand(c, before, rev), a, "prepare-"+before)
		if err != nil {
			t.Fatal(err)
		}
		rev++
	}
	return r, d, team, a, admin, c, out, workCommand(c, op, rev)
}

func TestTeamWorkControlLifecycle(t *testing.T) {
	_, d, team, a, admin, c, run := runningResultFixture(t)
	initial, err := d.GetTask(c.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateTask(TaskContent{Title: "other task", State: "READY"}, a.Actor, "other")
	if err != nil {
		t.Fatal(err)
	}
	capacity := c
	capacity.TaskID = other.ID
	var saved []TeamAssignment
	var commands []TeamWorkCommand
	ops := []string{"block", "resume-work", "block", "cancel", "stopped", "close-stop"}
	wantAttempts := []string{"BLOCKED", "RUNNING", "BLOCKED", "STOP_REQUESTED", "STOPPED", "CANCELLED"}
	wantTasks := []string{"BLOCKED", "IN_PROGRESS", "BLOCKED", "BLOCKED", "BLOCKED", "READY"}
	for i, op := range ops {
		cmd := workCommand(c, op, int64(i+2))
		out, err := d.ChangeTeamWork(team.ID, run.ID, op, cmd, a, fmt.Sprint("effect", i))
		if err != nil || out.State != wantAttempts[i] || out.Reason != cmd.Reason {
			t.Fatal(out, err)
		}
		saved, commands = append(saved, out), append(commands, cmd)
		task, err := d.GetTask(c.TaskID)
		if err != nil || task.State != wantTasks[i] || task.Revision != int64(i+3) || task.Coordination == nil {
			t.Fatal(task, err)
		}
		if (task.State == "BLOCKED") != (task.Blocker != "") {
			t.Fatal("blocker projection", task)
		}
		content := task.TaskContent
		content.State, content.Blocker = initial.State, initial.Blocker
		if !reflect.DeepEqual(content, initial.TaskContent) {
			t.Fatal("unrelated content changed", content)
		}
		if _, err := d.UpdateTask(task.ID, TaskContent{Title: "bypass", State: "DONE"}, task.Revision, admin.Actor, fmt.Sprint("bypass", i)); !errors.Is(err, ErrManagedTask) {
			t.Fatal(err)
		}
		if op != "resume-work" {
			sub := resultSubmission(c)
			sub.ExpectedRevision = task.Revision
			if _, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, fmt.Sprint("late-result", i)); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal("result crossed work-state fence", err)
			}
		}
		if op != "close-stop" {
			if task.Coordination.AttemptID != run.ID || task.Coordination.State != out.State {
				t.Fatal("lost reservation", task)
			}
			if _, err := d.OfferTeamAssignment(team.ID, capacity, a, "capacity", allowAssignmentWorker); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal("worker slot released early", err)
			}
		} else if task.Coordination.AttemptID != "" {
			t.Fatal("stop did not release", task)
		}
	}
	for i, op := range ops {
		replay, err := d.ChangeTeamWork(team.ID, run.ID, op, commands[i], a, fmt.Sprint("effect", i))
		if err != nil || !reflect.DeepEqual(replay, saved[i]) {
			t.Fatal("receipt changed", replay, err)
		}
		changed := commands[i]
		changed.Reason = "different explanation"
		if _, err := d.ChangeTeamWork(team.ID, run.ID, op, changed, a, fmt.Sprint("effect", i)); !errors.Is(err, ErrTaskRetryConflict) {
			t.Fatal(err)
		}
	}
	c.ExpectedRevision = 8
	newOffer := mustOffer(t, d, team, a, c, "new-offer")
	if newOffer.ID == run.ID {
		t.Fatal("reused stopped attempt")
	}
	history, err := d.TaskHistory(c.TaskID, 0, 20)
	if err != nil || len(history.Changes) != 8 {
		t.Fatal(history, err)
	}
	for _, change := range history.Changes {
		if change.Task.Coordination != nil {
			t.Fatal("persisted projection")
		}
	}
	var n int
	if err := d.sql.QueryRow(`SELECT count(*) FROM team_events WHERE json_extract(body,'$.assignment.id')=? AND json_extract(body,'$.assignment.state') IN ('BLOCKED','STOP_REQUESTED','STOPPED','CANCELLED') AND json_extract(body,'$.request_id')=?`, run.ID, a.RequestID).Scan(&n); err != nil || n != 5 {
		t.Fatal("audit missing or duplicated", n, err)
	}
}

func TestTeamWorkAuthorityAndRevision(t *testing.T) {
	for _, op := range []string{"block", "resume-work", "cancel", "stopped", "close-stop"} {
		for _, scenario := range []string{"wrong-role", "token", "admin", "generation", "co-generation", "revision", "unenrolled", "disabled", "wrong-team", "resumed"} {
			t.Run(op+"/"+scenario, func(t *testing.T) {
				r, d, team, a, admin, c, out, cmd := workFixture(t, op)
				switch scenario {
				case "wrong-role":
					cmd.TeamSessionHandle = c.TeamSessionHandle
					if op == "cancel" || op == "close-stop" {
						cmd.TeamSessionHandle = c.Worker
					}
				case "token":
					a.Actor.TokenID = uuidv7.New()
				case "admin":
					a = admin
				case "generation":
					cmd.Generation++
				case "co-generation":
					cmd.CoordinatorGeneration++
				case "revision":
					cmd.ExpectedRevision++
				case "unenrolled":
					content := team.TeamContent
					content.Enrollment = nil
					if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, content, admin, "unenroll"); err != nil {
						t.Fatal(err)
					}
				case "disabled":
					if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
						t.Fatal(err)
					}
				case "wrong-team":
					team.ID = uuidv7.New()
				case "resumed":
					if _, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: cmd.SessionID, Generation: cmd.Generation}, a, "resume"); err != nil {
						t.Fatal(err)
					}
				}
				before := assignmentCounts(t, d)
				_, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "effect")
				if err == nil || !reflect.DeepEqual(before, assignmentCounts(t, d)) {
					t.Fatal("invalid effect accepted", err)
				}
				if scenario == "revision" {
					var conflict *TaskConflict
					if !errors.As(err, &conflict) || conflict.Current.Coordination == nil {
						t.Fatal("missing revision evidence", err)
					}
				}
			})
		}
	}
}

func TestTeamWorkTransitionFences(t *testing.T) {
	for _, op := range []string{"block", "resume-work", "cancel", "stopped", "close-stop"} {
		for _, state := range []string{"OFFERED", "SUBMITTED", "ACCEPTED", "RETURNED", "CANCELLED"} {
			t.Run(op+"/"+state, func(t *testing.T) {
				_, d, team, a, _, _, out, cmd := workFixture(t, op)
				if _, err := d.sql.Exec(`UPDATE team_assignments SET body=json_set(body,'$.state',?) WHERE id=?`, state, out.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "invalid-state"); !errors.Is(err, ErrTeamAssignmentConflict) {
					t.Fatal(err)
				}
			})
		}
	}
	_, d, team, a, _, c, out, _ := workFixture(t, "stopped")
	for _, op := range []string{"block", "resume-work", "cancel", "close-stop"} {
		if _, err := d.ChangeTeamWork(team.ID, out.ID, op, workCommand(c, op, 3), a, op); !errors.Is(err, ErrTeamAssignmentConflict) {
			t.Fatal("stop request bypass", op, err)
		}
	}
}

func TestTeamWorkCurrentCoordinatorAndResumedWorker(t *testing.T) {
	_, d, team, a, _, c, out, cmd := workFixture(t, "close-stop")
	co, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.SessionID, Generation: c.Generation}, a, "co-resume")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Generation, cmd.CoordinatorGeneration = co.Generation, co.CoordinatorGeneration
	if _, err := d.ChangeTeamWork(team.ID, out.ID, "close-stop", cmd, a, "close"); err != nil {
		t.Fatal("current coordinator could not close acknowledged work", err)
	}
	_, d, team, a, _, c, out, cmd = workFixture(t, "resume-work")
	w, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: c.Worker.Generation}, a, "worker-resume")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Generation = w.Generation
	if _, err := d.ChangeTeamWork(team.ID, out.ID, "resume-work", cmd, a, "resume-work"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal("new handle took old-generation ownership", err)
	}
}

func TestTeamWorkValidationAndSize(t *testing.T) {
	_, d, team, a, _, c, out, cmd := workFixture(t, "block")
	for _, mutate := range []func(*TeamWorkCommand){
		func(c *TeamWorkCommand) { c.ExpectedRevision = 0 },
		func(c *TeamWorkCommand) { c.Reason = " " },
		func(c *TeamWorkCommand) { c.Reason = strings.Repeat("x", 4097) },
		func(c *TeamWorkCommand) { c.Reason = "ghp_" + strings.Repeat("x", 36) },
		func(c *TeamWorkCommand) { c.Reason = "bad\x00text" },
	} {
		bad := cmd
		mutate(&bad)
		if _, err := d.ChangeTeamWork(team.ID, out.ID, "block", bad, a, "invalid"); !errors.Is(err, ErrTaskInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := d.ChangeTeamWork(team.ID, out.ID, "recover", cmd, a, "unsupported"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatal(err)
	}
	// A valid task near the content limit must not become oversized when blocked.
	task, err := d.GetTask(c.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	task.Objective = strings.Repeat("a", 30<<10)
	if err := task.TaskContent.validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE tasks SET body=? WHERE id=?`, string(b), task.ID); err != nil {
		t.Fatal(err)
	}
	cmd.Reason = strings.Repeat("b", 4096)
	before := assignmentCounts(t, d)
	if _, err := d.ChangeTeamWork(team.ID, out.ID, "block", cmd, a, "size"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, assignmentCounts(t, d)) {
		t.Fatal("oversized task persisted")
	}
	cmd.Reason = "bounded blocker"
	if _, err := d.ChangeTeamWork(team.ID, out.ID, "block", cmd, a, "size"); err != nil {
		t.Fatal("validation consumed key", err)
	}
}

func TestTeamWorkRollback(t *testing.T) {
	for _, op := range []string{"block", "resume-work", "cancel", "stopped", "close-stop"} {
		for _, table := range []string{"tasks", "task_history", "team_assignments", "team_events", "task_requests"} {
			t.Run(op+"/"+table, func(t *testing.T) {
				_, d, team, a, _, c, out, cmd := workFixture(t, op)
				before := assignmentCounts(t, d)
				beforeTask, err := d.GetTask(c.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				verb := "INSERT"
				if table == "tasks" {
					verb = "UPDATE"
				}
				if _, err := d.sql.Exec(`CREATE TRIGGER fail_work BEFORE ` + verb + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				if _, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "effect"); err == nil {
					t.Fatal("injection did not fail")
				}
				got, err := d.GetTeamAssignment(team.ID, out.ID, c.TeamSessionHandle, a.Actor)
				if err != nil || !reflect.DeepEqual(got, out) || !reflect.DeepEqual(before, assignmentCounts(t, d)) {
					t.Fatal("partial assignment", got, err)
				}
				task, err := d.GetTask(c.TaskID)
				if err != nil || !reflect.DeepEqual(task, beforeTask) {
					t.Fatal("partial task", task, err)
				}
				if _, err := d.sql.Exec(`DROP TRIGGER fail_work`); err != nil {
					t.Fatal(err)
				}
				if _, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "effect"); err != nil {
					t.Fatal("rollback consumed key", err)
				}
			})
		}
	}
}

func TestTeamWorkRaces(t *testing.T) {
	for _, scenario := range []string{"submit-cancel", "resume-cancel", "close-close"} {
		t.Run(scenario, func(t *testing.T) {
			op := "cancel"
			if scenario == "resume-cancel" {
				op = "resume-work"
			}
			if scenario == "close-close" {
				op = "close-stop"
			}
			r, d, team, a, _, c, out, cmd := workFixture(t, op)
			peer, err := NewRegistry(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			d2, err := peer.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			calls := []func() error{
				func() error { _, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "first"); return err },
				func() error { _, err := d2.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "second"); return err },
			}
			if scenario == "submit-cancel" {
				calls[1] = func() error {
					_, err := d2.SubmitTeamResult(team.ID, out.ID, resultSubmission(c), a, "submit")
					return err
				}
			}
			if scenario == "resume-cancel" {
				calls[1] = func() error {
					_, err := d2.ChangeTeamWork(team.ID, out.ID, "cancel", workCommand(c, "cancel", cmd.ExpectedRevision), a, "cancel")
					return err
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			for i, call := range calls {
				wg.Add(1)
				go func(i int, call func() error) { defer wg.Done(); <-start; errs[i] = call() }(i, call)
			}
			close(start)
			wg.Wait()
			wins := 0
			for _, err := range errs {
				if err == nil {
					wins++
				} else {
					var conflict *TaskConflict
					if !errors.Is(err, ErrTeamAssignmentConflict) && !errors.As(err, &conflict) {
						t.Fatal(err)
					}
				}
			}
			if wins != 1 {
				t.Fatal("race winners", wins, errs)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.Revision != cmd.ExpectedRevision+1 {
				t.Fatal("duplicate task update", task)
			}
			if scenario == "close-close" && (task.State != "READY" || task.Coordination.AttemptID != "") {
				t.Fatal(task)
			}
			if scenario != "close-close" && task.Coordination.AttemptID != out.ID {
				t.Fatal("lost reservation", task)
			}
		})
	}
}

func TestTeamWorkReopenAndLiveness(t *testing.T) {
	for _, op := range []string{"block", "cancel", "stopped"} {
		t.Run(op, func(t *testing.T) {
			r, d, team, a, _, c, out, cmd := workFixture(t, op)
			saved, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "effect")
			if err != nil {
				t.Fatal(err)
			}
			s, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !s.Suspect(time.Now().Add(24*time.Hour), time.Second) {
				t.Fatal("fixture not suspect")
			}
			for _, action := range []string{"resume", "leave"} {
				g := int64(1)
				if action == "leave" {
					g = 2
				}
				if _, err := d.ChangeTeamSession(team.ID, action, TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: g}, a, action); err != nil {
					t.Fatal(err)
				}
			}
			root := r.root
			r.Close()
			r2, err := NewRegistry(root)
			if err != nil {
				t.Fatal(err)
			}
			defer r2.Close()
			d, err = r2.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil || task.Coordination == nil || task.Coordination.AttemptID != out.ID || task.Coordination.State != saved.State {
				t.Fatal("liveness released reservation", task, err)
			}
			replay, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "effect")
			if err != nil || !reflect.DeepEqual(replay, saved) {
				t.Fatal("reopen receipt changed", replay, err)
			}
			if op == "cancel" {
				late := workCommand(c, "stopped", task.Revision)
				if _, err := d.ChangeTeamWork(team.ID, out.ID, "stopped", late, a, "late"); !errors.Is(err, ErrTeamSessionStale) {
					t.Fatal("left worker acknowledged stop", err)
				}
			}
			if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ChangeTeamWork(team.ID, out.ID, op, cmd, a, "effect"); !errors.Is(err, ErrTeamSessionDenied) {
				t.Fatal("disabled receipt replay", err)
			}
		})
	}
}
