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

func resultSubmission(c TeamOffer) TeamResultSubmission {
	return TeamResultSubmission{TeamSessionHandle: c.Worker, ExpectedRevision: 2, TeamResultContent: TeamResultContent{
		BaseCommit: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Summary: "Bounded change ready for review",
		Validation: "go test ./... passed; candidate needs review and human merge", EvidenceRefs: []TaskRef{{Kind: "text", Ref: "Disposable test run: all packages passed"}},
	}}
}

func runningResultFixture(t *testing.T) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext, TeamOffer, TeamAssignment) {
	t.Helper()
	r, d, team, a, admin, c := assignmentFixture(t)
	out := mustOffer(t, d, team, a, c, "offer")
	out, err := d.ChangeTeamAssignment(team.ID, out.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker)
	if err != nil {
		t.Fatal(err)
	}
	return r, d, team, a, admin, c, out
}

func resultDecision(c TeamOffer, out TeamAssignment, decision string) TeamResultDecision {
	return TeamResultDecision{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: c.CoordinatorGeneration, ExpectedRevision: 3, ResultID: out.Result.ID, Decision: decision, Reason: "Evidence assessed against the recorded acceptance criteria"}
}

func TestTeamResultCorrectionAndAcceptance(t *testing.T) {
	for _, decision := range []string{"accept", "rework"} {
		t.Run(decision, func(t *testing.T) {
			_, d, team, a, admin, c, run := runningResultFixture(t)
			profile := testProfile()
			profile.Model = TeamModel{ID: "declared-new-model", Source: "agent_reported"}
			if _, err := d.ChangeTeamSession(team.ID, "profile", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1, ExpectedProfileRevision: 1, Profile: &profile}, a, "profile"); err != nil {
				t.Fatal(err)
			}
			sub := resultSubmission(c)
			out, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit")
			if err != nil || out.State != "SUBMITTED" || out.Result == nil {
				t.Fatal(out, err)
			}
			result := *out.Result
			if result.AttemptID != run.ID || result.Worker != c.Worker || result.TaskRevision != 2 || result.ProfileRevision != 2 || result.Profile.Model.ID != profile.Model.ID || !reflect.DeepEqual(out.Profile, run.Profile) || !reflect.DeepEqual(result.TeamResultContent, sub.TeamResultContent) {
				t.Fatalf("lost or rewritten snapshot: %+v", out)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil || task.Revision != 3 || task.State != "REVIEW" || task.Coordination.State != "SUBMITTED" {
				t.Fatal(task, err)
			}
			// Submission holds both reservation indexes, including the worker slot.
			other, err := d.CreateTask(TaskContent{Title: "next", State: "READY"}, a.Actor, "next")
			if err != nil {
				t.Fatal(err)
			}
			next := c
			next.TaskID = other.ID
			if _, err := d.OfferTeamAssignment(team.ID, next, a, "next-offer", allowAssignmentWorker); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal("submitted worker slot was released", err)
			}
			cmd := resultDecision(c, out, decision)
			closed, err := d.ReviewTeamResult(team.ID, run.ID, cmd, a, "review")
			if err != nil || closed.Review == nil || closed.Review.ResultID != result.ID || closed.Review.Decision != decision || !reflect.DeepEqual(*closed.Result, result) {
				t.Fatal(closed, err)
			}
			wantState, wantAttempt := "REVIEW", "ACCEPTED"
			if decision == "rework" {
				wantState, wantAttempt = "READY", "RETURNED"
			}
			task, err = d.GetTask(c.TaskID)
			if err != nil || task.State != wantState || task.Revision != 4 || closed.State != wantAttempt || task.Coordination == nil || task.Coordination.TeamID != team.ID || task.Coordination.AttemptID != "" {
				t.Fatal(task, closed, err)
			}
			if _, err := d.UpdateTask(task.ID, TaskContent{Title: "bypass", State: "DONE"}, task.Revision, admin.Actor, "bypass"); !errors.Is(err, ErrManagedTask) {
				t.Fatal(err)
			}
			if replay, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit"); err != nil || !reflect.DeepEqual(replay, out) {
				t.Fatal("submission receipt changed", replay, err)
			}
			if replay, err := d.ReviewTeamResult(team.ID, run.ID, cmd, a, "review"); err != nil || !reflect.DeepEqual(replay, closed) {
				t.Fatal("review receipt changed", replay, err)
			}
			if _, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "late-submit"); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal(err)
			}
			if _, err := d.ReviewTeamResult(team.ID, run.ID, cmd, a, "late-review"); !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal(err)
			}
			if decision == "rework" {
				next.TaskID, next.ExpectedRevision = task.ID, task.Revision
			}
			newOffer := mustOffer(t, d, team, a, next, "next-offer")
			if newOffer.ID == run.ID || newOffer.Result != nil || newOffer.Review != nil {
				t.Fatal("new offer inherited result", newOffer)
			}
			old, err := d.GetTeamAssignment(team.ID, run.ID, c.TeamSessionHandle, a.Actor)
			if err != nil || !reflect.DeepEqual(old, closed) {
				t.Fatal("old evidence changed", old, err)
			}
			history, err := d.TaskHistory(c.TaskID, 0, 20)
			if err != nil || len(history.Changes) != 4 {
				t.Fatal(history, err)
			}
			for _, change := range history.Changes {
				if change.Task.Coordination != nil {
					t.Fatal("projection persisted in history")
				}
			}
			var n int
			if err := d.sql.QueryRow(`SELECT count(*) FROM team_events WHERE json_extract(body,'$.operation') IN ('team.assignment.submit','team.assignment.review') AND json_extract(body,'$.assignment.result.id')=? AND json_extract(body,'$.request_id')=?`, result.ID, a.RequestID).Scan(&n); err != nil || n != 2 {
				t.Fatal("missing or duplicate correlated result events", n, err)
			}
		})
	}
}

func TestTeamResultAuthorityAndFences(t *testing.T) {
	for _, operation := range []string{"submit", "review"} {
		for _, scenario := range []string{"wrong-member", "wrong-token", "admin", "generation", "resumed", "left", "disabled", "unenrolled", "revision", "wrong-team", "wrong-state", "coordinator-generation", "wrong-result"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				r, d, team, a, admin, c, run := runningResultFixture(t)
				sub := resultSubmission(c)
				var cmd TeamResultDecision
				if operation == "review" {
					out, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit")
					if err != nil {
						t.Fatal(err)
					}
					cmd = resultDecision(c, out, "accept")
				}
				h := &sub.TeamSessionHandle
				if operation == "review" {
					h = &cmd.TeamSessionHandle
				}
				switch scenario {
				case "wrong-member":
					*h = messageMember(t, d, team, a, "unassigned")
				case "wrong-token":
					a.Actor.TokenID = uuidv7.New()
				case "admin":
					a = admin
				case "generation":
					h.Generation++
				case "resumed", "left":
					op := "resume"
					if scenario == "left" {
						op = "leave"
					}
					if _, err := d.ChangeTeamSession(team.ID, op, TeamSessionCommand{SessionID: h.SessionID, Generation: h.Generation}, a, op); err != nil {
						t.Fatal(err)
					}
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
				case "revision":
					sub.ExpectedRevision++
					cmd.ExpectedRevision++
				case "wrong-team":
					team.ID = uuidv7.New()
				case "wrong-state":
					if _, err := d.sql.Exec(`UPDATE team_assignments SET body=json_set(body,'$.state','WITHDRAWN') WHERE id=?`, run.ID); err != nil {
						t.Fatal(err)
					}
				case "coordinator-generation":
					if operation == "submit" {
						return
					}
					cmd.CoordinatorGeneration++
				case "wrong-result":
					if operation == "submit" {
						return
					}
					cmd.ResultID = uuidv7.New()
				}
				before := assignmentCounts(t, d)
				var err error
				if operation == "submit" {
					_, err = d.SubmitTeamResult(team.ID, run.ID, sub, a, "effect")
				} else {
					_, err = d.ReviewTeamResult(team.ID, run.ID, cmd, a, "effect")
				}
				if err == nil || !reflect.DeepEqual(before, assignmentCounts(t, d)) {
					t.Fatal("unauthorized/stale effect accepted", err)
				}
				if scenario == "revision" {
					var conflict *TaskConflict
					if !errors.As(err, &conflict) || conflict.Current.Coordination == nil {
						t.Fatal("missing current revision projection", err)
					}
				}
			})
		}
	}
}

func TestTeamResultValidation(t *testing.T) {
	for _, scenario := range []string{"short-base", "branch", "blank-summary", "blank-validation", "no-evidence", "bad-ref", "many-refs", "control", "secret", "oversize", "revision"} {
		t.Run(scenario, func(t *testing.T) {
			_, d, team, a, _, c, run := runningResultFixture(t)
			sub := resultSubmission(c)
			switch scenario {
			case "short-base":
				sub.BaseCommit = "abcdef"
			case "branch":
				sub.Commit = "main"
			case "blank-summary":
				sub.Summary = " "
			case "blank-validation":
				sub.Validation = ""
			case "no-evidence":
				sub.EvidenceRefs = nil
			case "bad-ref":
				sub.EvidenceRefs[0] = TaskRef{Kind: "ci", Ref: "not a URL"}
			case "many-refs":
				sub.EvidenceRefs = make([]TaskRef, 33)
			case "control":
				sub.Summary = "bad\x00text"
			case "secret":
				sub.Validation = "ghp_" + strings.Repeat("x", 36)
			case "oversize":
				sub.Summary, sub.Validation = strings.Repeat("a", 20<<10), strings.Repeat("b", 20<<10)
			case "revision":
				sub.ExpectedRevision = 0
			}
			before := assignmentCounts(t, d)
			if _, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit"); !errors.Is(err, ErrTaskInvalid) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, assignmentCounts(t, d)) {
				t.Fatal("invalid input wrote state")
			}
		})
	}
}

func TestTeamResultRetryAuthorization(t *testing.T) {
	r, d, team, a, admin, c, run := runningResultFixture(t)
	sub := resultSubmission(c)
	// SHA-256 repositories also use immutable, full object IDs.
	sub.BaseCommit, sub.Commit = strings.Repeat("a", 64), strings.Repeat("b", 64)
	out, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit")
	if err != nil {
		t.Fatal(err)
	}
	cmd := resultDecision(c, out, "accept")
	for _, mutate := range []func(*TeamResultDecision){
		func(c *TeamResultDecision) { c.ExpectedRevision = 0 },
		func(c *TeamResultDecision) { c.ResultID = "invalid" },
		func(c *TeamResultDecision) { c.Decision = "done" },
		func(c *TeamResultDecision) { c.Reason = " " },
	} {
		bad := cmd
		mutate(&bad)
		if _, err := d.ReviewTeamResult(team.ID, run.ID, bad, a, "review"); !errors.Is(err, ErrTaskInvalid) {
			t.Fatal(err)
		}
	}
	// A coordinator resumed after the offer may review existing worker output.
	co, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.SessionID, Generation: 1}, a, "co-resume")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReviewTeamResult(team.ID, run.ID, cmd, a, "review"); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal(err)
	}
	cmd.Generation, cmd.CoordinatorGeneration = co.Generation, co.CoordinatorGeneration
	closed, err := d.ReviewTeamResult(team.ID, run.ID, cmd, a, "review")
	if err != nil || closed.Review.CoordinatorGeneration != co.CoordinatorGeneration {
		t.Fatal(closed, err)
	}
	changed := sub
	changed.Summary = "different content"
	if _, err := d.SubmitTeamResult(team.ID, run.ID, changed, a, "submit"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatal(err)
	}
	changedDecision := cmd
	changedDecision.Decision = "rework"
	if _, err := d.ReviewTeamResult(team.ID, run.ID, changedDecision, a, "review"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatal(err)
	}
	if _, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, "worker-resume"); err != nil {
		t.Fatal(err)
	}
	if replay, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit"); err != nil || !reflect.DeepEqual(replay, out) {
		t.Fatal("receipt must survive resume", replay, err)
	}
	// Receipt replay still requires the original principal and current enrollment.
	other := a
	other.Actor.TokenID = uuidv7.New()
	if _, err := d.SubmitTeamResult(team.ID, run.ID, sub, other, "submit"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	content := team.TeamContent
	content.Enrollment = nil
	if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, content, admin, "unenroll"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	if _, err := d.ReviewTeamResult(team.ID, run.ID, cmd, a, "review"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
}

func TestTeamResultRollback(t *testing.T) {
	for _, operation := range []string{"submit", "accept", "rework"} {
		for _, table := range []string{"tasks", "task_history", "team_assignments", "team_events", "task_requests"} {
			t.Run(operation+"/"+table, func(t *testing.T) {
				_, d, team, a, _, c, run := runningResultFixture(t)
				sub := resultSubmission(c)
				out := run
				var err error
				if operation != "submit" {
					out, err = d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit")
					if err != nil {
						t.Fatal(err)
					}
				}
				before := assignmentCounts(t, d)
				beforeTask, err := d.GetTask(c.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				verb := "INSERT"
				if table == "tasks" {
					verb = "UPDATE"
				}
				if _, err := d.sql.Exec(`CREATE TRIGGER fail_result BEFORE ` + verb + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				call := func() error {
					if operation == "submit" {
						_, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "effect")
						return err
					}
					_, err := d.ReviewTeamResult(team.ID, run.ID, resultDecision(c, out, operation), a, "effect")
					return err
				}
				if err := call(); err == nil {
					t.Fatal("injection did not fail")
				}
				got, err := d.GetTeamAssignment(team.ID, run.ID, c.TeamSessionHandle, a.Actor)
				if err != nil || !reflect.DeepEqual(got, out) || !reflect.DeepEqual(before, assignmentCounts(t, d)) {
					t.Fatal("partial result transaction", got, err)
				}
				task, err := d.GetTask(c.TaskID)
				if err != nil || !reflect.DeepEqual(task, beforeTask) {
					t.Fatal("partial task transaction", task, err)
				}
				if _, err := d.sql.Exec(`DROP TRIGGER fail_result`); err != nil {
					t.Fatal(err)
				}
				if err := call(); err != nil {
					t.Fatal("failed transaction consumed key", err)
				}
			})
		}
	}
}

func TestTeamResultRacesAndReopen(t *testing.T) {
	for _, operation := range []string{"submit", "review"} {
		t.Run(operation, func(t *testing.T) {
			r, d, team, a, _, c, run := runningResultFixture(t)
			sub := resultSubmission(c)
			out := run
			var err error
			if operation == "review" {
				out, err = d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit")
				if err != nil {
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
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			for i, db := range []*DB{d, d2} {
				wg.Add(1)
				go func(i int, db *DB) {
					defer wg.Done()
					<-start
					if operation == "submit" {
						_, errs[i] = db.SubmitTeamResult(team.ID, run.ID, sub, a, fmt.Sprint("submit-", i))
					} else {
						_, errs[i] = db.ReviewTeamResult(team.ID, run.ID, resultDecision(c, out, []string{"accept", "rework"}[i]), a, fmt.Sprint("review-", i))
					}
				}(i, db)
			}
			close(start)
			wg.Wait()
			wins := 0
			for _, err := range errs {
				if err == nil {
					wins++
				} else if !errors.Is(err, ErrTeamAssignmentConflict) {
					t.Fatal(err)
				}
			}
			if wins != 1 {
				t.Fatal("race winners", wins, errs)
			}
			saved, err := d.GetTeamAssignment(team.ID, run.ID, c.TeamSessionHandle, a.Actor)
			if err != nil || saved.Result == nil {
				t.Fatal(saved, err)
			}
			root := r.root
			peer.Close()
			r.Close()
			reopened, err := NewRegistry(root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			d, err = reopened.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.GetTeamAssignment(team.ID, run.ID, c.TeamSessionHandle, a.Actor)
			if err != nil || !reflect.DeepEqual(got, saved) {
				t.Fatal("lost durable result", got, err)
			}
			// Old receipts predate optional result/review fields and still decode.
			replay, err := d.ChangeTeamAssignment(team.ID, run.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker)
			if err != nil || !reflect.DeepEqual(replay, run) {
				t.Fatal("old receipt changed", replay, err)
			}
			b, err := json.Marshal(replay)
			if err != nil || strings.Contains(string(b), `"result"`) || strings.Contains(string(b), `"review"`) {
				t.Fatal("legacy wire shape changed", string(b), err)
			}
		})
	}
}
