package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

func recoveryCommand(c TeamOffer, rev int64) TeamRecoveryCommand {
	return TeamRecoveryCommand{ExpectedRevision: rev, ExpectedWorker: c.Worker, ExpectedSessionGeneration: c.Worker.Generation, ExpectedCoordinatorGeneration: c.CoordinatorGeneration,
		Reconciliation: TeamRecoveryEvidence{ExecutionStopped: true, Reason: "Operator reconciled abandoned work", RuntimeCheck: "Fixture backend session/turn identified; no surviving process or uncertain command", WorktreeCheck: "Fixture worktree inspected; surviving changes preserved for separate review", EvidenceRefs: []TaskRef{{Kind: "text", Ref: "Disposable reconciliation fixture"}}}}
}

func recoveryFixture(t *testing.T, state string) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext, TeamOffer, TeamAssignment, TeamRecoveryCommand) {
	t.Helper()
	r, d, team, a, admin, c, out := runningResultFixture(t)
	rev := int64(2)
	for _, op := range map[string][]string{"BLOCKED": {"block"}, "STOP_REQUESTED": {"cancel"}, "STOPPED": {"cancel", "stopped"}}[state] {
		var err error
		out, err = d.ChangeTeamWork(team.ID, out.ID, op, workCommand(c, op, rev), a, "prepare-"+op)
		if err != nil {
			t.Fatal(err)
		}
		rev++
	}
	return r, d, team, a, admin, c, out, recoveryCommand(c, rev)
}

func TestTeamRecoveryRequeueAndImmutability(t *testing.T) {
	for _, state := range []string{"RUNNING", "BLOCKED", "STOP_REQUESTED", "STOPPED"} {
		t.Run(state, func(t *testing.T) {
			_, d, team, a, admin, c, before, cmd := recoveryFixture(t, state)
			session, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			taskBefore, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			out, err := d.RecoverTeamAssignment(team.ID, before.ID, cmd, admin, "recover")
			if err != nil || out.State != "RECOVERED" || out.Recovery == nil {
				t.Fatal(out, err)
			}
			if !reflect.DeepEqual(out.Recovery.Reconciliation, cmd.Reconciliation) || out.Recovery.TaskRevision != cmd.ExpectedRevision || out.Recovery.SessionGeneration != 1 || out.Recovery.CoordinatorGeneration != 1 || out.Worker != before.Worker || !reflect.DeepEqual(out.Profile, before.Profile) {
				t.Fatal("recovery snapshot", out)
			}
			afterSession, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil || !reflect.DeepEqual(session, afterSession) {
				t.Fatal("recovery changed session", afterSession, err)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil || task.State != "READY" || task.Blocker != "" || task.Revision != cmd.ExpectedRevision+1 || task.Coordination == nil || task.Coordination.AttemptID != "" {
				t.Fatal(task, err)
			}
			content := task.TaskContent
			content.State, content.Blocker = taskBefore.State, taskBefore.Blocker
			if !reflect.DeepEqual(content, taskBefore.TaskContent) {
				t.Fatal("unrelated task content changed")
			}
			if _, err := d.UpdateTask(task.ID, TaskContent{Title: "bypass", State: "DONE"}, task.Revision, admin.Actor, "bypass"); !errors.Is(err, ErrManagedTask) {
				t.Fatal(err)
			}
			for _, op := range []string{"block", "resume-work", "cancel", "stopped", "close-stop"} {
				if _, err := d.ChangeTeamWork(team.ID, out.ID, op, workCommand(c, op, task.Revision), a, "late-"+op); !errors.Is(err, ErrTeamAssignmentConflict) {
					t.Fatal("late work accepted", op, err)
				}
			}
			sub := resultSubmission(c)
			sub.ExpectedRevision = task.Revision
			if _, err := d.SubmitTeamResult(team.ID, out.ID, sub, a, "late-result"); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal(err)
			}
			c.ExpectedRevision = task.Revision
			next := mustOffer(t, d, team, a, c, "new-offer")
			if next.ID == out.ID || next.Recovery != nil {
				t.Fatal("old attempt reused")
			}
			replay, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover")
			if err != nil || !reflect.DeepEqual(replay, out) {
				t.Fatal("receipt changed", replay, err)
			}
			if _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "late-recover"); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal(err)
			}
			changed := cmd
			changed.Reconciliation.Reason = "different assessment"
			if _, err := d.RecoverTeamAssignment(team.ID, out.ID, changed, admin, "recover"); !errors.Is(err, ErrTaskRetryConflict) {
				t.Fatal(err)
			}
			var n int
			if err := d.sql.QueryRow(`SELECT count(*) FROM team_events WHERE json_extract(body,'$.operation')='team.assignment.recover' AND json_extract(body,'$.assignment.id')=? AND json_extract(body,'$.actor.kind')='admin' AND json_extract(body,'$.session.id')=? AND json_extract(body,'$.request_id')=?`, out.ID, session.ID, admin.RequestID).Scan(&n); err != nil || n != 1 {
				t.Fatal("recovery audit", n, err)
			}
			history, err := d.TaskHistory(task.ID, 0, 20)
			if err != nil || len(history.Changes) != int(task.Revision) || history.Changes[len(history.Changes)-1].Actor.Kind != "admin" {
				t.Fatal(history, err)
			}
		})
	}
}

func TestTeamRecoveryAuthorityAndFences(t *testing.T) {
	for _, scenario := range []string{"ordinary", "forged-admin", "disabled", "wrong-team", "wrong-worker", "assignment-generation", "session-generation", "coordinator-generation", "revision", "worker-resume", "coordinator-resume"} {
		t.Run(scenario, func(t *testing.T) {
			_, d, team, a, admin, c, out, cmd := recoveryFixture(t, "RUNNING")
			switch scenario {
			case "ordinary":
				admin = a
			case "forged-admin":
				admin.Actor.UserID = a.Actor.UserID
			case "disabled":
				if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "wrong-team":
				team.ID = uuidv7.New()
			case "wrong-worker":
				cmd.ExpectedWorker.SessionID = uuidv7.New()
			case "assignment-generation":
				cmd.ExpectedWorker.Generation++
			case "session-generation":
				cmd.ExpectedSessionGeneration++
			case "coordinator-generation":
				cmd.ExpectedCoordinatorGeneration++
			case "revision":
				cmd.ExpectedRevision++
			case "worker-resume", "coordinator-resume":
				h := c.Worker
				if scenario == "coordinator-resume" {
					h = c.TeamSessionHandle
				}
				if _, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: h.SessionID, Generation: h.Generation}, a, "resume"); err != nil {
					t.Fatal(err)
				}
			}
			counts := assignmentCounts(t, d)
			_, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover")
			if err == nil || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("invalid recovery accepted", err)
			}
			if scenario == "revision" {
				var conflict *TaskConflict
				if !errors.As(err, &conflict) || conflict.Current.Coordination == nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTeamRecoveryValidation(t *testing.T) {
	for _, scenario := range []string{"not-stopped", "reason", "runtime", "worktree", "no-refs", "bad-ref", "many-refs", "secret", "size", "revision", "session-generation", "coordinator-generation", "worker"} {
		t.Run(scenario, func(t *testing.T) {
			_, d, team, _, admin, _, out, cmd := recoveryFixture(t, "RUNNING")
			switch scenario {
			case "not-stopped":
				cmd.Reconciliation.ExecutionStopped = false
			case "reason":
				cmd.Reconciliation.Reason = " "
			case "runtime":
				cmd.Reconciliation.RuntimeCheck = ""
			case "worktree":
				cmd.Reconciliation.WorktreeCheck = ""
			case "no-refs":
				cmd.Reconciliation.EvidenceRefs = nil
			case "bad-ref":
				cmd.Reconciliation.EvidenceRefs[0] = TaskRef{Kind: "ci", Ref: "invalid"}
			case "many-refs":
				cmd.Reconciliation.EvidenceRefs = make([]TaskRef, 33)
			case "secret":
				cmd.Reconciliation.RuntimeCheck = "ghp_" + strings.Repeat("x", 36)
			case "size":
				for i := 0; i < 32; i++ {
					cmd.Reconciliation.EvidenceRefs = append(cmd.Reconciliation.EvidenceRefs, TaskRef{Kind: "text", Ref: strings.Repeat("a", 1100)})
				}
				cmd.Reconciliation.EvidenceRefs = cmd.Reconciliation.EvidenceRefs[1:]
			case "revision":
				cmd.ExpectedRevision = 0
			case "session-generation":
				cmd.ExpectedSessionGeneration = 0
			case "coordinator-generation":
				cmd.ExpectedCoordinatorGeneration = 0
			case "worker":
				cmd.ExpectedWorker.SessionID = "bad"
			}
			counts := assignmentCounts(t, d)
			if _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover"); !errors.Is(err, ErrTaskInvalid) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("invalid evidence wrote state")
			}
		})
	}
}

func TestTeamRecoveryProtectsOffersAndResults(t *testing.T) {
	for _, state := range []string{"OFFERED", "SUBMITTED", "ACCEPTED"} {
		t.Run(state, func(t *testing.T) {
			_, d, team, a, admin, c := assignmentFixture(t)
			out := mustOffer(t, d, team, a, c, "offer")
			if state != "OFFERED" {
				var err error
				out, err = d.ChangeTeamAssignment(team.ID, out.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker)
				if err != nil {
					t.Fatal(err)
				}
				out, err = d.SubmitTeamResult(team.ID, out.ID, resultSubmission(c), a, "submit")
				if err != nil {
					t.Fatal(err)
				}
				if state == "ACCEPTED" {
					out, err = d.ReviewTeamResult(team.ID, out.ID, resultDecision(c, out, "accept"), a, "review")
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.RecoverTeamAssignment(team.ID, out.ID, recoveryCommand(c, task.Revision), admin, "recover"); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal(err)
			}
			got, err := d.GetTeamAssignment(team.ID, out.ID, c.TeamSessionHandle, a.Actor)
			if err != nil || !reflect.DeepEqual(got, out) {
				t.Fatal("changed protected evidence", got, err)
			}
		})
	}
}

func TestTeamRecoveryDepartedWorkerAndReopen(t *testing.T) {
	r, d, team, a, admin, c, out, cmd := recoveryFixture(t, "STOP_REQUESTED")
	s, err := d.ChangeTeamSession(team.ID, "leave", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, "leave")
	if err != nil {
		t.Fatal(err)
	}
	content := team.TeamContent
	content.Enrollment = nil
	if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, content, admin, "unenroll"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover"); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal(err)
	}
	cmd.ExpectedSessionGeneration = s.Generation
	recovered, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Recovery.SessionGeneration != 2 || recovered.Worker.Generation != 1 {
		t.Fatal("lost separate generations", recovered)
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
	got, err := readTeamAssignment(d.sql, team.ID, out.ID)
	if err != nil || !reflect.DeepEqual(got, recovered) {
		t.Fatal("recovery lost on reopen", got, err)
	}
	replay, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover")
	if err != nil || !reflect.DeepEqual(replay, recovered) {
		t.Fatal(replay, err)
	}
	if _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, a, "recover"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal("ordinary replay", err)
	}
	if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal("disabled replay", err)
	}
}

func TestTeamRecoveryRollback(t *testing.T) {
	for _, table := range []string{"tasks", "task_history", "team_assignments", "team_events", "task_requests"} {
		t.Run(table, func(t *testing.T) {
			_, d, team, _, admin, c, out, cmd := recoveryFixture(t, "RUNNING")
			counts := assignmentCounts(t, d)
			before, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			verb := "INSERT"
			if table == "tasks" {
				verb = "UPDATE"
			}
			if _, err := d.sql.Exec(`CREATE TRIGGER fail_recovery BEFORE ` + verb + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover"); err == nil {
				t.Fatal("injection did not fail")
			}
			got, err := readTeamAssignment(d.sql, team.ID, out.ID)
			if err != nil || !reflect.DeepEqual(got, out) || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("partial assignment", got, err)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil || !reflect.DeepEqual(task, before) {
				t.Fatal("partial task", task, err)
			}
			if _, err := d.sql.Exec(`DROP TRIGGER fail_recovery`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover"); err != nil {
				t.Fatal("failed recovery consumed key", err)
			}
		})
	}
}

func TestTeamRecoveryRaces(t *testing.T) {
	for _, scenario := range []string{"submit", "cancel", "recover", "worker-resume", "coordinator-resume"} {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c, out, cmd := recoveryFixture(t, "RUNNING")
			peer, err := NewRegistry(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			d2, err := peer.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			calls := []func() error{func() error { _, err := d.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "recover"); return err }, nil}
			switch scenario {
			case "submit":
				calls[1] = func() error {
					_, err := d2.SubmitTeamResult(team.ID, out.ID, resultSubmission(c), a, "submit")
					return err
				}
			case "cancel":
				calls[1] = func() error {
					_, err := d2.ChangeTeamWork(team.ID, out.ID, "cancel", workCommand(c, "cancel", 2), a, "cancel")
					return err
				}
			case "recover":
				calls[1] = func() error {
					_, err := d2.RecoverTeamAssignment(team.ID, out.ID, cmd, admin, "other-recover")
					return err
				}
			default:
				h := c.Worker
				if scenario == "coordinator-resume" {
					h = c.TeamSessionHandle
				}
				calls[1] = func() error {
					_, err := d2.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: h.SessionID, Generation: h.Generation}, a, "resume")
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
					if !errors.Is(err, ErrTeamAssignmentConflict) && !errors.Is(err, ErrTeamSessionStale) && !errors.As(err, &conflict) {
						t.Fatal(err)
					}
				}
			}
			if strings.HasSuffix(scenario, "resume") {
				if errs[1] != nil || wins < 1 {
					t.Fatal(errs)
				}
			} else if wins != 1 {
				t.Fatal("race winners", wins, errs)
			}
			got, err := readTeamAssignment(d.sql, team.ID, out.ID)
			if err != nil {
				t.Fatal(err)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State == "RECOVERED" {
				if task.Coordination.AttemptID != "" || task.State != "READY" || got.Recovery.SessionGeneration != 1 || got.Recovery.CoordinatorGeneration != 1 {
					t.Fatal("bad recovery winner", got, task)
				}
			} else if task.Coordination.AttemptID != out.ID {
				t.Fatal("lost reservation", task)
			}
		})
	}
}

func TestTeamRecoveryOldReceiptShape(t *testing.T) {
	_, d, team, a, admin, c, run, cmd := recoveryFixture(t, "RUNNING")
	if _, err := d.RecoverTeamAssignment(team.ID, run.ID, cmd, admin, "recover"); err != nil {
		t.Fatal(err)
	}
	replay, err := d.ChangeTeamAssignment(team.ID, run.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker)
	if err != nil || !reflect.DeepEqual(replay, run) {
		t.Fatal(replay, err)
	}
	b, err := json.Marshal(replay)
	if err != nil || strings.Contains(string(b), `"recovery"`) {
		t.Fatal(fmt.Sprint("old receipt shape ", string(b)), err)
	}
}
