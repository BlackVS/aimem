package store

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

func tokenRebind(session TeamSessionHandle, token string) TeamTokenRebindCommand {
	return TeamTokenRebindCommand{SessionID: session.SessionID, ExpectedGeneration: session.Generation, TokenID: token,
		Reconciliation: TeamTokenRebindEvidence{OldCredentialStopped: true, Reason: "Credential revoked after a leak report", RuntimeCheck: "Old client stopped; no command in flight", EvidenceRefs: []TaskRef{{Kind: "text", Ref: "Operator checked the host"}}}}
}

// rebound returns the same user acting with the replacement token.
func rebound(a TeamAuditContext, token string) TeamAuditContext {
	a.Actor.TokenID = token
	return a
}

func TestTokenRebindCarriesWorkToTheReplacementCredential(t *testing.T) {
	_, d, team, a, admin, c, run := runningResultFixture(t)
	token := uuidv7.New()
	counts := assignmentCounts(t, d)
	s, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "rebind", allowAssignmentWorker)
	if err != nil || s.TokenID != token || s.Generation != 2 || s.State != "active" || s.UserID != a.Actor.UserID {
		t.Fatalf("rebind: %+v %v", s, err)
	}
	after := assignmentCounts(t, d)
	// Session row updated in place; rebind-token event + attempt rebind event; one receipt.
	if after[1] != counts[1] || after[3] != counts[3]+2 || after[4] != counts[4]+1 {
		t.Fatal("row counts", counts, after)
	}
	fresh := rebound(a, token)
	current := TeamSessionHandle{SessionID: s.ID, Generation: s.Generation}
	// The old credential can use no handle; the replacement commands the same attempt.
	if _, err := d.ReservedTeamAssignment(team.ID, current, a.Actor); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal("old credential read the attempt", err)
	}
	if _, err := d.ChangeTeamWork(team.ID, run.ID, "block", workCommand(c, "block", 2), a, "old-block"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal("old credential commanded the attempt", err)
	}
	got, err := d.ReservedTeamAssignment(team.ID, current, fresh.Actor)
	if err != nil || got.ID != run.ID || got.Worker != current || got.RebindCount != 1 {
		t.Fatal(got, err)
	}
	cmd := workCommand(c, "block", 2)
	cmd.TeamSessionHandle = current
	if got, err := d.ChangeTeamWork(team.ID, run.ID, "block", cmd, fresh, "new-block"); err != nil || got.State != "BLOCKED" {
		t.Fatal(got, err)
	}
	// The coordinator learned of the rebind through its inbox (admin actor: both parties).
	inbox := lifecycleOnly(inboxOf(t, d, team, a, c.TeamSessionHandle))
	found := false
	for _, m := range inbox {
		if m.Lifecycle.Operation == "team.assignment.rebind" && m.Lifecycle.ActorKind == "admin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("coordinator inbox: %+v", inbox)
	}
	// The audit event records the transfer with the reconciliation; the receipt replays.
	events, err := d.TeamEvents(team.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, e := range events {
		if e.Operation == "team.session.rebind_token" && e.TokenRebind != nil && e.TokenRebind.TokenID == token && e.TokenRebind.PreviousTokenID == a.Actor.TokenID && e.PreviousGeneration == 1 && e.Actor.Kind == "admin" && e.TokenRebind.Reconciliation.OldCredentialStopped {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("rebind event")
	}
	if replay, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "rebind", allowAssignmentWorker); err != nil || !reflect.DeepEqual(replay, s) {
		t.Fatal("replay", replay, err)
	}
	if _, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "again", allowAssignmentWorker); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal("rebound twice from a stale handle", err)
	}
	// A receipt recorded under the old credential does not replay for the new one.
	if _, err := d.ChangeTeamAssignment(team.ID, run.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: c.Worker}, fresh, "accept", allowAssignmentWorker); err == nil {
		t.Fatal("old credential's receipt replayed for the replacement")
	}
}

func TestTokenRebindCoordinator(t *testing.T) {
	_, d, team, a, admin, c := assignmentFixture(t)
	token := uuidv7.New()
	s, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.TeamSessionHandle, token), admin, "rebind", allowAssignmentWorker)
	if err != nil || s.Role != "coordinator" || s.Generation != 2 || s.CoordinatorGeneration != 2 {
		t.Fatalf("coordinator rebind: %+v %v", s, err)
	}
	// Old handle and old credential are stale; the replacement offers with the new generations.
	if _, err := d.OfferTeamAssignment(team.ID, c, a, "old-offer", allowAssignmentWorker); err == nil {
		t.Fatal("old credential offered")
	}
	fresh := rebound(a, token)
	c.TeamSessionHandle, c.CoordinatorGeneration = TeamSessionHandle{s.ID, s.Generation}, s.CoordinatorGeneration
	if _, err := d.OfferTeamAssignment(team.ID, c, fresh, "new-offer", allowAssignmentWorker); err != nil {
		t.Fatal(err)
	}
	if n, g := activeCoordinators(t, d, team.ID); n != 1 || g != 2 {
		t.Fatal(n, g)
	}
}

func TestTokenRebindFences(t *testing.T) {
	for _, scenario := range []string{"ordinary", "generation", "same-token", "left", "unenrolled", "unknown-session", "unknown-team", "disabled", "not-stopped", "no-refs", "bad-ref", "no-reason", "no-token", "authorize-refused", "no-authorize"} {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c, _ := runningResultFixture(t)
			cmd, actor, authorize, wantInvalid := tokenRebind(c.Worker, uuidv7.New()), admin, WorkerAuthority(allowAssignmentWorker), false
			sessionTeam := team.ID
			switch scenario {
			case "ordinary":
				actor = a
			case "generation":
				cmd.ExpectedGeneration++
			case "same-token":
				cmd.TokenID, wantInvalid = a.Actor.TokenID, true
			case "left":
				if _, err := d.ChangeTeamSession(team.ID, "leave", TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, "leave"); err != nil {
					t.Fatal(err)
				}
			case "unenrolled":
				content := team.TeamContent
				content.Enrollment = nil
				if _, err := r.ConfigureTeam("alpha", team.ID, team.Revision, content, admin, "unenroll"); err != nil {
					t.Fatal(err)
				}
			case "unknown-session":
				cmd.SessionID = uuidv7.New()
			case "unknown-team":
				team.ID = uuidv7.New()
			case "disabled":
				if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "not-stopped":
				cmd.Reconciliation.OldCredentialStopped, wantInvalid = false, true
			case "no-refs":
				cmd.Reconciliation.EvidenceRefs, wantInvalid = nil, true
			case "bad-ref":
				cmd.Reconciliation.EvidenceRefs[0], wantInvalid = TaskRef{Kind: "ci", Ref: "invalid"}, true
			case "no-reason":
				cmd.Reconciliation.Reason, wantInvalid = " ", true
			case "no-token":
				cmd.TokenID, wantInvalid = "", true
			case "authorize-refused":
				authorize = func(TeamSession) error { return ErrTeamSessionDenied }
			case "no-authorize":
				authorize = nil
			}
			before, err := readTeamSession(d.sql, sessionTeam, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			counts := assignmentCounts(t, d)
			_, err = d.RebindTeamSessionToken(team.ID, cmd, actor, "rebind", authorize)
			if err == nil || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("invalid rebind accepted", err)
			}
			if wantInvalid != errors.Is(err, ErrTaskInvalid) {
				t.Fatal(scenario, err)
			}
			if after, err := readTeamSession(d.sql, sessionTeam, c.Worker.SessionID); err != nil || !reflect.DeepEqual(after, before) {
				t.Fatal("session changed", after, err)
			}
		})
	}
}

func TestTokenRebindRollback(t *testing.T) {
	triggers := map[string]string{
		"team_sessions":    `BEFORE UPDATE ON team_sessions`,
		"team_events":      `BEFORE INSERT ON team_events`,
		"team_assignments": `BEFORE UPDATE ON team_assignments`,
		"team_messages":    `BEFORE INSERT ON team_messages`,
		"task_requests":    `BEFORE INSERT ON task_requests`,
	}
	for _, boundary := range []string{"team_sessions", "team_events", "team_assignments", "team_messages", "task_requests"} {
		t.Run(boundary, func(t *testing.T) {
			_, d, team, _, admin, c, run := runningResultFixture(t)
			token := uuidv7.New()
			before, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			counts := assignmentCounts(t, d)
			if _, err := d.sql.Exec(`CREATE TRIGGER fail_rebind_token ` + triggers[boundary] + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "rebind", allowAssignmentWorker); err == nil {
				t.Fatal("injection did not fail")
			}
			if after, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID); err != nil || !reflect.DeepEqual(after, before) || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("partial rebind", after, err)
			}
			if got, err := readTeamAssignment(d.sql, team.ID, run.ID); err != nil || !reflect.DeepEqual(got, run) {
				t.Fatal("partial attempt", got, err)
			}
			if _, err := d.sql.Exec(`DROP TRIGGER fail_rebind_token`); err != nil {
				t.Fatal(err)
			}
			if s, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "rebind", allowAssignmentWorker); err != nil || s.Generation != 2 {
				t.Fatal("rollback consumed key", s, err)
			}
		})
	}
}

func TestTokenRebindRaces(t *testing.T) {
	for _, scenario := range []string{"resume", "leave", "submit"} {
		t.Run(scenario, func(t *testing.T) {
			r, d, team, a, admin, c, run := runningResultFixture(t)
			peer, err := NewRegistry(r.root)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			d2, err := peer.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			token := uuidv7.New()
			rebind := func() error {
				_, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "rebind", allowAssignmentWorker)
				return err
			}
			var other func() error
			switch scenario {
			case "resume", "leave":
				other = func() error {
					_, err := d2.ChangeTeamSession(team.ID, scenario, TeamSessionCommand{SessionID: c.Worker.SessionID, Generation: 1}, a, scenario)
					return err
				}
			case "submit":
				other = func() error {
					_, err := d2.SubmitTeamResult(team.ID, run.ID, resultSubmission(c), a, "submit")
					return err
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, 2)
			for i, call := range []func() error{rebind, other} {
				wg.Add(1)
				go func(i int, call func() error) { defer wg.Done(); <-start; errs[i] = call() }(i, call)
			}
			close(start)
			wg.Wait()
			for _, err := range errs {
				if err != nil && !errors.Is(err, ErrTeamSessionStale) && !errors.Is(err, ErrTeamSessionDenied) && !errors.Is(err, ErrTeamAssignmentConflict) {
					t.Fatal(err)
				}
			}
			session, err := readTeamSession(d.sql, team.ID, c.Worker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := readTeamAssignment(d.sql, team.ID, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "resume", "leave":
				// Both advance the generation from 1: exactly one wins.
				if (errs[0] == nil) == (errs[1] == nil) || session.Generation != 2 {
					t.Fatal("race", errs, session)
				}
				if errs[0] == nil && (session.TokenID != token || got.Worker.Generation != 2) {
					t.Fatal("rebind winner state", session, got)
				}
			case "submit":
				// Submission does not advance the generation; either order is consistent.
				if errs[0] != nil || session.TokenID != token || session.Generation != 2 || got.Worker.Generation != 2 {
					t.Fatal("submit race", errs, session, got)
				}
				if errs[1] == nil && got.State != "SUBMITTED" {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestTokenRebindReopen(t *testing.T) {
	r, d, team, a, admin, c, run := runningResultFixture(t)
	token := uuidv7.New()
	s, err := d.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "rebind", allowAssignmentWorker)
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
	fresh := rebound(a, token)
	current := TeamSessionHandle{SessionID: s.ID, Generation: s.Generation}
	if got, err := d2.ReservedTeamAssignment(team.ID, current, fresh.Actor); err != nil || got.ID != run.ID || got.Worker != current {
		t.Fatal(got, err)
	}
	if _, err := d2.ReservedTeamAssignment(team.ID, current, a.Actor); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal("old credential survived reopen", err)
	}
	if replay, err := d2.RebindTeamSessionToken(team.ID, tokenRebind(c.Worker, token), admin, "rebind", allowAssignmentWorker); err != nil || !reflect.DeepEqual(replay, s) {
		t.Fatal("receipt after reopen", replay, err)
	}
}
