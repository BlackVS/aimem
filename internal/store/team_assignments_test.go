package store

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

func allowAssignmentWorker(TeamSession) error { return nil }

func assignmentFixture(t *testing.T) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext, TeamOffer) {
	t.Helper()
	r, d, team, a, admin := sessionFixture(t)
	co, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "co")
	if err != nil {
		t.Fatal(err)
	}
	w := messageMember(t, d, team, a, "worker")
	task, err := d.CreateTask(TaskContent{Title: "Implement bounded change", State: "READY", Objective: "Verified outcome", AcceptanceCriteria: "Runtime proof"}, a.Actor, "task")
	if err != nil {
		t.Fatal(err)
	}
	c := TeamOffer{TeamSessionHandle: TeamSessionHandle{co.ID, co.Generation}, CoordinatorGeneration: co.CoordinatorGeneration, TaskID: task.ID, ExpectedRevision: task.Revision, Worker: w, SuitabilityRationale: "S task; operator confirmed model and required Go capability", CostRationale: "Least costly suitable available worker"}
	return r, d, team, a, admin, c
}

func mustOffer(t *testing.T, d *DB, team Team, a TeamAuditContext, c TeamOffer, key string) TeamAssignment {
	t.Helper()
	out, err := d.OfferTeamAssignment(team.ID, c, a, key, allowAssignmentWorker)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTeamAssignmentLifecycleAndProjection(t *testing.T) {
	_, d, team, a, admin, c := assignmentFixture(t)
	out := mustOffer(t, d, team, a, c, "offer")
	if out.State != "OFFERED" || out.ProfileRevision != 1 || out.Profile.Model.Source != "unknown" || out.TaskRevision != 1 || out.Requirements.Objective == "" {
		t.Fatalf("snapshot %+v", out)
	}
	// Task responsibility is separate; offering does not change task revision/state.
	task, err := d.GetTask(c.TaskID)
	if err != nil || task.State != "READY" || task.Revision != 1 || task.Assignee != nil || task.Coordination == nil || task.Coordination.AttemptID != out.ID {
		t.Fatal(task, err)
	}
	page, err := d.ListTasks(TaskFilter{State: "READY", Limit: 1})
	if err != nil || len(page.Tasks) != 1 || !reflect.DeepEqual(page.Tasks[0].Coordination, task.Coordination) {
		t.Fatal(page, err)
	}
	for _, actor := range []TaskActor{a.Actor, admin.Actor} {
		if _, err := d.UpdateTask(task.ID, TaskContent{Title: "bypass", State: "DONE", Archived: true}, 1, actor, "bypass"); !errors.Is(err, ErrManagedTask) {
			t.Fatal(err)
		}
	}
	if _, err := d.AddTaskComment(task.ID, "Question remains discussion", a.Actor, "comment"); err != nil {
		t.Fatal(err)
	}
	// A new profile does not rewrite an offer's source/model snapshot.
	p := testProfile()
	p.Model.ID = "different-model"
	if _, err := d.ChangeTeamSession(team.ID, "profile", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1, ExpectedProfileRevision: 1, Profile: &p}, a, "profile"); err != nil {
		t.Fatal(err)
	}
	cmd := TeamAssignmentCommand{TeamSessionHandle: c.Worker}
	run, err := d.ChangeTeamAssignment(team.ID, out.ID, "accept", cmd, a, "accept", allowAssignmentWorker)
	if err != nil || run.State != "RUNNING" || run.Profile.Model.ID != out.Profile.Model.ID {
		t.Fatal(run, err)
	}
	task, err = d.GetTask(c.TaskID)
	if err != nil || task.State != "IN_PROGRESS" || task.Revision != 2 || task.Coordination.State != "RUNNING" {
		t.Fatal(task, err)
	}
	history, err := d.TaskHistory(task.ID, 0, 10)
	if err != nil || len(history.Changes) != 2 || history.Changes[1].Task.State != "IN_PROGRESS" || history.Changes[1].Task.Coordination != nil {
		t.Fatal(history, err)
	}
	if replay := mustOffer(t, d, team, a, c, "offer"); !reflect.DeepEqual(replay, out) {
		t.Fatal("offer receipt changed")
	}
	if replay, err := d.ChangeTeamAssignment(team.ID, out.ID, "accept", cmd, a, "accept", allowAssignmentWorker); err != nil || !reflect.DeepEqual(replay, run) {
		t.Fatal(replay, err)
	}
	c.CostRationale = "different"
	if _, err := d.OfferTeamAssignment(team.ID, c, a, "offer", allowAssignmentWorker); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatal(err)
	}
	if _, err := d.ChangeTeamAssignment(team.ID, out.ID, "withdraw", TeamAssignmentCommand{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: 1, Reason: "cancel"}, a, "withdraw", nil); !errors.Is(err, ErrTeamAssignmentConflict) {
		t.Fatal(err)
	}
}

func TestTeamAssignmentGates(t *testing.T) {
	for _, scenario := range []string{"worker-offer", "co-generation", "session-generation", "worker-generation", "reviewer", "unavailable", "missing-authority", "revoked-authority", "revision", "not-ready", "dependency", "foreign-dependency", "rationale", "wrong-accept", "token-binding", "disabled", "unenrolled"} {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c := assignmentFixture(t)
			authorize := WorkerAuthority(allowAssignmentWorker)
			switch scenario {
			case "worker-offer":
				c.TeamSessionHandle = c.Worker
			case "co-generation":
				c.CoordinatorGeneration++
			case "session-generation":
				c.Generation++
			case "worker-generation":
				c.Worker.Generation++
			case "reviewer":
				s, err := d.JoinTeam(team.ID, "reviewer", testProfile(), a, "reviewer")
				if err != nil {
					t.Fatal(err)
				}
				c.Worker = TeamSessionHandle{s.ID, s.Generation}
			case "unavailable":
				_, err := d.ChangeTeamSession(team.ID, "heartbeat", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1, Availability: "unavailable"}, a, "unavailable")
				if err != nil {
					t.Fatal(err)
				}
			case "missing-authority":
				authorize = nil
			case "revoked-authority":
				authorize = func(s TeamSession) error {
					if s.ID != c.Worker.SessionID || s.TokenID != a.Actor.TokenID {
						t.Fatal("wrong authorization target")
					}
					return ErrTeamSessionDenied
				}
			case "revision":
				c.ExpectedRevision++
			case "not-ready", "dependency", "foreign-dependency":
				task, _ := d.GetTask(c.TaskID)
				if scenario == "not-ready" {
					task.State = "BACKLOG"
				} else {
					depDB := d
					if scenario == "foreign-dependency" {
						var err error
						depDB, err = r.Open("foreign")
						if err != nil {
							t.Fatal(err)
						}
					}
					dep, err := depDB.CreateTask(TaskContent{Title: "dependency", State: "READY"}, a.Actor, "dep")
					if err != nil {
						t.Fatal(err)
					}
					task.Dependencies = []string{dep.ID}
				}
				up, err := d.UpdateTask(task.ID, task.TaskContent, task.Revision, a.Actor, "edit")
				if err != nil {
					t.Fatal(err)
				}
				c.ExpectedRevision = up.Revision
			case "rationale":
				c.SuitabilityRationale = ""
			case "wrong-accept", "token-binding":
				out := mustOffer(t, d, team, a, c, "offer")
				cmd := TeamAssignmentCommand{TeamSessionHandle: c.Worker}
				if scenario == "wrong-accept" {
					cmd.TeamSessionHandle = messageMember(t, d, team, a, "other")
				} else {
					a.Actor.TokenID = uuidv7.New()
				}
				if _, err := d.ChangeTeamAssignment(team.ID, out.ID, "accept", cmd, a, "accept", authorize); err == nil {
					t.Fatal("accepted unauthorized worker")
				}
				return
			case "disabled":
				if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "unenrolled":
				team.Enrollment = nil
				if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, team.TeamContent, admin, "remove"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.OfferTeamAssignment(team.ID, c, a, "offer", authorize); err == nil {
				t.Fatal("invalid offer accepted")
			}
			task, err := d.GetTask(c.TaskID)
			if err != nil || task.Coordination != nil {
				t.Fatal("failed offer reserved", task, err)
			}
		})
	}
}

func TestTeamAssignmentAcceptanceRechecksAndStickyManagement(t *testing.T) {
	for _, change := range []string{"authority", "dependency", "availability", "resume", "leave", "disable", "enrollment"} {
		t.Run(change, func(t *testing.T) {
			r, d, team, a, admin, c := assignmentFixture(t)
			dep, err := d.CreateTask(TaskContent{Title: "dependency", State: "DONE"}, a.Actor, "dep")
			if err != nil {
				t.Fatal(err)
			}
			task, _ := d.GetTask(c.TaskID)
			task.Dependencies = []string{dep.ID}
			task, err = d.UpdateTask(task.ID, task.TaskContent, 1, a.Actor, "deps")
			if err != nil {
				t.Fatal(err)
			}
			c.ExpectedRevision = task.Revision
			out := mustOffer(t, d, team, a, c, "offer")
			authorize := WorkerAuthority(allowAssignmentWorker)
			switch change {
			case "authority":
				authorize = func(TeamSession) error { return ErrTeamSessionDenied }
			case "dependency":
				dep.State = "READY"
				if _, err = d.UpdateTask(dep.ID, dep.TaskContent, dep.Revision, a.Actor, "reopen"); err != nil {
					t.Fatal(err)
				}
			case "availability":
				_, err = d.ChangeTeamSession(team.ID, "heartbeat", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1, Availability: "unavailable"}, a, "busy")
			case "resume", "leave":
				_, err = d.ChangeTeamSession(team.ID, change, TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, change)
			case "disable":
				err = d.SetMeta(TasksMetaKey, "off")
			case "enrollment":
				team.Enrollment = nil
				_, err = r.ConfigureTeam("alpha", team.ID, team.Revision, team.TeamContent, admin, "remove")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.ChangeTeamAssignment(team.ID, out.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", authorize); err == nil {
				t.Fatal("accepted after authority/readiness changed")
			}
			task, err = d.GetTask(c.TaskID)
			if err != nil || task.State != "READY" || task.Coordination.AttemptID != out.ID {
				t.Fatal(task, err)
			}
		})
	}
	_, d, team, a, _, c := assignmentFixture(t)
	for _, op := range []string{"decline", "withdraw"} {
		out := mustOffer(t, d, team, a, c, "offer-"+op)
		cmd := TeamAssignmentCommand{TeamSessionHandle: c.Worker, Reason: "reassess"}
		if op == "withdraw" {
			cmd.TeamSessionHandle = c.TeamSessionHandle
			cmd.CoordinatorGeneration = c.CoordinatorGeneration
		}
		closed, err := d.ChangeTeamAssignment(team.ID, out.ID, op, cmd, a, op, nil)
		if err != nil {
			t.Fatal(err)
		}
		if replay, err := d.ChangeTeamAssignment(team.ID, out.ID, op, cmd, a, op, nil); err != nil || !reflect.DeepEqual(replay, closed) {
			t.Fatal(replay, err)
		}
		task, err := d.GetTask(c.TaskID)
		if err != nil || task.Coordination == nil || task.Coordination.AttemptID != "" || task.State != "READY" {
			t.Fatal(task, err)
		}
		if _, err = d.UpdateTask(task.ID, task.TaskContent, task.Revision, a.Actor, "bypass-"+op); !errors.Is(err, ErrManagedTask) {
			t.Fatal(err)
		}
	}
}

func TestTeamAssignmentRaces(t *testing.T) {
	for _, kind := range []string{"cross-team-offer", "capacity", "accept-withdraw", "accept-decline", "decline-withdraw", "generic-update"} {
		t.Run(kind, func(t *testing.T) {
			r, d, team, a, admin, c := assignmentFixture(t)
			peerRegistry, err := NewRegistry(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer peerRegistry.Close()
			peerDB, err := peerRegistry.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			var calls [2]func() error
			calls[0] = func() error {
				_, err := d.OfferTeamAssignment(team.ID, c, a, "offer", allowAssignmentWorker)
				return err
			}
			switch kind {
			case "cross-team-offer":
				other, err := r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "other", Enrollment: team.Enrollment}, admin, "other")
				if err != nil {
					t.Fatal(err)
				}
				co, err := d.JoinTeam(other.ID, "coordinator", testProfile(), a, "other-co")
				if err != nil {
					t.Fatal(err)
				}
				c2 := c
				c2.TeamSessionHandle = TeamSessionHandle{co.ID, 1}
				c2.Worker = messageMember(t, d, other, a, "other-worker")
				calls[1] = func() error {
					_, err := peerDB.OfferTeamAssignment(other.ID, c2, a, "offer", allowAssignmentWorker)
					return err
				}
			case "capacity":
				task, err := d.CreateTask(TaskContent{Title: "other", State: "READY"}, a.Actor, "other")
				if err != nil {
					t.Fatal(err)
				}
				c2 := c
				c2.TaskID = task.ID
				calls[1] = func() error {
					_, err := peerDB.OfferTeamAssignment(team.ID, c2, a, "offer2", allowAssignmentWorker)
					return err
				}
			case "generic-update":
				calls[1] = func() error {
					_, err := peerDB.UpdateTask(c.TaskID, TaskContent{Title: "edited", State: "READY"}, 1, admin.Actor, "generic")
					return err
				}
			default:
				out := mustOffer(t, d, team, a, c, "offer")
				worker := TeamAssignmentCommand{TeamSessionHandle: c.Worker}
				withdraw := TeamAssignmentCommand{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: 1, Reason: "reassess"}
				calls[0] = func() error {
					_, err := d.ChangeTeamAssignment(team.ID, out.ID, "accept", worker, a, "accept", allowAssignmentWorker)
					return err
				}
				calls[1] = func() error {
					_, err := peerDB.ChangeTeamAssignment(team.ID, out.ID, "withdraw", withdraw, a, "withdraw", nil)
					return err
				}
				decline := func() error {
					cmd := worker
					cmd.Reason = "not suitable"
					_, err := d.ChangeTeamAssignment(team.ID, out.ID, "decline", cmd, a, "decline", nil)
					return err
				}
				if kind == "accept-decline" {
					calls[1] = decline
				}
				if kind == "decline-withdraw" {
					calls[0] = decline
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			for i := range calls {
				wg.Add(1)
				go func(i int) { defer wg.Done(); <-start; errs[i] = calls[i]() }(i)
			}
			close(start)
			wg.Wait()
			wins := 0
			for _, err := range errs {
				if err == nil {
					wins++
				} else {
					var conflict *TaskConflict
					if !errors.Is(err, ErrTeamAssignmentConflict) && !errors.Is(err, ErrManagedTask) && !errors.As(err, &conflict) {
						t.Fatal("unexpected losing error", err)
					}
				}
			}
			if wins != 1 {
				t.Fatalf("winners %d errors %v", wins, errs)
			}
		})
	}
}

func assignmentCounts(t *testing.T, d *DB) []int {
	t.Helper()
	out := []int{}
	for _, table := range []string{"team_managed_tasks", "team_assignments", "task_history", "team_events", "task_requests"} {
		var n int
		if err := d.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestTeamAssignmentRollback(t *testing.T) {
	for _, op := range []string{"offer", "accept", "decline", "withdraw"} {
		for _, table := range []string{"team_events", "task_requests"} {
			t.Run(op+"/"+table, func(t *testing.T) {
				_, d, team, a, _, c := assignmentFixture(t)
				var out TeamAssignment
				if op != "offer" {
					out = mustOffer(t, d, team, a, c, "offer")
				}
				before := assignmentCounts(t, d)
				if _, err := d.sql.Exec(`CREATE TRIGGER fail_assignment BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				call := func() error {
					if op == "offer" {
						_, err := d.OfferTeamAssignment(team.ID, c, a, "effect", allowAssignmentWorker)
						return err
					}
					cmd := TeamAssignmentCommand{TeamSessionHandle: c.Worker}
					if op != "accept" {
						cmd.Reason = "reassess"
					}
					if op == "withdraw" {
						cmd.TeamSessionHandle = c.TeamSessionHandle
						cmd.CoordinatorGeneration = 1
					}
					_, err := d.ChangeTeamAssignment(team.ID, out.ID, op, cmd, a, "effect", allowAssignmentWorker)
					return err
				}
				if err := call(); err == nil {
					t.Fatal("injection did not fail")
				}
				if after := assignmentCounts(t, d); !reflect.DeepEqual(before, after) {
					t.Fatal("partial transaction", before, after)
				}
				task, err := d.GetTask(c.TaskID)
				if err != nil || task.Revision != 1 || task.State != "READY" {
					t.Fatal(task, err)
				}
				if op != "offer" {
					saved, err := d.GetTeamAssignment(team.ID, out.ID, c.TeamSessionHandle, a.Actor)
					if err != nil || saved.State != "OFFERED" {
						t.Fatal(saved, err)
					}
				}
				if _, err := d.sql.Exec(`DROP TRIGGER fail_assignment`); err != nil {
					t.Fatal(err)
				}
				if err := call(); err != nil {
					t.Fatal("rollback consumed retry key", err)
				}
			})
		}
	}
}

func TestTeamAssignmentMigrationAndReopen(t *testing.T) {
	r, d, team, a, _, c := assignmentFixture(t)
	rewindReservationSchema(t, d)
	for _, q := range []string{`DROP TABLE team_assignments`, `DROP TABLE team_managed_tasks`, `UPDATE meta SET value='16' WHERE key='schema_version'`} {
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
	out := mustOffer(t, d, team, a, c, "offer")
	if _, err := d.ChangeTeamAssignment(team.ID, out.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker); err != nil {
		t.Fatal(err)
	}
	// Liveness and lifecycle operations are not ownership-release operations.
	for _, op := range []string{"resume", "leave"} {
		generation := int64(1)
		if op == "leave" {
			generation = 2
		}
		if _, err := d.ChangeTeamSession(team.ID, op, TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: generation}, a, op); err != nil {
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
	d2, err := r2.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	task, err := d2.GetTask(c.TaskID)
	if err != nil || task.Coordination == nil || task.Coordination.AttemptID != out.ID || task.Coordination.State != "RUNNING" {
		t.Fatal(task, err)
	}
	if _, err := d2.UpdateTask(task.ID, TaskContent{Title: "late writer", State: "DONE"}, task.Revision, a.Actor, "late"); !errors.Is(err, ErrManagedTask) {
		t.Fatal(err)
	}
	if replay, err := d2.ChangeTeamAssignment(team.ID, out.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, a, "accept", allowAssignmentWorker); err != nil || replay.State != "RUNNING" {
		t.Fatal(replay, err)
	}
}
