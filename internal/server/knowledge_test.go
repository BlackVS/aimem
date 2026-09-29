package server

// Task 19d8: the first pilot's knowledge reads (docs/DESIGN-AIFORGE-KNOWLEDGE.md).
// Recall, the doc list and one doc's current revision serve an ordinary user
// token under its own live grant, and a verified team context under its
// profile's live grant only; reserved projects and ?rev= are refused to both;
// legacy writer and admin tokens are unchanged.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
)

// knowledgeRig is the team rig with knowledge in alpha, beta, a group space
// and the personal store, written by the hub admin.
type knowledgeRig struct {
	*teamRig
}

func newKnowledgeRig(t *testing.T) *knowledgeRig {
	t.Helper()
	g := &knowledgeRig{newTeamRig(t)}
	for _, p := range []string{"alpha", "beta", "group-x", "user"} {
		for i, body := range []string{"first " + p + " runbook", "second " + p + " runbook"} {
			put, _ := json.Marshal(map[string]any{"body": body, "base_rev": i, "updated_by": "admin"})
			if r := g.call(t, g.tls, "PUT", "/v1/projects/"+p+"/docs/RUNBOOK", g.env, nil, string(put), true); r.status != 200 {
				t.Fatalf("seed %s doc: %d %s", p, r.status, r.body)
			}
		}
		mem, _ := json.Marshal(map[string]any{"text": "the " + p + " convention is recorded here", "actor": "admin"})
		if r := g.call(t, g.tls, "POST", "/v1/projects/"+p+"/memories", g.env, nil, string(mem), true); r.status != 200 {
			t.Fatalf("seed %s memory: %d %s", p, r.status, r.body)
		}
	}
	return g
}

// knowledgePaths are the pilot's three reads in project p.
func knowledgePaths(p string) []string {
	base := "/v1/projects/" + p
	return []string{base + "/memories/recall?q=convention", base + "/docs", base + "/docs/RUNBOOK"}
}

// instance is p's access instance, created if needed.
func (g *knowledgeRig) instance(t *testing.T, p string) string {
	t.Helper()
	id, err := g.s.reg.ProjectAccessID(p)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (g *knowledgeRig) personalGet(t *testing.T, bearer, path string) identityResp {
	t.Helper()
	return g.call(t, g.tls, "GET", path, bearer, nil, "", true)
}

// An ordinary token reads the pilot set in a project it is granted, with the
// same answer a legacy credential gets; it is refused without a grant, on
// the reserved projects (which no grant can name), and for an unknown project
// (which is not created); a read-only-scope token reads nothing.
func TestKnowledgeReadsPersonal(t *testing.T) {
	g := newKnowledgeRig(t)
	for _, path := range knowledgePaths("alpha") {
		for name, bearer := range map[string]string{"user-scoped": g.alice, "project-scoped": g.project} {
			got, legacy := g.personalGet(t, bearer, path), g.personalGet(t, g.env, path)
			if got.status != 200 || string(got.body) != string(legacy.body) {
				t.Errorf("%s %s: %d %s (legacy %d %s)", name, path, got.status, got.body, legacy.status, legacy.body)
			}
		}
	}
	for _, path := range knowledgePaths("beta") {
		if r := g.personalGet(t, g.alice, path); r.status != 403 || strings.Contains(string(r.body), "beta runbook") {
			t.Errorf("no grant %s: %d %s", path, r.status, r.body)
		}
	}
	// The reserved projects are refused as a reserved scope (access
	// administration has no access instance for them to grant).
	for _, p := range []string{"user", "group-x"} {
		for _, path := range knowledgePaths(p) {
			if r := g.personalGet(t, g.alice, path); r.status != 400 || strings.Contains(string(r.body), "runbook") {
				t.Errorf("reserved %s: %d %s", path, r.status, r.body)
			}
		}
	}
	for _, path := range knowledgePaths("nope") {
		if r := g.personalGet(t, g.alice, path); r.status != 403 {
			t.Errorf("unknown project %s: %d %s", path, r.status, r.body)
		}
	}
	if _, err := g.s.reg.OpenExisting("nope"); err == nil {
		t.Fatal("a knowledge read created a project")
	}
	_, readOnly, err := g.db.IssueScoped("admin", g.aliceID, "reader", access.ScopeReadOnly, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, readOnly)
	for _, path := range knowledgePaths("alpha") {
		if r := g.personalGet(t, readOnly, path); r.status != 403 {
			t.Errorf("read-only token %s: %d %s", path, r.status, r.body)
		}
	}
	// Outside the pilot set the ordinary token is refused at the gate.
	for _, path := range []string{"/v1/projects/alpha/docs/RUNBOOK/log", "/v1/projects/alpha/memories", "/v1/projects/alpha/search?q=x"} {
		if r := g.personalGet(t, g.alice, path); r.status != 403 {
			t.Errorf("outside the pilot set %s: %d", path, r.status)
		}
	}
	g.assertNoSecretLeak(t)
}

// A scoped caller reads the current revision only: ?rev= is refused over
// direct HTTP in personal and team mode, whatever its value; a legacy
// credential still reads a retained revision (seq232).
func TestKnowledgeScopedCallersReadTheCurrentRevisionOnly(t *testing.T) {
	g := newKnowledgeRig(t)
	for _, q := range []string{"?rev=1", "?rev=2", "?rev=0", "?rev=", "?rev=x"} {
		path := "/v1/projects/alpha/docs/RUNBOOK" + q
		if r := g.personalGet(t, g.alice, path); r.status != 400 || strings.Contains(string(r.body), "runbook") {
			t.Errorf("personal %s: %d %s", q, r.status, r.body)
		}
		r := g.team(t, "GET", path)
		if r.status != 400 || r.code() != "invalid_request" || strings.Contains(string(r.body), "runbook") {
			t.Errorf("team %s: %d %s", q, r.status, r.body)
		}
	}
	var doc store.Doc
	if r := g.personalGet(t, g.env, "/v1/projects/alpha/docs/RUNBOOK?rev=1"); r.status != 200 || json.Unmarshal(r.body, &doc) != nil || doc.Body != "first alpha runbook" {
		t.Fatalf("legacy retained revision: %d %s", r.status, r.body)
	}
	doc = store.Doc{}
	if r := g.personalGet(t, g.alice, "/v1/projects/alpha/docs/RUNBOOK"); r.status != 200 || json.Unmarshal(r.body, &doc) != nil || doc.Body != "second alpha runbook" {
		t.Fatalf("the current revision: %d %s", r.status, r.body)
	}
}

// A team context reads the pilot set under its profile's grant only: the
// same answer as a personal read where the profile is granted; grant_denied
// where only the user's personal grant exists, on the reserved projects, and
// for an unknown project. Each request is
// verified online once, and each refusal audited.
func TestKnowledgeReadsTeam(t *testing.T) {
	g := newKnowledgeRig(t)
	for _, path := range knowledgePaths("alpha") {
		before := g.fake.Calls()
		team, personal := g.team(t, "GET", path), g.personalGet(t, g.alice, path)
		if team.status != 200 || string(team.body) != string(personal.body) {
			t.Errorf("team %s: %d %s (personal %d %s)", path, team.status, team.body, personal.status, personal.body)
		}
		if n := g.fake.Calls() - before; n != 1 {
			t.Errorf("%s: %d introspection calls", path, n)
		}
	}
	// A personal grant never stands in for the profile's.
	if err := g.db.SetGrant("admin", g.instance(t, "beta"), "user", g.aliceID, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range knowledgePaths("beta") {
		if r := g.personalGet(t, g.alice, path); r.status != 200 {
			t.Fatalf("personal beta %s: %d", path, r.status)
		}
		if r := g.team(t, "GET", path); r.status != 403 || r.code() != "grant_denied" || strings.Contains(string(r.body), "runbook") {
			t.Errorf("team beta %s: %d %s", path, r.status, r.body)
		}
	}
	// No profile can be granted the reserved projects.
	for _, p := range []string{"user", "group-x"} {
		for _, path := range knowledgePaths(p) {
			if r := g.team(t, "GET", path); r.status != 403 || r.code() != "grant_denied" || strings.Contains(string(r.body), "runbook") {
				t.Errorf("team reserved %s: %d %s", path, r.status, r.body)
			}
		}
	}
	for _, path := range knowledgePaths("nope") {
		if r := g.team(t, "GET", path); r.status != 403 || r.code() != "grant_denied" {
			t.Errorf("team unknown %s: %d %s", path, r.status, r.body)
		}
	}
	// Outside the pilot set a team request is refused before aicrew is asked.
	before := g.fake.Calls()
	for _, path := range []string{"/v1/projects/alpha/docs/RUNBOOK/log", "/v1/projects/alpha/memories", "/v1/projects/alpha/search?q=x"} {
		if r := g.team(t, "GET", path); r.status != 403 || r.code() != "team_operation_unsupported" {
			t.Errorf("team outside the pilot set %s: %d %s", path, r.status, r.body)
		}
	}
	if g.fake.Calls() != before {
		t.Fatal("a request outside the team routes asked aicrew")
	}
	denied := 0
	for _, ev := range g.audit(t) {
		if ev.Action == "team.refused.grant_denied" && strings.Contains(ev.Subject, "/docs") {
			denied++
		}
	}
	if denied == 0 {
		t.Fatal("knowledge refusals are not audited")
	}
	g.assertNoSecretLeak(t)
}

// Revocation takes effect on the next read: a removed personal grant, a
// revoked token, a removed profile grant and a disabled profile.
func TestKnowledgeRevocationOnTheNextRead(t *testing.T) {
	g := newKnowledgeRig(t)
	doc := "/v1/projects/alpha/docs/RUNBOOK"
	alpha := g.instance(t, "alpha")
	if r := g.team(t, "GET", doc); r.status != 200 {
		t.Fatalf("team before: %d %s", r.status, r.body)
	}
	if err := g.db.SetTeamGrant("admin", alpha, g.profile.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := g.team(t, "GET", doc); r.status != 403 || r.code() != "grant_denied" {
		t.Fatalf("team after the profile grant is removed: %d %s", r.status, r.body)
	}
	if err := g.db.SetTeamGrant("admin", alpha, g.profile.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := g.db.SetTeamProfileDisabled("admin", g.profile.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := g.team(t, "GET", doc); r.status != 403 || r.code() != "context_stale" {
		t.Fatalf("team after the profile is disabled: %d %s", r.status, r.body)
	}
	if r := g.personalGet(t, g.alice, doc); r.status != 200 {
		t.Fatalf("personal before: %d %s", r.status, r.body)
	}
	if err := g.db.SetGrant("admin", alpha, "user", g.aliceID, false); err != nil {
		t.Fatal(err)
	}
	if r := g.personalGet(t, g.alice, doc); r.status != 403 {
		t.Fatalf("personal after the grant is removed: %d %s", r.status, r.body)
	}
	if err := g.db.SetGrant("admin", alpha, "user", g.aliceID, true); err != nil {
		t.Fatal(err)
	}
	if err := g.db.Revoke("admin", g.aliceTok); err != nil {
		t.Fatal(err)
	}
	if r := g.personalGet(t, g.alice, doc); r.status != http.StatusUnauthorized {
		t.Fatalf("personal after the token is revoked: %d %s", r.status, r.body)
	}
}

// Legacy writer and admin credentials keep today's behavior on the same
// routes: every project, the reserved ones included, and retained revisions.
func TestKnowledgeLegacyUnchanged(t *testing.T) {
	g := newKnowledgeRig(t)
	writer, wd, _ := NewTokenSecret()
	if err := SaveTokens(g.s.reg.Root(), []TokenEntry{{Name: "old-writer", Role: "writer", SHA256: wd}}); err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, writer)
	for _, bearer := range []string{writer, g.env} {
		for _, p := range []string{"alpha", "beta", "user", "group-x"} {
			for _, path := range append(knowledgePaths(p), "/v1/projects/"+p+"/docs/RUNBOOK?rev=1") {
				if r := g.personalGet(t, bearer, path); r.status != 200 {
					t.Errorf("legacy %s: %d %s", path, r.status, r.body)
				}
			}
		}
	}
}
