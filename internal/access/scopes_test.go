package access

import (
	"errors"
	"testing"
	"time"
)

func TestUserTokenFollowsLiveGrants(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser("admin", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := s.IssueScoped("admin", u.ID, "token-agent", ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	check := func(project string, want bool) {
		t.Helper()
		_, err := s.Authorize(secret, project)
		if (err == nil) != want || (err != nil && !errors.Is(err, ErrDenied)) {
			t.Fatalf("authorize %s = %v, want %v", project, err, want)
		}
		got, err := s.CanWriteToken(u.ID, tok.ID, project)
		if err != nil || got != want {
			t.Fatalf("write %s = %v, %v", project, got, err)
		}
	}
	check("A", false)
	if err := s.SetGrant("admin", "A", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	check("A", true)
	g, err := s.CreateGroup("admin", "Team")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember("admin", g.ID, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant("admin", "B", "group", g.ID, true); err != nil {
		t.Fatal(err)
	}
	check("B", true)
	check("A", true)
	if err := s.SetMember("admin", g.ID, u.ID, false); err != nil {
		t.Fatal(err)
	}
	check("B", false)
	if err := s.SetGrant("admin", "A", "user", u.ID, false); err != nil {
		t.Fatal(err)
	}
	check("A", false)
	if err := s.SetGrant("admin", "A", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUser("admin", u.ID, "Agent", true); err != nil {
		t.Fatal(err)
	}
	check("A", false)
	if err := s.SetUser("admin", u.ID, "Renamed", false); err != nil {
		t.Fatal(err)
	}
	check("A", true)
	if _, err := s.db.Exec("UPDATE tokens SET expires_at=? WHERE id=?", time.Now().Unix()-1, tok.ID); err != nil {
		t.Fatal(err)
	}
	check("A", false)
	if _, err := s.db.Exec("UPDATE tokens SET expires_at=? WHERE id=?", time.Now().Add(time.Hour).Unix(), tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("admin", tok.ID); err != nil {
		t.Fatal(err)
	}
	check("A", false)
}

func TestScopeValidationAndRestrictions(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser("admin", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A", "B"} {
		if err := s.SetGrant("admin", p, "user", u.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		scope   TokenScope
		project string
	}{
		{ScopeUser, "A"}, {ScopeReadOnly, "A"}, {ScopeProject, ""}, {"admin", ""}, {"typo", "A"},
	} {
		if _, _, err := s.IssueScoped("admin", u.ID, "bad", tc.scope, tc.project, time.Now().Add(time.Hour)); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	for _, tc := range []struct {
		scope   TokenScope
		project string
	}{{ScopeProject, "A"}, {ScopeReadOnly, ""}} {
		_, secret, err := s.IssueScoped("admin", u.ID, "restricted", tc.scope, tc.project, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Authorize(secret, "B"); !errors.Is(err, ErrDenied) {
			t.Fatalf("restricted token wrote B: %v", err)
		}
	}
}

func TestScopeMigrationPreservesLegacyTokens(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser("admin", "Legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A", "B"} {
		if err := s.SetGrant("admin", p, "user", u.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	pt, projectSecret, err := s.Issue("admin", u.ID, "project", "A", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rt, readSecret, err := s.Issue("admin", u.ID, "read", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct schema 1 with real issued secrets, digests and metadata.
	if _, err := s.db.Exec(`DROP TABLE team_profile_grants;
DROP TABLE team_access_profiles;
DROP TABLE hub_identity;
ALTER TABLE tokens DROP COLUMN scope;
PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // migration then ordinary reopen
		s, err = Open(root)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
			t.Fatalf("version %d: %v", version, err)
		}
		for _, tc := range []struct {
			secret, id string
			scope      TokenScope
		}{{projectSecret, pt.ID, ScopeProject}, {readSecret, rt.ID, ScopeReadOnly}} {
			id, err := s.Authenticate(tc.secret)
			if err != nil || id.TokenID != tc.id || id.Scope != tc.scope {
				t.Fatalf("migrated identity %+v: %v", id, err)
			}
			if _, err := s.Authorize(tc.secret, "B"); !errors.Is(err, ErrDenied) {
				t.Fatalf("migration broadened token: %v", err)
			}
		}
		if _, err := s.Authorize(projectSecret, "A"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Authorize(readSecret, "A"); !errors.Is(err, ErrDenied) {
			t.Fatalf("read-only gained writes: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
