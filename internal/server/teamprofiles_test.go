package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const teamsPath = "/v1/identity/peers/aicrew-example/teams"

type profileListing struct {
	ProfileID string `json:"profile_id"`
	TeamID    string `json:"team_id"`
	Disabled  bool   `json:"disabled"`
	Grants    []struct {
		Project  string `json:"project"`
		Instance string `json:"instance"`
	} `json:"grants"`
}

func (g *identityRig) admin(t *testing.T, method, path, body string) identityResp {
	t.Helper()
	return g.call(t, g.tls, method, path, g.env, nil, body, true)
}

func (g *identityRig) teams(t *testing.T) []profileListing {
	t.Helper()
	r := g.admin(t, "GET", teamsPath, "")
	var out struct {
		Teams []profileListing `json:"teams"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil {
		t.Fatalf("list teams: %d %s", r.status, r.body)
	}
	return out.Teams
}

func (g *identityRig) auditHas(t *testing.T, action, actor string, parts ...string) {
	t.Helper()
	db, err := g.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range snap.Audit {
		if ev.Action != action || ev.Actor != actor {
			continue
		}
		all := true
		for _, p := range parts {
			all = all && strings.Contains(ev.Subject, p)
		}
		if all {
			return
		}
	}
	t.Errorf("no %s audit by %s naming %v", action, actor, parts)
}

func TestTeamProfileRoutes(t *testing.T) {
	g := newIdentityRig(t)
	if r := g.admin(t, "GET", teamsPath, ""); r.status != 404 {
		t.Fatalf("teams of an unregistered peer: %d %s", r.status, r.body)
	}
	if r := g.admin(t, "POST", teamsPath, `{"team_id":"team-1"}`); r.status != 404 {
		t.Fatalf("profile for an unregistered peer: %d %s", r.status, r.body)
	}
	g.registerPeer(t, "aicrew-example")
	for _, rt := range [][2]string{{"GET", "/v1/identity/peers/aicrew-other/teams"}, {"PUT", "/v1/identity/peers/aicrew-other/teams/team-1/grants/alpha"}} {
		if r := g.admin(t, rt[0], rt[1], ""); r.status != 404 {
			t.Errorf("%s %s for a service that is not a peer: %d %s", rt[0], rt[1], r.status, r.body)
		}
	}
	// A disabled peer still accepts a prepared profile.
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example", `{"disabled":true}`); r.status != 200 {
		t.Fatal(r.status)
	}
	r := g.admin(t, "POST", teamsPath, `{"team_id":"team-1"}`)
	var created profileListing
	if r.status != 201 || json.Unmarshal(r.body, &created) != nil || created.TeamID != "team-1" || created.ProfileID == "" {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	for body, want := range map[string]int{
		`{"team_id":"team-1"}`:            409,
		`{"team_id":"team one"}`:          400,
		`{"team_id":"team/1"}`:            400,
		`{"team_id":"."}`:                 400,
		`{"team_id":".."}`:                400,
		`{}`:                              400,
		`{"team_id":"t","role":"worker"}`: 400,
	} {
		if r := g.admin(t, "POST", teamsPath, body); r.status != want {
			t.Errorf("create %s: %d, want %d (%s)", body, r.status, want, r.body)
		}
	}
	g.auditHas(t, "team_profile.create", "env", "service=aicrew-example", "team=team-1", "profile="+created.ProfileID)

	// Grants resolve the project to its access instance.
	team := teamsPath + "/team-1"
	instance, err := g.s.reg.ProjectAccessID("alpha")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if r := g.admin(t, "PUT", team+"/grants/alpha", ""); r.status != 200 || !strings.Contains(string(r.body), instance) {
			t.Fatalf("grant alpha: %d %s", r.status, r.body)
		}
	}
	g.auditHas(t, "team_grant.true", "env", "team=team-1", "instance="+instance)
	for project, want := range map[string]int{"missing": 404, "group-x": 400, "user": 400} {
		if r := g.admin(t, "PUT", team+"/grants/"+project, ""); r.status != want {
			t.Errorf("grant %s: %d, want %d (%s)", project, r.status, want, r.body)
		}
	}
	// Revoke by name never mints an instance.
	if _, err := g.s.reg.Open("beta"); err != nil {
		t.Fatal(err)
	}
	if r := g.admin(t, "DELETE", team+"/grants/beta", ""); r.status != 200 || !strings.Contains(string(r.body), `"revoked":false`) {
		t.Fatalf("revoke a never-granted project: %d %s", r.status, r.body)
	}
	if id, err := g.s.reg.ExistingProjectAccessID("beta"); err != nil || id != "" {
		t.Fatalf("revoke minted an access instance for beta: %q %v", id, err)
	}
	g.auditHas(t, "team_grant.false", "env", "team=team-1", "project=beta", "instance=none")
	// An orphaned instance is listed without a project and revoked by ID.
	orphan := "01a0e121-0000-7000-8000-000000000001"
	db, _ := g.s.openAccess(false)
	if err := db.SetTeamGrant("admin", orphan, created.ProfileID, true); err != nil {
		t.Fatal(err)
	}
	teams := g.teams(t)
	if len(teams) != 1 || len(teams[0].Grants) != 2 {
		t.Fatalf("list: %+v", teams)
	}
	for _, gr := range teams[0].Grants {
		if (gr.Instance == instance) != (gr.Project == "alpha") || (gr.Instance == orphan) != (gr.Project == "") {
			t.Fatalf("grant listing: %+v", teams[0].Grants)
		}
	}
	if r := g.admin(t, "DELETE", team+"/grant-instances/not-an-id", ""); r.status != 400 {
		t.Fatalf("revoke a malformed instance: %d", r.status)
	}
	if r := g.admin(t, "DELETE", team+"/grant-instances/"+orphan, ""); r.status != 200 {
		t.Fatalf("revoke the orphan: %d %s", r.status, r.body)
	}
	if r := g.admin(t, "DELETE", team+"/grants/alpha", ""); r.status != 200 || !strings.Contains(string(r.body), `"revoked":true`) {
		t.Fatalf("revoke alpha: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team_grant.false", "env", "team=team-1", "instance="+orphan)
	g.auditHas(t, "team_grant.false", "env", "team=team-1", "instance="+instance)
	if r := g.admin(t, "GET", team+"/grants", ""); r.status != 200 || !strings.Contains(string(r.body), `"grants":[]`) {
		t.Fatalf("grants after revoking all: %d %s", r.status, r.body)
	}

	// Disable, re-enable, unknown team.
	if r := g.admin(t, "PUT", team, `{"disabled":true}`); r.status != 200 {
		t.Fatal(r.status)
	}
	if teams := g.teams(t); !teams[0].Disabled {
		t.Fatal("the profile is not disabled")
	}
	g.auditHas(t, "team_profile.disabled.true", "env", "team=team-1")
	if r := g.admin(t, "PUT", team, `{"disabled":false}`); r.status != 200 {
		t.Fatal(r.status)
	}
	if r := g.admin(t, "PUT", team, `{}`); r.status != 400 {
		t.Fatalf("update without disabled: %d", r.status)
	}
	for _, rt := range [][2]string{{"PUT", teamsPath + "/team-9"}, {"GET", teamsPath + "/team-9/grants"}, {"PUT", teamsPath + "/team-9/grants/alpha"}} {
		if r := g.admin(t, rt[0], rt[1], `{"disabled":true}`); r.status != 404 {
			t.Errorf("%s %s: %d", rt[0], rt[1], r.status)
		}
	}
	g.assertNoSecretLeak(t)
}

// TestTeamProfileRoutesAreAdminOverHubTLS: every route refuses ordinary,
// peer and legacy writer credentials, and plain HTTP.
func TestTeamProfileRoutesAreAdminOverHubTLS(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peerSecret := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	writer, digest, err := NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveTokens(g.s.reg.Root(), []TokenEntry{{Name: "old-writer", Role: "writer", SHA256: digest}}); err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, writer)
	if r := g.admin(t, "POST", teamsPath, `{"team_id":"team-1"}`); r.status != 201 {
		t.Fatal(r.status)
	}
	routes := [][3]string{
		{"GET", teamsPath, ""}, {"POST", teamsPath, `{"team_id":"team-2"}`}, {"PUT", teamsPath + "/team-1", `{"disabled":true}`},
		{"GET", teamsPath + "/team-1/grants", ""}, {"PUT", teamsPath + "/team-1/grants/alpha", ""},
		{"DELETE", teamsPath + "/team-1/grants/alpha", ""}, {"DELETE", teamsPath + "/team-1/grant-instances/01a0e121-0000-7000-8000-000000000001", ""},
	}
	for _, rt := range routes {
		for name, bearer := range map[string]string{"ordinary": g.alice, "project": g.project, "peer": peerSecret, "writer": writer} {
			if r := g.call(t, g.tls, rt[0], rt[1], bearer, nil, rt[2], true); r.status != 403 {
				t.Errorf("%s %s with a %s credential: %d %s", rt[0], rt[1], name, r.status, r.body)
			}
		}
		if r := g.call(t, g.plain, rt[0], rt[1], g.env, nil, rt[2], true); r.code() != "tls_required" {
			t.Errorf("%s %s over plain HTTP: %d %s", rt[0], rt[1], r.status, r.body)
		}
	}
	if teams := g.teams(t); len(teams) != 1 || teams[0].Disabled || len(teams[0].Grants) != 0 {
		t.Fatalf("a refused request changed state: %+v", teams)
	}
}

// TestTeamProfileRoutesTakeEffectOnTheNextTeamRequest drives grants and the
// profile switch through the routes and reads in team mode after each change.
func TestTeamProfileRoutesTakeEffectOnTheNextTeamRequest(t *testing.T) {
	g := newTeamRig(t)
	// A team created and granted through the routes is served.
	if r := g.admin(t, "POST", teamsPath, `{"team_id":"team-2"}`); r.status != 201 {
		t.Fatal(r.status)
	}
	g.answerActive(func(m map[string]any) { m["team_id"] = "team-2" })
	read := func() string {
		t.Helper()
		r := g.team(t, "GET", "/v1/tasks/"+g.alphaTask)
		if r.status == 200 {
			return "served"
		}
		code, _, _ := refusal(t, r)
		return code
	}
	team := teamsPath + "/team-2"
	steps := []struct {
		method, path, body, want string
	}{
		{"", "", "", "grant_denied"},
		{"PUT", team + "/grants/alpha", "", "served"},
		{"DELETE", team + "/grants/alpha", "", "grant_denied"},
		{"PUT", team + "/grants/alpha", "", "served"},
		{"PUT", team, `{"disabled":true}`, "context_stale"},
		{"PUT", team, `{"disabled":false}`, "served"},
		{"DELETE", team + "/grant-instances/" + g.alphaInstance, "", "grant_denied"},
	}
	for _, st := range steps {
		if st.method != "" {
			if r := g.admin(t, st.method, st.path, st.body); r.status != 200 {
				t.Fatalf("%s %s: %d %s", st.method, st.path, r.status, r.body)
			}
		}
		if got := read(); got != st.want {
			t.Fatalf("after %s %s: team read %s, want %s", st.method, st.path, got, st.want)
		}
	}
	// team-1 (granted alpha by the rig) is untouched, and so is alice's
	// personal access.
	g.answerActive(nil)
	if got := read(); got != "served" {
		t.Fatalf("another team's grant changed: %s", got)
	}
	if r := g.call(t, g.tls, "GET", "/v1/tasks/"+g.alphaTask, g.alice, nil, "", true); r.status != 200 {
		t.Fatalf("personal read: %d", r.status)
	}
}
