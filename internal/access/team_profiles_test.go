package access

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"aimem/internal/store"
)

func TestSchema3MigrationPreservesStandaloneAccess(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser("admin", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant("admin", "project-instance", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	tok, secret, err := s.IssueScoped("admin", u.ID, "agent", ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct a populated schema 2 database; its users, grants and token
	// must survive the additive migration.
	if _, err := s.db.Exec(dropIdentitySchema4 + `DROP TABLE team_profile_grants;
DROP TABLE team_access_profiles;
DROP TABLE hub_identity;
PRAGMA user_version=2;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var firstHubID string
	for range 2 {
		s, err = Open(root)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
			t.Fatalf("schema version %d: %v", version, err)
		}
		hubID, err := s.HubID()
		if err != nil || hubID == "" {
			t.Fatalf("hub ID %q: %v", hubID, err)
		}
		if firstHubID != "" && hubID != firstHubID {
			t.Fatalf("hub ID changed on reopen: %q to %q", firstHubID, hubID)
		}
		firstHubID = hubID
		id, err := s.Authenticate(secret)
		if err != nil || id.UserID != u.ID || id.TokenID != tok.ID {
			t.Fatalf("migrated actor %+v: %v", id, err)
		}
		if ok, err := s.CanWriteToken(u.ID, tok.ID, "project-instance"); err != nil || !ok {
			t.Fatalf("standalone grant lost: %v, %v", ok, err)
		}
		if ok, err := s.CanWriteToken(u.ID, tok.ID, "another-instance"); err != nil || ok {
			t.Fatalf("standalone grant widened: %v, %v", ok, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTeamProfileGrantIsSeparateAndLive(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser("admin", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.IssueScoped("admin", u.ID, "agent", ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant("admin", "personal", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateTeamProfile("admin", "service-1", "team-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeamProfile("admin", "service-1", "team-1"); err == nil {
		t.Fatal("duplicate peer/team link accepted")
	}
	if err := s.SetTeamGrant("admin", "team-project", p.ID, true); err != nil {
		t.Fatal(err)
	}
	check := func(profile, project string, want bool) {
		t.Helper()
		got, err := s.TeamGrantAllows(u.ID, tok.ID, profile, project)
		if err != nil || got != want {
			t.Fatalf("team grant %q/%q = %v, %v; want %v", profile, project, got, err, want)
		}
	}
	check(p.ID, "team-project", true)
	check(p.ID, "personal", false)
	check(p.ID, "other", false)
	check("another-hub-profile", "team-project", false)
	if got, err := s.TeamGrantAllows("another-user", tok.ID, p.ID, "team-project"); err != nil || got {
		t.Fatalf("wrong actor gained profile grant: %v, %v", got, err)
	}
	if got, err := s.TeamGrantAllows(u.ID, "another-token", p.ID, "team-project"); err != nil || got {
		t.Fatalf("wrong token gained profile grant: %v, %v", got, err)
	}
	if err := s.SetGrant("admin", "team-project", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	projectToken, _, err := s.IssueScoped("admin", u.ID, "project", ScopeProject, "team-project", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.TeamGrantAllows(u.ID, projectToken.ID, p.ID, "team-project"); err != nil || got {
		t.Fatalf("project-scoped token gained team grant: %v, %v", got, err)
	}
	if err := s.SetGrant("admin", "team-project", "user", u.ID, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CanWriteToken(u.ID, tok.ID, "team-project"); err != nil || ok {
		t.Fatalf("profile leaked into personal grants: %v, %v", ok, err)
	}
	if ok, err := s.CanWriteToken(u.ID, tok.ID, "personal"); err != nil || !ok {
		t.Fatalf("personal grant changed: %v, %v", ok, err)
	}
	if got, err := s.TeamGrantProjects(p.ID); err != nil || len(got) != 1 || got[0] != "team-project" {
		t.Fatalf("granted projects: %v, %v", got, err)
	}
	if err := s.SetTeamProfileDisabled("admin", p.ID, true); err != nil {
		t.Fatal(err)
	}
	check(p.ID, "team-project", false)
	if got, err := s.TeamGrantProjects(p.ID); err != nil || len(got) != 0 {
		t.Fatalf("a disabled profile still lists grants: %v, %v", got, err)
	}
	if err := s.SetTeamProfileDisabled("admin", p.ID, false); err != nil {
		t.Fatal(err)
	}
	check(p.ID, "team-project", true)
	if err := s.SetTeamGrant("admin", "team-project", p.ID, false); err != nil {
		t.Fatal(err)
	}
	check(p.ID, "team-project", false)
	if err := s.SetTeamGrant("admin", "team-project", p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUser("admin", u.ID, "Agent", true); err != nil {
		t.Fatal(err)
	}
	check(p.ID, "team-project", false)
	if err := s.SetUser("admin", u.ID, "Agent", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tokens SET expires_at=? WHERE id=?", time.Now().Unix()-1, tok.ID); err != nil {
		t.Fatal(err)
	}
	check(p.ID, "team-project", false)
	if _, err := s.db.Exec("UPDATE tokens SET expires_at=? WHERE id=?", time.Now().Add(time.Hour).Unix(), tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("admin", tok.ID); err != nil {
		t.Fatal(err)
	}
	check(p.ID, "team-project", false)
}

func TestTeamProfileGrantFollowsProjectInstanceRename(t *testing.T) {
	root := t.TempDir()
	r, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Open("original"); err != nil {
		t.Fatal(err)
	}
	projectID, err := r.ProjectAccessID("original")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	hubID, err := s.HubID()
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser("admin", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.IssueScoped("admin", u.ID, "agent", ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateTeamProfile("admin", "service-1", "team-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamGrant("admin", projectID, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Rename("original", "renamed"); err != nil {
		t.Fatal(err)
	}
	newID, err := r.ExistingProjectAccessID("renamed")
	if err != nil || newID != projectID {
		t.Fatalf("project instance changed on rename: %q, %v", newID, err)
	}
	if got, err := s.HubID(); err != nil || got != hubID {
		t.Fatalf("hub ID changed: %q, %v", got, err)
	}
	if got, err := s.TeamProfileByKey("service-1", "team-1"); err != nil || got.ID != p.ID {
		t.Fatalf("profile link changed: %+v, %v", got, err)
	}
	if ok, err := s.TeamGrantAllows(u.ID, tok.ID, p.ID, newID); err != nil || !ok {
		t.Fatalf("renamed project lost grant: %v, %v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.HubID(); err != nil || got != hubID {
		t.Fatalf("hub ID changed on reopen: %q, %v", got, err)
	}
	if got, err := s.TeamProfileByKey("service-1", "team-1"); err != nil || got.ID != p.ID {
		t.Fatalf("profile link changed on reopen: %+v, %v", got, err)
	}
	if ok, err := s.TeamGrantAllows(u.ID, tok.ID, p.ID, newID); err != nil || !ok {
		t.Fatalf("reopen lost profile grant: %v, %v", ok, err)
	}
	if _, err := s.TeamProfileByKey("service-2", "team-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong service resolved profile: %v", err)
	}
}
