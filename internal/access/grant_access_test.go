package access

import (
	"testing"
	"time"
)

// Schema 10 gives every grant its access. A store at schema 9, with user
// and group grants and tokens of each scope, migrates with each grant a
// write grant: every token reads and writes exactly as it did.
func TestAccessSchema10KeepsEveryGrantAWriteGrant(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := s.CreateUser("admin", "alice")
	bob, _ := s.CreateUser("admin", "bob")
	carol, _ := s.CreateUser("admin", "carol")
	grp, _ := s.CreateGroup("admin", "reviewers")
	if err := s.SetMember("admin", grp.ID, bob.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ project, kind, subject string }{
		{"inst-a", "user", alice.ID}, {"inst-b", "user", alice.ID}, {"inst-a", "group", grp.ID},
	} {
		if err := s.SetGrant("admin", g.project, g.kind, g.subject, true); err != nil {
			t.Fatal(err)
		}
	}
	exp := time.Now().Add(time.Hour)
	type tok struct {
		name, user, id string
		scope          TokenScope
		project        string
	}
	var toks []tok
	for _, c := range []tok{
		{"alice-user", alice.ID, "", ScopeUser, ""}, {"alice-a", alice.ID, "", ScopeProject, "inst-a"},
		{"alice-ro", alice.ID, "", ScopeReadOnly, ""}, {"bob-user", bob.ID, "", ScopeUser, ""},
		{"bob-ro", bob.ID, "", ScopeReadOnly, ""}, {"carol-user", carol.ID, "", ScopeUser, ""},
	} {
		tk, _, err := s.IssueScoped("admin", c.user, c.name, c.scope, c.project, exp)
		if err != nil {
			t.Fatal(err)
		}
		c.id = tk.ID
		toks = append(toks, c)
	}
	type answer struct{ read, write bool }
	answers := func(s *Store) map[string]answer {
		out := map[string]answer{}
		for _, c := range toks {
			for _, inst := range []string{"inst-a", "inst-b", "inst-c"} {
				r, err := s.CanReadToken(c.user, c.id, inst)
				if err != nil {
					t.Fatal(err)
				}
				w, err := writes(s, c.user, c.id, inst)
				if err != nil {
					t.Fatal(err)
				}
				out[c.name+"@"+inst] = answer{r, w}
			}
		}
		return out
	}
	before := answers(s)
	// Back to schema 9: grants without their access.
	if _, err := s.db.Exec("ALTER TABLE grants DROP COLUMN access; PRAGMA user_version=9;"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the migration, then an ordinary reopen
		if s, err = Open(root); err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 10 || accessSchema != 10 {
			t.Fatalf("schema version %d: %v", version, err)
		}
		snap, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Grants) != 3 {
			t.Fatalf("grants: %+v", snap.Grants)
		}
		for _, g := range snap.Grants {
			if g.Access != GrantWrite {
				t.Errorf("grant %+v did not migrate as a write grant", g)
			}
		}
		after := answers(s)
		for k, want := range before {
			if after[k] != want {
				t.Errorf("%s: %+v after the migration, %+v before", k, after[k], want)
			}
		}
		if !after["alice-a@inst-a"].write || !after["bob-user@inst-a"].write || after["carol-user@inst-a"].read {
			t.Fatalf("the reference answers are wrong: %+v", after)
		}
		s.Close()
	}
}

// A read grant reads and never writes, a write grant does both, and
// re-granting sets the access; removal leaves neither. Issuance of a
// project-scoped token accepts either kind.
func TestGrantAccessDecidesWrites(t *testing.T) {
	s := testStore(t)
	u, _ := s.CreateUser("admin", "auditor")
	if _, err := s.SetGrantAccess("admin", "inst", "user", u.ID, GrantRead); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.IssueScoped("admin", u.ID, "auditor-inst", ScopeProject, "inst", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("a project-scoped token under a read grant: %v", err)
	}
	check := func(stage string, read, write bool) {
		t.Helper()
		r, _ := s.CanReadToken(u.ID, tk.ID, "inst")
		d, _ := s.DecideWrite(u.ID, tk.ID, "inst")
		want := map[[2]bool]WriteDecision{{false, false}: WriteHidden, {true, false}: WriteReadOnly, {true, true}: WriteAllowed}[[2]bool{read, write}]
		if r != read || d != want {
			t.Fatalf("%s: read %v decision %v, want %v %v", stage, r, d, read, want)
		}
	}
	check("read grant", true, false)
	if _, err := s.SetGrantAccess("admin", "inst", "user", u.ID, GrantWrite); err != nil {
		t.Fatal(err)
	}
	check("raised to write", true, true)
	if _, err := s.SetGrantAccess("admin", "inst", "user", u.ID, GrantRead); err != nil {
		t.Fatal(err)
	}
	check("lowered to read", true, false)
	if err := s.SetGrant("admin", "inst", "user", u.ID, false); err != nil {
		t.Fatal(err)
	}
	check("removed", false, false)
	if _, err := s.SetGrantAccess("admin", "inst", "user", u.ID, "admin"); err == nil {
		t.Fatal("an unknown access was stored")
	}
	events, err := s.AuditEvents(time.Time{}, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	want := map[string]bool{"grant.read": false, "grant.true": false, "grant.false": false}
	for _, a := range actions {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for a, seen := range want {
		if !seen {
			t.Errorf("no %s audit event among %v", a, actions)
		}
	}
}

// writes is DecideWrite's yes: the token may write project.
func writes(s *Store, user, token, project string) (bool, error) {
	d, err := s.DecideWrite(user, token, project)
	return d == WriteAllowed, err
}
