package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"aimem/internal/uuidv7"
)

func sessionFixture(t *testing.T) (*Registry, *DB, Team, TeamAuditContext, TeamAuditContext) {
	t.Helper()
	r, d, admin := teamFixture(t)
	a := TeamAuditContext{Actor: TaskActor{Kind: "user", UserID: uuidv7.New(), TokenID: uuidv7.New(), Name: "agent"}, RequestID: uuidv7.New(), ServerVersion: "test"}
	team, err := r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "builders", Enrollment: []TeamEnrollment{{UserID: a.Actor.UserID, Coordinator: true}}}, admin, "setup")
	if err != nil {
		t.Fatal(err)
	}
	return r, d, team, a, admin
}

func testProfile() TeamProfile {
	return TeamProfile{Label: "worker", Platform: "test-client", PlatformVersion: "unknown"}
}

func TestTeamSessionLifecycle(t *testing.T) {
	r, d, team, a, admin := sessionFixture(t)
	s, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "join")
	if err != nil {
		t.Fatal(err)
	}
	if s.Model.ID != "unknown" || s.Model.Source != "unknown" || s.CoordinatorGeneration != 1 {
		t.Fatalf("profile: %+v", s)
	}
	second, err := d.JoinTeam(team.ID, "worker", testProfile(), a, "second")
	if err != nil || second.ID == s.ID {
		t.Fatalf("distinct session: %+v %v", second, err)
	}
	last, _ := time.Parse(time.RFC3339Nano, s.LastSeenAt)
	if s.Suspect(last.Add(119*time.Second), 120*time.Second) || !s.Suspect(last.Add(120*time.Second), 120*time.Second) {
		t.Fatal("suspect boundary")
	}
	cmd := TeamSessionCommand{SessionID: s.ID, Generation: 1}
	resumed, err := d.ChangeTeamSession(team.ID, "resume", cmd, a, "resume")
	if err != nil || resumed.Generation != 2 || resumed.CoordinatorGeneration != 2 {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	retry, err := d.ChangeTeamSession(team.ID, "resume", cmd, a, "resume")
	if err != nil || retry.Generation != 2 {
		t.Fatalf("resume receipt: %+v %v", retry, err)
	}
	if _, err = d.ChangeTeamSession(team.ID, "resume", cmd, a, "stale"); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal(err)
	}
	if _, err = d.TeamRoster(team.ID, s.ID, 1, a.Actor, "", 100); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal(err)
	}
	roster, err := d.TeamRoster(team.ID, s.ID, 2, a.Actor, "", 100)
	if err != nil || len(roster) != 2 {
		t.Fatalf("roster: %v %v", roster, err)
	}
	wrong := a
	wrong.Actor.TokenID = uuidv7.New()
	if _, err = d.TeamRoster(team.ID, s.ID, 2, wrong.Actor, "", 100); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatalf("token binding: %v", err)
	}
	p := testProfile()
	p.Model = TeamModel{ID: "declared-model", Source: "agent_reported"}
	cmd.Generation = 2
	cmd.Profile = &p
	cmd.ExpectedProfileRevision = 1
	updated, err := d.ChangeTeamSession(team.ID, "profile", cmd, a, "profile")
	if err != nil || updated.ProfileRevision != 2 || updated.Model.ID != "declared-model" {
		t.Fatalf("profile: %+v %v", updated, err)
	}
	if _, err = d.ChangeTeamSession(team.ID, "profile", cmd, a, "stale-profile"); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal(err)
	}
	cmd = TeamSessionCommand{SessionID: s.ID, Generation: 2, Availability: "unavailable"}
	if _, err = d.ChangeTeamSession(team.ID, "heartbeat", cmd, a, "heartbeat"); err != nil {
		t.Fatal(err)
	}
	cmd.Availability = ""
	left, err := d.ChangeTeamSession(team.ID, "leave", cmd, a, "leave")
	if err != nil || left.State != "left" || left.Generation != 3 {
		t.Fatalf("leave: %+v %v", left, err)
	}
	if _, err = d.ChangeTeamSession(team.ID, "leave", cmd, a, "leave"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ChangeTeamSession(team.ID, "resume", cmd, a, "after-leave"); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal(err)
	}
	newCoordinator, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "replacement")
	if err != nil || newCoordinator.CoordinatorGeneration != 3 {
		t.Fatalf("replacement: %+v %v", newCoordinator, err)
	}
	// Enrollment is checked before receipt lookup, even for an old join.
	team.Enrollment = nil
	if _, err = r.ConfigureTeam("alpha", team.ID, 1, team.TeamContent, admin, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.JoinTeam(team.ID, "coordinator", testProfile(), a, "join"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatalf("revoked replay: %v", err)
	}
	if _, err = d.TeamRoster(team.ID, second.ID, 1, a.Actor, "", 100); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
}

func TestTeamSessionConcurrentCommands(t *testing.T) {
	_, d, team, a, _ := sessionFixture(t)
	var wg sync.WaitGroup
	results := make([]TeamSession, 12)
	errs := make([]error, 12)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = d.JoinTeam(team.ID, "coordinator", testProfile(), a, fmt.Sprint(i)) })
	}
	wg.Wait()
	winners := 0
	var s TeamSession
	for i, err := range errs {
		if err == nil {
			winners++
			s = results[i]
		} else if !errors.Is(err, ErrTeamCoordinatorOccupied) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("coordinator winners %d", winners)
	}
	for i := range results {
		wg.Go(func() {
			results[i], errs[i] = d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: s.ID, Generation: 1}, a, fmt.Sprint(i))
		})
	}
	wg.Wait()
	winners = 0
	for _, err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrTeamSessionStale) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("resume winners %d", winners)
	}
	for i := range results {
		wg.Go(func() { results[i], errs[i] = d.JoinTeam(team.ID, "worker", testProfile(), a, "shared-key") })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || results[i].ID != results[0].ID {
			t.Fatalf("retry: %v %v", results, errs)
		}
	}
	changed := testProfile()
	changed.Label = "changed"
	if _, err := d.JoinTeam(team.ID, "worker", changed, a, "shared-key"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatal(err)
	}
	events, err := d.TeamEvents(team.ID, 0, 100)
	if err != nil || len(events) != 4 {
		t.Fatalf("atomic audit: %d %v", len(events), err)
	}
}

func TestTeamSessionRollbackAndQuota(t *testing.T) {
	_, d, team, a, _ := sessionFixture(t)
	if _, err := d.sql.Exec(`CREATE TRIGGER reject_session_event BEFORE INSERT ON team_events BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "join"); err == nil {
		t.Fatal("audit failure accepted")
	}
	var count int
	if err := d.sql.QueryRow(`SELECT count(*) FROM team_sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback: %d %v", count, err)
	}
	if _, err := d.sql.Exec(`DROP TRIGGER reject_session_event`); err != nil {
		t.Fatal(err)
	}
	s, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "join")
	if err != nil || s.CoordinatorGeneration != 1 {
		t.Fatalf("retry rollback: %+v %v", s, err)
	}
	for i := 1; i < MaxTeamSessions; i++ {
		if _, err = d.JoinTeam(team.ID, "worker", testProfile(), a, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = d.JoinTeam(team.ID, "worker", testProfile(), a, "over"); !errors.Is(err, ErrTeamSessionQuota) {
		t.Fatal(err)
	}
	if _, err = d.JoinTeam(team.ID, "coordinator", testProfile(), a, "join"); err != nil {
		t.Fatalf("quota receipt: %v", err)
	}
	if err = d.SetMeta(TasksMetaKey, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.JoinTeam(team.ID, "coordinator", testProfile(), a, "join"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatalf("disabled receipt: %v", err)
	}
}

func TestTeamSessionMigrationAndReopen(t *testing.T) {
	r, d, team, a, _ := sessionFixture(t)
	for _, q := range []string{`DROP TABLE team_sessions`, `DROP TABLE team_session_control`, `UPDATE meta SET value='14' WHERE key='schema_version'`} {
		if _, err := d.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
	s, err := d.JoinTeam(team.ID, "worker", testProfile(), a, "join")
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
	roster, err := d2.TeamRoster(team.ID, s.ID, 1, a.Actor, "", 100)
	if err != nil || len(roster) != 1 || roster[0].ID != s.ID {
		t.Fatalf("persisted roster: %+v %v", roster, err)
	}
	events, err := d2.TeamEvents(team.ID, 0, 100)
	if err != nil || len(events) != 2 || events[1].Session == nil || events[1].Session.ID != s.ID || events[1].Actor != a.Actor {
		t.Fatalf("persisted audit: %+v %v", events, err)
	}
}

func TestTeamSessionAuthorityAndValidation(t *testing.T) {
	r, d, team, a, admin := sessionFixture(t)
	for _, bad := range []TeamAuditContext{admin, {Actor: TaskActor{Kind: "user", UserID: uuidv7.New(), TokenID: uuidv7.New(), Name: "outsider"}}} {
		if _, err := d.JoinTeam(team.ID, "worker", testProfile(), bad, "denied"); !errors.Is(err, ErrTeamSessionDenied) {
			t.Fatalf("join authority: %v", err)
		}
	}
	team.Enrollment[0].Coordinator = false
	if _, err := r.ConfigureTeam("alpha", team.ID, 1, team.TeamContent, admin, "demote"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "escalate"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	s, err := d.JoinTeam(team.ID, "worker", testProfile(), a, "worker")
	if err != nil {
		t.Fatal(err)
	}
	wrong := a
	wrong.Actor.TokenID = uuidv7.New()
	cmd := TeamSessionCommand{SessionID: s.ID, Generation: 1}
	if _, err = d.ChangeTeamSession(team.ID, "resume", cmd, wrong, "wrong-token"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	other, err := r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "other", Enrollment: team.Enrollment}, admin, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ChangeTeamSession(other.ID, "resume", cmd, a, "wrong-team"); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	if _, err = d.TeamRoster(other.ID, s.ID, 1, a.Actor, "", 100); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	for _, op := range []string{"assign", "handoff"} {
		if _, err = d.ChangeTeamSession(team.ID, op, cmd, a, op); !errors.Is(err, ErrTaskInvalid) {
			t.Fatal(err)
		}
	}
	for _, profile := range []TeamProfile{{}, {Label: "x", Platform: "x", PlatformVersion: "x", Model: TeamModel{Source: "inferred"}}, {Label: "x", Platform: "x", PlatformVersion: "x", Model: TeamModel{ObservedAt: "yesterday"}}} {
		if _, err = d.JoinTeam(team.ID, "worker", profile, a, "bad-profile"); !errors.Is(err, ErrTaskInvalid) {
			t.Fatal(err)
		}
	}
	if _, err = d.sql.Exec(`CREATE TRIGGER reject_session_change BEFORE INSERT ON team_events BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ChangeTeamSession(team.ID, "resume", cmd, a, "rollback"); err == nil {
		t.Fatal("accepted without audit")
	}
	roster, err := d.TeamRoster(team.ID, s.ID, 1, a.Actor, "", 1)
	if err != nil || len(roster) != 1 || roster[0].Generation != 1 {
		t.Fatalf("rollback: %+v %v", roster, err)
	}
	if _, err = d.sql.Exec(`DROP TRIGGER reject_session_change`); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ChangeTeamSession(team.ID, "resume", cmd, a, "rollback"); err != nil {
		t.Fatal(err)
	}
}
