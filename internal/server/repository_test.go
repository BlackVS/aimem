package server

// A project's one repository (docs/DESIGN-AIFORGE-PILOT-1.md §1): an admin
// sets and clears it, every change is recorded with the values before and
// after, an ordinary token reads it under its own live grant, and team mode
// does not serve it yet.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/projectrepo"
)

func TestProjectRepositorySetReadClear(t *testing.T) {
	g := newTeamRig(t)
	put := func(bearer, project, body string) identityResp {
		return g.call(t, g.tls, "PUT", "/v1/projects/"+project+"/repository", bearer, nil, body, true)
	}
	get := func(bearer, path string) identityResp {
		return g.call(t, g.tls, "GET", path, bearer, nil, "", true)
	}
	// Nothing set: null for an admin and for a granted reader.
	for _, bearer := range []string{g.env, g.alice} {
		if r := get(bearer, "/v1/projects/alpha/repository"); r.status != 200 || !strings.Contains(string(r.body), `"repository":null`) {
			t.Fatalf("unset: %d %s", r.status, r.body)
		}
	}
	// Only an admin sets it.
	set := `{"kind":"github","url":"https://github.com/example/example.git"}`
	if r := put(g.alice, "alpha", set); r.status != 403 {
		t.Fatalf("ordinary token set the repository: %d %s", r.status, r.body)
	}
	for _, bad := range []string{
		`{"kind":"bitbucket","url":"https://example.org/x/y.git"}`,
		`{"kind":"github","url":"ftp://example.org/x/y.git"}`,
		`{"kind":"github","url":"https://user:secret@example.org/x/y.git"}`,
		`{"kind":"github","url":"https://user@example.org/x/y.git"}`,
		`{"kind":"github","url":"https://example.org/x/y.git?token=1"}`,
		`{"kind":"github","url":"https://example.org/"}`,
		`{"kind":"github","url":"https://example.org/x/y.git","access":"admin"}`,
		`{"kind":"github","url":"https://example.org/x/y.git","default_branch":"main"}`,
		`{"clear":true,"kind":"github"}`,
	} {
		if r := put(g.env, "alpha", bad); r.status != 400 {
			t.Fatalf("bad repository accepted: %d %s (%s)", r.status, r.body, bad)
		}
	}
	if r := put(g.env, "nope", set); r.status != 404 {
		t.Fatalf("unknown project: %d %s", r.status, r.body)
	}
	if _, err := g.s.reg.OpenExisting("nope"); err == nil {
		t.Fatal("setting a repository created a project")
	}
	r := put(g.env, "alpha", set)
	var out struct {
		Action     string          `json:"action"`
		Repository *repositoryView `json:"repository"`
		Previous   *repositoryView `json:"previous"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil || out.Action != projectrepo.ActionSet || out.Previous != nil ||
		out.Repository == nil || out.Repository.Access != "write" || out.Repository.Host != "github.com" || out.Repository.SetBy == "" {
		t.Fatalf("set: %d %s", r.status, r.body)
	}
	// The granted user reads it with either of its tokens; not without a
	// grant, not with a read-only token, not for a reserved or unknown project.
	for _, bearer := range []string{g.alice, g.project} {
		var got struct {
			Repository repositoryView `json:"repository"`
		}
		r := get(bearer, "/v1/projects/alpha/repository")
		if r.status != 200 || json.Unmarshal(r.body, &got) != nil || got.Repository.URL != "https://github.com/example/example.git" || got.Repository.Kind != "github" {
			t.Fatalf("granted read: %d %s", r.status, r.body)
		}
	}
	if r := put(g.env, "beta", `{"kind":"gitea","url":"git@forge.example.org:team/beta.git","access":"read"}`); r.status != 200 || !strings.Contains(string(r.body), `"host":"forge.example.org"`) {
		t.Fatalf("set beta: %d %s", r.status, r.body)
	}
	if r := get(g.alice, "/v1/projects/beta/repository"); r.status != 403 || strings.Contains(string(r.body), "forge.example.org") {
		t.Fatalf("read without a grant: %d %s", r.status, r.body)
	}
	_, readOnly, err := g.db.IssueScoped("admin", g.aliceID, "reader", access.ScopeReadOnly, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, readOnly)
	if r := get(readOnly, "/v1/projects/alpha/repository"); r.status != 403 {
		t.Fatalf("read-only token: %d %s", r.status, r.body)
	}
	if r := get(g.alice, "/v1/projects/user/repository"); r.status != 400 {
		t.Fatalf("reserved project: %d %s", r.status, r.body)
	}
	if r := get(g.alice, "/v1/projects/nope/repository"); r.status != 403 {
		t.Fatalf("unknown project, ordinary token: %d %s", r.status, r.body)
	}
	if r := get(g.env, "/v1/projects/nope/repository"); r.status != 404 {
		t.Fatalf("unknown project, admin: %d %s", r.status, r.body)
	}
	if r := get(g.alice, "/v1/projects/alpha/repository?history=1"); r.status != 403 {
		t.Fatalf("history for an ordinary token: %d %s", r.status, r.body)
	}
	// Team mode does not serve the route yet (increment 2 amends identity.v1).
	if r := g.team(t, "GET", "/v1/projects/alpha/repository"); r.status != 403 || r.code() != "team_operation_unsupported" {
		t.Fatalf("team mode: %d %s", r.status, r.body)
	}
	// Change, then clear: the history records both with the values before and after.
	if r := put(g.env, "alpha", `{"kind":"gitlab","url":"ssh://git@gitlab.example.org:2222/team/alpha.git","access":"read"}`); r.status != 200 || !strings.Contains(string(r.body), `"previous":{"kind":"github"`) ||
		!strings.Contains(string(r.body), `"host":"gitlab.example.org:2222"`) {
		t.Fatalf("change: %d %s", r.status, r.body)
	}
	if r := put(g.env, "alpha", `{"clear":true}`); r.status != 200 || !strings.Contains(string(r.body), `"repository":null`) {
		t.Fatalf("clear: %d %s", r.status, r.body)
	}
	if r := put(g.env, "alpha", `{"clear":true}`); r.status != 404 {
		t.Fatalf("clear with nothing set: %d %s", r.status, r.body)
	}
	var hist struct {
		Repository *repositoryView      `json:"repository"`
		History    []projectrepo.Change `json:"history"`
	}
	r = get(g.env, "/v1/projects/alpha/repository?history=1")
	if r.status != 200 || json.Unmarshal(r.body, &hist) != nil || hist.Repository != nil || len(hist.History) != 3 {
		t.Fatalf("history: %d %s", r.status, r.body)
	}
	h := hist.History
	if h[0].Action != projectrepo.ActionClear || h[0].New != nil || h[0].Old == nil || h[0].Old.Kind != "gitlab" ||
		h[1].Action != projectrepo.ActionSet || h[1].Old == nil || h[1].Old.Kind != "github" || h[1].New.Kind != "gitlab" ||
		h[2].Action != projectrepo.ActionSet || h[2].Old != nil || h[2].New.Kind != "github" || h[0].By == "" {
		t.Fatalf("history entries: %s", r.body)
	}
	g.assertNoSecretLeak(t)
}

// The project's grants list users, groups and team profiles with names
// beside IDs, for the admin only.
func TestProjectGrants(t *testing.T) {
	g := newTeamRig(t)
	grp, err := g.db.CreateGroup("admin", "reviewers")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.db.SetGrant("admin", g.alphaInstance, "group", grp.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := g.db.SetTeamProfileDisabled("admin", g.profile.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := g.call(t, g.tls, "GET", "/v1/projects/alpha/grants", g.alice, nil, "", true); r.status != 403 {
		t.Fatalf("ordinary token read the grants: %d %s", r.status, r.body)
	}
	r := g.call(t, g.tls, "GET", "/v1/projects/alpha/grants", g.env, nil, "", true)
	var out struct {
		Instance string                  `json:"instance"`
		Grants   []access.ProjectGrantee `json:"grants"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil || out.Instance != g.alphaInstance || len(out.Grants) != 3 {
		t.Fatalf("grants: %d %s", r.status, r.body)
	}
	want := []access.ProjectGrantee{
		{Kind: "user", ID: g.aliceID, Name: "Alice"},
		{Kind: "group", ID: grp.ID, Name: "reviewers"},
		{Kind: "team_profile", ID: g.profile.ID, Disabled: true, ServiceID: "aicrew-example", TeamID: "team-1"},
	}
	for i := range want {
		if out.Grants[i] != want[i] {
			t.Fatalf("grant %d: %+v, want %+v", i, out.Grants[i], want[i])
		}
	}
	// beta has an access instance but no grant; a project never granted has none.
	if r := g.call(t, g.tls, "GET", "/v1/projects/beta/grants", g.env, nil, "", true); r.status != 200 || !strings.Contains(string(r.body), `"grants":[]`) {
		t.Fatalf("beta: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/projects/nope/grants", g.env, nil, "", true); r.status != 404 {
		t.Fatalf("unknown project: %d %s", r.status, r.body)
	}
}
