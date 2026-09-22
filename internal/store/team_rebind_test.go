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

// rebindFixture prepares a reserved attempt in the requested state for a
// worker that has not resumed yet. SUBMITTED keeps the reservation with a result.
func rebindFixture(t *testing.T, state string) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext, TeamOffer, TeamAssignment, int64) {
	t.Helper()
	if state == "SUBMITTED" {
		r, d, team, a, admin, c, out := runningResultFixture(t)
		out, err := d.SubmitTeamResult(team.ID, out.ID, resultSubmission(c), a, "submit")
		if err != nil {
			t.Fatal(err)
		}
		return r, d, team, a, admin, c, out, 3
	}
	r, d, team, a, admin, c, out, cmd := recoveryFixture(t, state)
	return r, d, team, a, admin, c, out, cmd.ExpectedRevision
}

func resumeWorker(t *testing.T, d *DB, team Team, a TeamAuditContext, h TeamSessionHandle, key string) TeamSession {
	t.Helper()
	s, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: h.SessionID, Generation: h.Generation}, a, key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rebindEvents(t *testing.T, d *DB, attempt string) int {
	t.Helper()
	var n int
	if err := d.sql.QueryRow(`SELECT count(*) FROM team_events WHERE json_extract(body,'$.operation')='team.assignment.rebind' AND json_extract(body,'$.assignment.id')=?`, attempt).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTeamSessionRebindCarriesReservedAttempt(t *testing.T) {
	for _, state := range []string{"RUNNING", "BLOCKED", "STOP_REQUESTED", "STOPPED", "SUBMITTED"} {
		t.Run(state, func(t *testing.T) {
			_, d, team, a, admin, c, before, rev := rebindFixture(t, state)
			taskBefore, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			historyBefore, err := d.TaskHistory(c.TaskID, 0, 50)
			if err != nil {
				t.Fatal(err)
			}
			counts := assignmentCounts(t, d)
			w := resumeWorker(t, d, team, a, c.Worker, "resume")
			if w.Generation != 2 {
				t.Fatal(w)
			}
			current := TeamSessionHandle{SessionID: w.ID, Generation: w.Generation}
			out, err := d.ReservedTeamAssignment(team.ID, current, a.Actor)
			if err != nil || out.ID != before.ID || out.State != state || out.Worker != current || out.RebindCount != 1 || len(out.Rebinds) != 1 || out.Rebinds[0].PreviousGeneration != 1 || out.Rebinds[0].Generation != 2 || out.Rebinds[0].CreatedAt == "" {
				t.Fatalf("rebound attempt: %+v %v", out, err)
			}
			// Everything except the worker generation and rebind history is intact.
			same := out
			same.Worker, same.Rebinds, same.RebindCount, same.UpdatedAt = before.Worker, nil, 0, before.UpdatedAt
			if !reflect.DeepEqual(same, before) {
				t.Fatalf("rebind changed unrelated fields:\n%+v\n%+v", same, before)
			}
			if state == "SUBMITTED" && out.Result.Worker != before.Worker {
				t.Fatal("result snapshot rebound", out.Result.Worker)
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil || !reflect.DeepEqual(task, taskBefore) {
				t.Fatal("rebind touched the task", task, err)
			}
			history, err := d.TaskHistory(c.TaskID, 0, 50)
			if err != nil || !reflect.DeepEqual(history, historyBefore) {
				t.Fatal("rebind wrote task history", err)
			}
			after := assignmentCounts(t, d)
			// Managed-task, assignment and task-history rows are unchanged; the
			// session event, the rebind event and one receipt are added.
			if after[0] != counts[0] || after[1] != counts[1] || after[2] != counts[2] || after[3] != counts[3]+2 || after[4] != counts[4]+1 {
				t.Fatal("rebind row counts", counts, after)
			}
			var n int
			if err := d.sql.QueryRow(`SELECT count(*) FROM team_events WHERE json_extract(body,'$.operation')='team.assignment.rebind' AND json_extract(body,'$.assignment.id')=? AND json_extract(body,'$.session.generation')=2 AND json_extract(body,'$.assignment.worker.generation')=2 AND json_extract(body,'$.request_id')=?`, out.ID, a.RequestID).Scan(&n); err != nil || n != 1 || rebindEvents(t, d, out.ID) != 1 {
				t.Fatal("rebind audit", n, err)
			}
			// The old handle can neither read nor command the attempt; the new one can.
			if _, err := d.ReservedTeamAssignment(team.ID, c.Worker, a.Actor); !errors.Is(err, ErrTeamSessionStale) {
				t.Fatal(err)
			}
			cmd := workCommand(c, "block", rev)
			switch state {
			case "RUNNING":
				if _, err := d.ChangeTeamWork(team.ID, out.ID, "block", cmd, a, "old-block"); !errors.Is(err, ErrTeamSessionStale) {
					t.Fatal(err)
				}
				sub := resultSubmission(c)
				sub.ExpectedRevision = rev
				if _, err := d.SubmitTeamResult(team.ID, out.ID, sub, a, "old-submit"); !errors.Is(err, ErrTeamSessionStale) {
					t.Fatal(err)
				}
				cmd.TeamSessionHandle = current
				if got, err := d.ChangeTeamWork(team.ID, out.ID, "block", cmd, a, "new-block"); err != nil || got.State != "BLOCKED" || got.Worker != current {
					t.Fatal(got, err)
				}
				cmd.Reason, cmd.ExpectedRevision = "resume-work: fixture explanation", rev+1
				if _, err := d.ChangeTeamWork(team.ID, out.ID, "resume-work", cmd, a, "new-resume-work"); err != nil {
					t.Fatal(err)
				}
				sub.TeamSessionHandle, sub.ExpectedRevision = current, rev+2
				got, err := d.SubmitTeamResult(team.ID, out.ID, sub, a, "new-submit")
				if err != nil || got.State != "SUBMITTED" || got.Result.Worker != current || got.Worker != current || got.RebindCount != 1 {
					t.Fatal(got, err)
				}
			case "BLOCKED":
				cmd.Reason = "resume-work: fixture explanation"
				if _, err := d.ChangeTeamWork(team.ID, out.ID, "resume-work", cmd, a, "old-resume-work"); !errors.Is(err, ErrTeamSessionStale) {
					t.Fatal(err)
				}
				cmd.TeamSessionHandle = current
				if got, err := d.ChangeTeamWork(team.ID, out.ID, "resume-work", cmd, a, "new-resume-work"); err != nil || got.State != "RUNNING" {
					t.Fatal(got, err)
				}
			case "STOP_REQUESTED":
				cmd.Reason = "stopped: fixture explanation"
				if _, err := d.ChangeTeamWork(team.ID, out.ID, "stopped", cmd, a, "old-stopped"); !errors.Is(err, ErrTeamSessionStale) {
					t.Fatal(err)
				}
				cmd.TeamSessionHandle = current
				if got, err := d.ChangeTeamWork(team.ID, out.ID, "stopped", cmd, a, "new-stopped"); err != nil || got.State != "STOPPED" {
					t.Fatal(got, err)
				}
				// Recovery needs the rebound handle and the observed generation.
				recover := recoveryCommand(c, rev+1)
				if _, err := d.RecoverTeamAssignment(team.ID, out.ID, recover, admin, "old-recover"); !errors.Is(err, ErrTeamAssignmentConflict) {
					t.Fatal("recovery accepted the pre-resume handle", err)
				}
				recover.ExpectedWorker, recover.ExpectedSessionGeneration = current, current.Generation
				if got, err := d.RecoverTeamAssignment(team.ID, out.ID, recover, admin, "recover"); err != nil || got.State != "RECOVERED" || got.Recovery.SessionGeneration != 2 || got.RebindCount != 1 {
					t.Fatal(got, err)
				}
			case "STOPPED":
				// The coordinator closes acknowledged work regardless of worker rebinding.
				if got, err := d.ChangeTeamWork(team.ID, out.ID, "close-stop", workCommand(c, "close-stop", rev), a, "close"); err != nil || got.State != "CANCELLED" || got.Worker != current {
					t.Fatal(got, err)
				}
			case "SUBMITTED":
				if got, err := d.ReviewTeamResult(team.ID, out.ID, resultDecision(c, out, "accept"), a, "review"); err != nil || got.State != "ACCEPTED" || got.Result.Worker != before.Worker || got.Worker != current {
					t.Fatal(got, err)
				}
			}
			// Replaying the resume returns the original session without a second rebind.
			replay := resumeWorker(t, d, team, a, c.Worker, "resume")
			if !reflect.DeepEqual(replay, w) || rebindEvents(t, d, out.ID) != 1 {
				t.Fatal("resume replay repeated effects", replay)
			}
		})
	}
}

func TestTeamSessionRebindExclusions(t *testing.T) {
	t.Run("offered", func(t *testing.T) {
		_, d, team, a, _, c := assignmentFixture(t)
		offer := mustOffer(t, d, team, a, c, "offer")
		w := resumeWorker(t, d, team, a, c.Worker, "resume")
		current := TeamSessionHandle{SessionID: w.ID, Generation: w.Generation}
		got, err := d.ReservedTeamAssignment(team.ID, current, a.Actor)
		if err != nil || !reflect.DeepEqual(got, offer) || rebindEvents(t, d, offer.ID) != 0 {
			t.Fatal("offer was rebound", got, err)
		}
		if _, err := d.ChangeTeamAssignment(team.ID, offer.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: current}, a, "accept", allowAssignmentWorker); !errors.Is(err, ErrTeamSessionDenied) {
			t.Fatal("resumed worker accepted an old-generation offer", err)
		}
		if _, err := d.ChangeTeamAssignment(team.ID, offer.ID, "withdraw", TeamAssignmentCommand{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: c.CoordinatorGeneration, Reason: "worker resumed"}, a, "withdraw", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := d.ReservedTeamAssignment(team.ID, current, a.Actor); !errors.Is(err, ErrTeamAssignmentNotFound) {
			t.Fatal(err)
		}
		c.Worker = current
		next := mustOffer(t, d, team, a, c, "re-offer")
		if got, err := d.ChangeTeamAssignment(team.ID, next.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: current}, a, "accept-new", allowAssignmentWorker); err != nil || got.State != "RUNNING" || got.RebindCount != 0 {
			t.Fatal(got, err)
		}
	})
	t.Run("coordinator-heartbeat-profile", func(t *testing.T) {
		_, d, team, a, _, c, out := runningResultFixture(t)
		co := resumeWorker(t, d, team, a, c.TeamSessionHandle, "co-resume")
		if co.Role != "coordinator" || co.CoordinatorGeneration != 2 {
			t.Fatal(co)
		}
		if _, err := d.ChangeTeamSession(team.ID, "heartbeat", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1, Availability: "unavailable"}, a, "heartbeat"); err != nil {
			t.Fatal(err)
		}
		p := testProfile()
		if _, err := d.ChangeTeamSession(team.ID, "profile", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1, Profile: &p, ExpectedProfileRevision: 1}, a, "profile"); err != nil {
			t.Fatal(err)
		}
		got, err := d.ReservedTeamAssignment(team.ID, c.Worker, a.Actor)
		if err != nil || !reflect.DeepEqual(got, out) || rebindEvents(t, d, out.ID) != 0 {
			t.Fatal("non-resume operation rebound", got, err)
		}
		// The coordinator's own handle holds no reservation; wrong bindings are denied.
		coHandle := TeamSessionHandle{SessionID: co.ID, Generation: co.Generation}
		if _, err := d.ReservedTeamAssignment(team.ID, coHandle, a.Actor); !errors.Is(err, ErrTeamAssignmentNotFound) {
			t.Fatal(err)
		}
		wrong := a.Actor
		wrong.TokenID = uuidv7.New()
		if _, err := d.ReservedTeamAssignment(team.ID, c.Worker, wrong); !errors.Is(err, ErrTeamSessionDenied) {
			t.Fatal(err)
		}
		if _, err := d.ReservedTeamAssignment(uuidv7.New(), c.Worker, a.Actor); !errors.Is(err, ErrTeamSessionDenied) {
			t.Fatal(err)
		}
	})
	t.Run("leave-keeps-reservation", func(t *testing.T) {
		_, d, team, a, admin, c, out := runningResultFixture(t)
		left, err := d.ChangeTeamSession(team.ID, "leave", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, "leave")
		if err != nil || left.State != "left" || left.Generation != 2 {
			t.Fatal(left, err)
		}
		got, err := readTeamAssignment(d.sql, team.ID, out.ID)
		if err != nil || !reflect.DeepEqual(got, out) || rebindEvents(t, d, out.ID) != 0 {
			t.Fatal("leave rebound or released", got, err)
		}
		task, err := d.GetTask(c.TaskID)
		if err != nil || task.Coordination.AttemptID != out.ID {
			t.Fatal(task, err)
		}
		// The departed handle stays the assignment handle; the observed generation is 2.
		recover := recoveryCommand(c, 2)
		recover.ExpectedSessionGeneration = 2
		if got, err := d.RecoverTeamAssignment(team.ID, out.ID, recover, admin, "recover"); err != nil || got.State != "RECOVERED" {
			t.Fatal(got, err)
		}
	})
	t.Run("pre-rebind-database", func(t *testing.T) {
		// A database written before rebinding existed may hold an attempt whose
		// handle lags the session generation. Resume still carries it forward and
		// records the handle it actually replaced, instead of stranding the worker.
		_, d, team, a, _, c, out := runningResultFixture(t)
		if _, err := d.sql.Exec(`UPDATE team_sessions SET body=json_set(body,'$.generation',2) WHERE id=?`, c.Worker.SessionID); err != nil {
			t.Fatal(err)
		}
		w := resumeWorker(t, d, team, a, TeamSessionHandle{SessionID: c.Worker.SessionID, Generation: 2}, "resume")
		got, err := d.ReservedTeamAssignment(team.ID, TeamSessionHandle{SessionID: w.ID, Generation: w.Generation}, a.Actor)
		if err != nil || got.ID != out.ID || got.Worker.Generation != 3 || got.RebindCount != 1 || got.Rebinds[0].PreviousGeneration != 1 || got.Rebinds[0].Generation != 3 {
			t.Fatalf("lagging handle not carried forward: %+v %v", got, err)
		}
	})
	t.Run("idle-worker", func(t *testing.T) {
		_, d, team, a, _, c := assignmentFixture(t)
		counts := assignmentCounts(t, d)
		w := resumeWorker(t, d, team, a, c.Worker, "resume")
		after := assignmentCounts(t, d)
		if after[3] != counts[3]+1 || after[4] != counts[4]+1 {
			t.Fatal("idle resume wrote assignment state", counts, after)
		}
		if _, err := d.ReservedTeamAssignment(team.ID, TeamSessionHandle{SessionID: w.ID, Generation: w.Generation}, a.Actor); !errors.Is(err, ErrTeamAssignmentNotFound) {
			t.Fatal(err)
		}
	})
}

func TestTeamSessionRebindHistoryBound(t *testing.T) {
	_, d, team, a, _, c, out := runningResultFixture(t)
	h := c.Worker
	for i := range MaxTeamRebinds + 3 {
		w := resumeWorker(t, d, team, a, h, fmt.Sprint("resume-", i))
		h = TeamSessionHandle{SessionID: w.ID, Generation: w.Generation}
	}
	got, err := d.ReservedTeamAssignment(team.ID, h, a.Actor)
	if err != nil || got.Worker != h || got.RebindCount != MaxTeamRebinds+3 || len(got.Rebinds) != MaxTeamRebinds || got.Rebinds[0].PreviousGeneration != 4 || got.Rebinds[MaxTeamRebinds-1].Generation != h.Generation {
		t.Fatalf("bounded history: %+v %v", got, err)
	}
	if rebindEvents(t, d, out.ID) != MaxTeamRebinds+3 {
		t.Fatal("audit must keep every rebind")
	}
	if b, err := json.Marshal(got); err != nil || len(b) > MaxTaskBytes {
		t.Fatal("assignment grew past the task limit", len(b), err)
	}
}

func TestTeamSessionRebindRollback(t *testing.T) {
	triggers := map[string]string{
		"team_sessions":    `BEFORE UPDATE ON team_sessions`,
		"session-event":    `BEFORE INSERT ON team_events WHEN json_extract(NEW.body,'$.operation')='team.resume'`,
		"team_assignments": `BEFORE UPDATE ON team_assignments`,
		"rebind-event":     `BEFORE INSERT ON team_events WHEN json_extract(NEW.body,'$.operation')='team.assignment.rebind'`,
		"task_requests":    `BEFORE INSERT ON task_requests`,
	}
	for _, boundary := range []string{"team_sessions", "session-event", "team_assignments", "rebind-event", "task_requests"} {
		t.Run(boundary, func(t *testing.T) {
			_, d, team, a, _, c, out := runningResultFixture(t)
			session, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			counts := assignmentCounts(t, d)
			if _, err := d.sql.Exec(`CREATE TRIGGER fail_rebind ` + triggers[boundary] + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, "resume"); err == nil {
				t.Fatal("injection did not fail")
			}
			after, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil || !reflect.DeepEqual(after, session) {
				t.Fatal("partial session", after, err)
			}
			got, err := readTeamAssignment(d.sql, team.ID, out.ID)
			if err != nil || !reflect.DeepEqual(got, out) || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("partial assignment", got, err)
			}
			if _, err := d.sql.Exec(`DROP TRIGGER fail_rebind`); err != nil {
				t.Fatal(err)
			}
			w := resumeWorker(t, d, team, a, c.Worker, "resume")
			if got, err := d.ReservedTeamAssignment(team.ID, TeamSessionHandle{SessionID: w.ID, Generation: w.Generation}, a.Actor); err != nil || got.Worker.Generation != 2 || got.RebindCount != 1 {
				t.Fatal("rollback consumed key or rebind missing", got, err)
			}
		})
	}
}

func TestTeamSessionRebindRaces(t *testing.T) {
	for _, scenario := range []string{"submit", "cancel", "recover", "resume"} {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c, out := runningResultFixture(t)
			peer, err := NewRegistry(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			d2, err := peer.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			resume := func() error {
				_, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, "resume")
				return err
			}
			var other func() error
			switch scenario {
			case "submit":
				other = func() error {
					_, err := d2.SubmitTeamResult(team.ID, out.ID, resultSubmission(c), a, "submit")
					return err
				}
			case "cancel":
				other = func() error {
					_, err := d2.ChangeTeamWork(team.ID, out.ID, "cancel", workCommand(c, "cancel", 2), a, "cancel")
					return err
				}
			case "recover":
				other = func() error {
					_, err := d2.RecoverTeamAssignment(team.ID, out.ID, recoveryCommand(c, 2), admin, "recover")
					return err
				}
			case "resume":
				other = func() error {
					_, err := d2.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, "other-resume")
					return err
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			for i, call := range []func() error{resume, other} {
				wg.Add(1)
				go func(i int, call func() error) { defer wg.Done(); <-start; errs[i] = call() }(i, call)
			}
			close(start)
			wg.Wait()
			for _, err := range errs {
				var conflict *TaskConflict
				if err != nil && !errors.Is(err, ErrTeamAssignmentConflict) && !errors.Is(err, ErrTeamSessionStale) && !errors.As(err, &conflict) {
					t.Fatal(err)
				}
			}
			session, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := readTeamAssignment(d.sql, team.ID, out.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "resume":
				// Two resumes from the same old handle have exactly one winner.
				if (errs[0] == nil) == (errs[1] == nil) || session.Generation != 2 || got.Worker.Generation != 2 || got.RebindCount != 1 {
					t.Fatal("resume race", errs, session, got)
				}
			case "cancel":
				// Coordinator cancellation is independent of the worker generation.
				if errs[0] != nil || errs[1] != nil || got.State != "STOP_REQUESTED" || got.Worker.Generation != 2 {
					t.Fatal("cancel race", errs, got)
				}
			default:
				if errs[0] != nil {
					t.Fatal("resume lost", errs)
				}
				if errs[1] == nil {
					// The other command won before resume: the attempt is either closed
					// (recover, no rebind) or submitted and then carried forward.
					if scenario == "recover" && (got.State != "RECOVERED" || got.RebindCount != 0) {
						t.Fatal("recover race", got)
					}
					if scenario == "submit" && (got.State != "SUBMITTED" || got.Worker.Generation != 2 || got.Result.Worker.Generation != 1) {
						t.Fatal("submit race", got)
					}
				} else if got.State != "RUNNING" || got.Worker.Generation != 2 || got.RebindCount != 1 {
					t.Fatal("rebind winner", got)
				}
			}
			if got.State != "RECOVERED" && got.Worker.Generation != session.Generation {
				t.Fatal("reserved attempt not bound to the current session", got, session)
			}
		})
	}
}

func TestTeamSessionRebindReopenAndReceipts(t *testing.T) {
	r, d, team, a, _, c, out := runningResultFixture(t)
	w := resumeWorker(t, d, team, a, c.Worker, "resume")
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
	current := TeamSessionHandle{SessionID: w.ID, Generation: w.Generation}
	got, err := d2.ReservedTeamAssignment(team.ID, current, a.Actor)
	if err != nil || got.ID != out.ID || got.Worker != current || got.RebindCount != 1 || len(got.Rebinds) != 1 {
		t.Fatal(got, err)
	}
	// Old accept receipts keep their pre-rebind shape; they report a past command.
	replay, err := d2.ChangeTeamAssignment(team.ID, out.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker)
	if err != nil || !reflect.DeepEqual(replay, out) {
		t.Fatal(replay, err)
	}
	if b, err := json.Marshal(replay); err != nil || strings.Contains(string(b), `"rebind`) {
		t.Fatal("old receipt shape", string(b), err)
	}
	sub := resultSubmission(c)
	sub.TeamSessionHandle = current
	if got, err := d2.SubmitTeamResult(team.ID, out.ID, sub, a, "submit"); err != nil || got.State != "SUBMITTED" || got.Result.Worker != current {
		t.Fatal(got, err)
	}
}
