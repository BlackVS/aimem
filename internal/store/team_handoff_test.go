package store

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

func handoffCommand(c TeamOffer, target TeamSessionHandle) TeamHandoffCommand {
	return TeamHandoffCommand{TeamSessionHandle: c.TeamSessionHandle, CoordinatorGeneration: c.CoordinatorGeneration, Target: target, Reason: "Coordinator session ends its shift"}
}

func adminHandoff(cmd TeamHandoffCommand) TeamHandoffCommand {
	cmd.Reconciliation = &TeamHandoffReconciliation{CoordinatorStopped: true, LivenessCheck: "Fixture: backend session closed, no pending coordinator command", EvidenceRefs: []TaskRef{{Kind: "text", Ref: "Disposable reconciliation fixture"}}}
	return cmd
}

// handoffFixture has a RUNNING attempt on c.Worker and an idle designated
// successor session of the same user.
func handoffFixture(t *testing.T) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext, TeamOffer, TeamAssignment, TeamSessionHandle) {
	t.Helper()
	r, d, team, a, admin, c, out := runningResultFixture(t)
	return r, d, team, a, admin, c, out, messageMember(t, d, team, a, "successor")
}

func handoffEvents(t *testing.T, d *DB, teamID string) []TeamEvent {
	t.Helper()
	events, err := d.TeamEvents(teamID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := []TeamEvent{}
	for _, e := range events {
		if e.Operation == "team.coordinator.handoff" {
			out = append(out, e)
		}
	}
	return out
}

func activeCoordinators(t *testing.T, d *DB, teamID string) (int, int64) {
	t.Helper()
	var n int
	var generation int64
	if err := d.sql.QueryRow(`SELECT count(*) FROM team_sessions WHERE team_id=? AND role='coordinator' AND state='active'`, teamID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := d.sql.QueryRow(`SELECT coordinator_generation FROM team_session_control WHERE team_id=?`, teamID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	return n, generation
}

func TestTeamHandoffTransfersSlot(t *testing.T) {
	for _, path := range []string{"coordinator", "admin"} {
		t.Run(path, func(t *testing.T) {
			_, d, team, a, admin, c, out, successor := handoffFixture(t)
			cmd, actor := handoffCommand(c, successor), a
			if path == "admin" {
				cmd, actor = adminHandoff(cmd), admin
			}
			workerBefore, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			taskBefore, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			counts := assignmentCounts(t, d)
			toBefore, err := readTeamSession(d.sql, team.ID, successor.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			fromBefore, err := readTeamSession(d.sql, team.ID, c.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			to, err := d.HandoffTeamCoordinator(team.ID, cmd, actor, "handoff")
			if err != nil || to.ID != successor.SessionID || to.Role != "coordinator" || to.CoordinatorGeneration != 2 || to.Generation != successor.Generation || to.State != "active" {
				t.Fatalf("handoff: %+v %v", to, err)
			}
			// Only role and slot generation change on the successor; liveness,
			// availability and profile are untouched by a transfer it did not perform.
			same := to
			same.Role, same.CoordinatorGeneration = toBefore.Role, toBefore.CoordinatorGeneration
			if !reflect.DeepEqual(same, toBefore) {
				t.Fatalf("handoff changed unrelated successor fields:\n%+v\n%+v", same, toBefore)
			}
			from, err := readTeamSession(d.sql, team.ID, c.SessionID)
			if err != nil || from.State != "left" || from.Generation != c.Generation+1 || from.Role != "coordinator" || from.CoordinatorGeneration != 1 || from.LastSeenAt != fromBefore.LastSeenAt {
				t.Fatalf("outgoing: %+v %v", from, err)
			}
			if n, g := activeCoordinators(t, d, team.ID); n != 1 || g != 2 {
				t.Fatal("slot", n, g)
			}
			events := handoffEvents(t, d, team.ID)
			if len(events) != 1 || events[0].Handoff == nil || events[0].Handoff.From != c.TeamSessionHandle || events[0].Handoff.To != successor || events[0].Handoff.PreviousCoordinatorGeneration != 1 || events[0].Handoff.CoordinatorGeneration != 2 || events[0].Session.ID != to.ID || events[0].Actor.Kind != actor.Actor.Kind || events[0].RequestID != actor.RequestID || (events[0].Handoff.Reconciliation != nil) != (path == "admin") {
				t.Fatalf("handoff audit: %+v", events)
			}
			after := assignmentCounts(t, d)
			if after[0] != counts[0] || after[1] != counts[1] || after[2] != counts[2] || after[3] != counts[3]+1 || after[4] != counts[4]+1 {
				t.Fatal("handoff row counts", counts, after)
			}
			// Worker attempt, task and worker session are untouched.
			got, err := readTeamAssignment(d.sql, team.ID, out.ID)
			if err != nil || !reflect.DeepEqual(got, out) {
				t.Fatal("attempt changed", got, err)
			}
			if task, err := d.GetTask(c.TaskID); err != nil || !reflect.DeepEqual(task, taskBefore) {
				t.Fatal("task changed", task, err)
			}
			if worker, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID); err != nil || !reflect.DeepEqual(worker, workerBefore) {
				t.Fatal("worker session changed", worker, err)
			}
			// The old coordinator handle is stale for commands and reads.
			if _, err := d.OfferTeamAssignment(team.ID, c, a, "old-offer", allowAssignmentWorker); !errors.Is(err, ErrTeamSessionStale) {
				t.Fatal("old coordinator offered", err)
			}
			if _, err := d.ChangeTeamWork(team.ID, out.ID, "cancel", workCommand(c, "cancel", 2), a, "old-cancel"); !errors.Is(err, ErrTeamSessionStale) {
				t.Fatal("old coordinator cancelled", err)
			}
			if _, err := d.TeamRoster(team.ID, c.SessionID, c.Generation, a.Actor, "", 100); !errors.Is(err, ErrTeamSessionStale) {
				t.Fatal(err)
			}
			// The successor commands the surviving attempt under the new generation.
			next := c
			next.TeamSessionHandle, next.CoordinatorGeneration = TeamSessionHandle{to.ID, to.Generation}, to.CoordinatorGeneration
			stale := workCommand(next, "cancel", 2)
			stale.CoordinatorGeneration = 1
			if _, err := d.ChangeTeamWork(team.ID, out.ID, "cancel", stale, a, "stale-cancel"); !errors.Is(err, ErrTeamSessionStale) {
				t.Fatal("old slot generation accepted", err)
			}
			cancelled, err := d.ChangeTeamWork(team.ID, out.ID, "cancel", workCommand(next, "cancel", 2), a, "new-cancel")
			if err != nil || cancelled.State != "STOP_REQUESTED" || cancelled.CoordinatorGeneration != 1 {
				t.Fatal("successor could not command existing attempt", cancelled, err)
			}
			if _, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "second-coordinator"); !errors.Is(err, ErrTeamCoordinatorOccupied) {
				t.Fatal(err)
			}
			// Replays return the original snapshot; changed content conflicts.
			if replay, err := d.HandoffTeamCoordinator(team.ID, cmd, actor, "handoff"); err != nil || !reflect.DeepEqual(replay, to) || len(handoffEvents(t, d, team.ID)) != 1 {
				t.Fatal("replay", replay, err)
			}
			changed := cmd
			changed.Reason = "different"
			if _, err := d.HandoffTeamCoordinator(team.ID, changed, actor, "handoff"); !errors.Is(err, ErrTaskRetryConflict) {
				t.Fatal(err)
			}
			if _, err := d.HandoffTeamCoordinator(team.ID, cmd, actor, "again"); !errors.Is(err, ErrTeamSessionStale) {
				t.Fatal("handoff repeated from a closed session", err)
			}
			// The outgoing principal returns as a worker with a fresh join; the slot
			// continues its generation after the successor leaves cleanly.
			if _, err := d.JoinTeam(team.ID, "worker", testProfile(), a, "return"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ChangeTeamSession(team.ID, "leave", TeamSessionCommand{SessionID: to.ID, Generation: to.Generation}, a, "successor-leave"); err != nil {
				t.Fatal(err)
			}
			if third, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "third"); err != nil || third.CoordinatorGeneration != 3 {
				t.Fatal(third, err)
			}
		})
	}
}

func TestTeamHandoffFences(t *testing.T) {
	scenarios := []string{"session-generation", "coordinator-generation", "target-generation", "target-self", "target-reserved", "target-not-designated", "target-left", "target-unknown", "wrong-team", "unenrolled-caller", "disabled", "forged-admin", "ordinary-with-evidence", "admin-without-evidence", "admin-stale-slot", "coordinator-resumed", "not-stopped", "no-liveness", "no-refs", "bad-ref", "empty-reason"}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c, _, successor := handoffFixture(t)
			cmd, actor, wantInvalid := handoffCommand(c, successor), a, false
			switch scenario {
			case "session-generation":
				cmd.Generation++
			case "coordinator-generation":
				cmd.CoordinatorGeneration++
			case "target-generation":
				cmd.Target.Generation++
			case "target-self":
				cmd.Target, wantInvalid = c.TeamSessionHandle, true
			case "target-reserved":
				cmd.Target = c.Worker
			case "target-not-designated":
				other := a
				other.Actor.UserID, other.Actor.TokenID = uuidv7.New(), uuidv7.New()
				content := team.TeamContent
				content.Enrollment = append(content.Enrollment, TeamEnrollment{UserID: other.Actor.UserID})
				if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, content, admin, "enroll-other"); err != nil {
					t.Fatal(err)
				}
				cmd.Target = messageMember(t, d, team, other, "other")
			case "target-left":
				if _, err := d.ChangeTeamSession(team.ID, "leave", TeamSessionCommand{SessionID: successor.SessionID, Generation: successor.Generation}, a, "leave"); err != nil {
					t.Fatal(err)
				}
			case "target-unknown":
				cmd.Target.SessionID = uuidv7.New()
			case "wrong-team":
				team.ID = uuidv7.New()
			case "unenrolled-caller":
				content := team.TeamContent
				content.Enrollment = nil
				if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, content, admin, "unenroll"); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "forged-admin":
				actor = admin
				actor.Actor.UserID = a.Actor.UserID
				cmd = adminHandoff(cmd)
			case "ordinary-with-evidence":
				cmd, wantInvalid = adminHandoff(cmd), true
			case "admin-without-evidence":
				actor, wantInvalid = admin, true
			case "admin-stale-slot":
				if _, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.SessionID, Generation: c.Generation}, a, "co-resume"); err != nil {
					t.Fatal(err)
				}
				cmd, actor = adminHandoff(cmd), admin
			case "coordinator-resumed":
				if _, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: c.SessionID, Generation: c.Generation}, a, "co-resume"); err != nil {
					t.Fatal(err)
				}
			case "not-stopped":
				cmd, actor, wantInvalid = adminHandoff(cmd), admin, true
				cmd.Reconciliation.CoordinatorStopped = false
			case "no-liveness":
				cmd, actor, wantInvalid = adminHandoff(cmd), admin, true
				cmd.Reconciliation.LivenessCheck = " "
			case "no-refs":
				cmd, actor, wantInvalid = adminHandoff(cmd), admin, true
				cmd.Reconciliation.EvidenceRefs = nil
			case "bad-ref":
				cmd, actor, wantInvalid = adminHandoff(cmd), admin, true
				cmd.Reconciliation.EvidenceRefs[0] = TaskRef{Kind: "ci", Ref: "invalid"}
			case "empty-reason":
				cmd.Reason, wantInvalid = "", true
			}
			counts := assignmentCounts(t, d)
			before, err := d.TeamEvents(team.ID, 0, 100)
			if err != nil && scenario != "wrong-team" {
				t.Fatal(err)
			}
			_, err = d.HandoffTeamCoordinator(team.ID, cmd, actor, "handoff")
			if err == nil || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("invalid handoff accepted", err)
			}
			if wantInvalid != errors.Is(err, ErrTaskInvalid) {
				t.Fatal(scenario, err)
			}
			if scenario == "target-reserved" && !errors.Is(err, ErrTeamAssignmentConflict) {
				t.Fatal(err)
			}
			if scenario != "wrong-team" {
				if n, g := activeCoordinators(t, d, team.ID); n != 1 || (g != 1 && !strings.Contains(scenario, "resumed") && scenario != "admin-stale-slot") {
					t.Fatal("slot changed", n, g)
				}
				if after, err := d.TeamEvents(team.ID, 0, 100); err != nil || len(after) != len(before) {
					t.Fatal("event written", err)
				}
			}
		})
	}
}

func TestTeamHandoffRollback(t *testing.T) {
	triggers := map[string]string{
		"outgoing":  `BEFORE UPDATE ON team_sessions WHEN NEW.state='left'`,
		"successor": `BEFORE UPDATE ON team_sessions WHEN NEW.role='coordinator' AND NEW.state='active'`,
		"control":   `BEFORE UPDATE ON team_session_control`,
		"event":     `BEFORE INSERT ON team_events`,
		"receipt":   `BEFORE INSERT ON task_requests`,
	}
	for _, boundary := range []string{"outgoing", "successor", "control", "event", "receipt"} {
		t.Run(boundary, func(t *testing.T) {
			_, d, team, a, _, c, _, successor := handoffFixture(t)
			cmd := handoffCommand(c, successor)
			from, err := readTeamSession(d.sql, team.ID, c.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			to, err := readTeamSession(d.sql, team.ID, successor.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			counts := assignmentCounts(t, d)
			if _, err := d.sql.Exec(`CREATE TRIGGER fail_handoff ` + triggers[boundary] + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.HandoffTeamCoordinator(team.ID, cmd, a, "handoff"); err == nil {
				t.Fatal("injection did not fail")
			}
			if got, err := readTeamSession(d.sql, team.ID, c.SessionID); err != nil || !reflect.DeepEqual(got, from) {
				t.Fatal("partial outgoing", got, err)
			}
			if got, err := readTeamSession(d.sql, team.ID, successor.SessionID); err != nil || !reflect.DeepEqual(got, to) {
				t.Fatal("partial successor", got, err)
			}
			if n, g := activeCoordinators(t, d, team.ID); n != 1 || g != 1 || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("partial slot", n, g)
			}
			if _, err := d.sql.Exec(`DROP TRIGGER fail_handoff`); err != nil {
				t.Fatal(err)
			}
			if got, err := d.HandoffTeamCoordinator(team.ID, cmd, a, "handoff"); err != nil || got.CoordinatorGeneration != 2 {
				t.Fatal("rollback consumed key", got, err)
			}
		})
	}
}

func TestTeamHandoffRaces(t *testing.T) {
	for _, scenario := range []string{"handoff-handoff", "handoff-offer", "handoff-accept", "handoff-admin"} {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c, _, successor := handoffFixture(t)
			peer, err := NewRegistry(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			d2, err := peer.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			second := messageMember(t, d, team, a, "second")
			offer := c
			offer.Worker = successor
			var pending TeamAssignment
			if scenario == "handoff-accept" {
				task, err := d.CreateTask(TaskContent{Title: "second task", State: "READY"}, a.Actor, "task-2")
				if err != nil {
					t.Fatal(err)
				}
				offer.TaskID, offer.ExpectedRevision = task.ID, task.Revision
				pending = mustOffer(t, d, team, a, offer, "pending")
			}
			handoff := func() error {
				_, err := d.HandoffTeamCoordinator(team.ID, handoffCommand(c, successor), a, "handoff")
				return err
			}
			var other func() error
			switch scenario {
			case "handoff-handoff":
				other = func() error {
					_, err := d2.HandoffTeamCoordinator(team.ID, handoffCommand(c, second), a, "other")
					return err
				}
			case "handoff-offer":
				other = func() error {
					task, err := d2.CreateTask(TaskContent{Title: "late task", State: "READY"}, a.Actor, "task-late")
					if err != nil {
						return err
					}
					offer.TaskID, offer.ExpectedRevision = task.ID, task.Revision
					_, err = d2.OfferTeamAssignment(team.ID, offer, a, "late-offer", allowAssignmentWorker)
					return err
				}
			case "handoff-accept":
				other = func() error {
					_, err := d2.ChangeTeamAssignment(team.ID, pending.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: successor}, a, "accept", allowAssignmentWorker)
					return err
				}
			case "handoff-admin":
				other = func() error {
					_, err := d2.HandoffTeamCoordinator(team.ID, adminHandoff(handoffCommand(c, second)), admin, "admin")
					return err
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			for i, call := range []func() error{handoff, other} {
				wg.Add(1)
				go func(i int, call func() error) { defer wg.Done(); <-start; errs[i] = call() }(i, call)
			}
			close(start)
			wg.Wait()
			wins := 0
			for _, err := range errs {
				if err == nil {
					wins++
					continue
				}
				if !errors.Is(err, ErrTeamSessionStale) && !errors.Is(err, ErrTeamAssignmentConflict) && !errors.Is(err, ErrTeamSessionDenied) {
					t.Fatal(err)
				}
			}
			if wins != 1 {
				t.Fatal("race winners", wins, errs)
			}
			n, generation := activeCoordinators(t, d, team.ID)
			if n != 1 {
				t.Fatal("coordinator rows", n)
			}
			to, err := readTeamSession(d.sql, team.ID, successor.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case errs[0] == nil:
				if generation != 2 || to.Role != "coordinator" {
					t.Fatal("handoff winner state", generation, to)
				}
			case scenario == "handoff-handoff" || scenario == "handoff-admin":
				if generation != 2 || to.Role != "worker" {
					t.Fatal("other handoff winner state", generation, to)
				}
			default:
				var reserved bool
				if err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_assignments WHERE worker_id=? AND reserved=1)`, successor.SessionID).Scan(&reserved); err != nil || !reserved || generation != 1 || to.Role != "worker" {
					t.Fatal("reservation winner state", reserved, generation, to, err)
				}
			}
		})
	}
}

func TestTeamHandoffReopen(t *testing.T) {
	r, d, team, a, _, c, out, successor := handoffFixture(t)
	to, err := d.HandoffTeamCoordinator(team.ID, handoffCommand(c, successor), a, "handoff")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SubmitTeamResult(team.ID, out.ID, resultSubmission(c), a, "submit"); err != nil {
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
	if n, g := activeCoordinators(t, d2, team.ID); n != 1 || g != 2 {
		t.Fatal(n, g)
	}
	roster, err := d2.TeamRoster(team.ID, to.ID, to.Generation, a.Actor, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, s := range roster {
		roles[s.ID] = s.Role + "/" + s.State
	}
	if roles[c.SessionID] != "coordinator/left" || roles[to.ID] != "coordinator/active" || roles[c.Worker.SessionID] != "worker/active" {
		t.Fatal(roles)
	}
	submitted, err := d2.GetTeamAssignment(team.ID, out.ID, TeamSessionHandle{to.ID, to.Generation}, a.Actor)
	if err != nil {
		t.Fatal(err)
	}
	decision := resultDecision(c, submitted, "accept")
	decision.TeamSessionHandle, decision.CoordinatorGeneration = TeamSessionHandle{to.ID, to.Generation}, to.CoordinatorGeneration
	if got, err := d2.ReviewTeamResult(team.ID, out.ID, decision, a, "review"); err != nil || got.State != "ACCEPTED" || got.Review.CoordinatorGeneration != 2 {
		t.Fatal("successor could not review after reopen", got, err)
	}
	if replay, err := d2.HandoffTeamCoordinator(team.ID, handoffCommand(c, successor), a, "handoff"); err != nil || replay.ID != to.ID {
		t.Fatal("receipt after reopen", replay, err)
	}
	if events := handoffEvents(t, d2, team.ID); len(events) != 1 || events[0].Handoff.To != successor {
		t.Fatal(events)
	}
}
