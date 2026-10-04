package server

// aicrewd's team.register and team.read (docs/DESIGN-AIFORGE-PILOT-1.md §3),
// served over the real TLS listener with real peer credentials.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	teamA      = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	teamB      = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5c"
	teamTheirs = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5d"
)

type teamRegRig struct {
	*identityRig
	register, read, redeem string // peer bearers, one per operation
	registerID, readID     string // their credential IDs
}

func newTeamRegRig(t *testing.T) *teamRegRig {
	t.Helper()
	g := &teamRegRig{identityRig: newIdentityRig(t)}
	// A hub has one active peer. other-peer registers first, gets a team
	// profile and is disabled, so its team is on the hub but not ours.
	g.registerPeer(t, "other-peer")
	if r := g.admin(t, "POST", "/v1/identity/peers/other-peer/teams", `{"team_id":"`+teamTheirs+`"}`); r.status != 201 {
		t.Fatalf("other peer's team: %d %s", r.status, r.body)
	}
	if r := g.admin(t, "PUT", "/v1/identity/peers/other-peer", `{"disabled":true}`); r.status != 200 {
		t.Fatalf("disable other peer: %d %s", r.status, r.body)
	}
	g.registerPeer(t, "aicrew-example")
	exp := time.Now().Add(24 * time.Hour)
	g.registerID, g.register = g.issueCredentialFor(t, "aicrew-example", "team.register", exp)
	g.readID, g.read = g.issueCredentialFor(t, "aicrew-example", "team.read", exp)
	_, g.redeem = g.issueCredentialFor(t, "aicrew-example", "", exp)
	return g
}

func (g *teamRegRig) reg(t *testing.T, bearer, service, team, name string) identityResp {
	t.Helper()
	return g.call(t, g.tls, "PUT", "/v1/identity/peers/"+service+"/team-registrations/"+team, bearer, v1, `{"team_name":`+jsonString(name)+`}`, true)
}

func (g *teamRegRig) readAll(t *testing.T, bearer string) identityResp {
	t.Helper()
	return g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/team-reads", bearer, v1, "", true)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestTeamRegister(t *testing.T) {
	g := newTeamRegRig(t)
	var out teamRegistrationView
	r := g.reg(t, g.register, "aicrew-example", teamA, "pilot")
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil || !out.Created || out.TeamID != teamA || out.TeamName != "pilot" || out.ProfileID == "" {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	profile := out.ProfileID
	g.auditHas(t, "team.register.created", "peer:aicrew-example", teamA, `new_name="pilot"`)
	// Renaming keeps the profile; the same name again changes nothing.
	if r := g.reg(t, g.register, "aicrew-example", teamA, "pilot-2"); r.status != 200 || json.Unmarshal(r.body, &out) != nil ||
		out.Created || out.ProfileID != profile || out.OldName != "pilot" || out.TeamName != "pilot-2" {
		t.Fatalf("rename: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.renamed", "peer:aicrew-example", `old_name="pilot"`, `new_name="pilot-2"`)
	if r := g.reg(t, g.register, "aicrew-example", teamA, "pilot-2"); r.status != 200 {
		t.Fatalf("same name: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.unchanged", "peer:aicrew-example", teamA)
	// A name another team of the peer holds is refused, and audited.
	if r := g.reg(t, g.register, "aicrew-example", teamB, "pilot-2"); r.status != 409 || r.code() != "team_name_taken" {
		t.Fatalf("taken: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.refused.team_name_taken", "peer:aicrew-example", teamB)
	// Names are unique per peer: another peer may hold the same name.
	db, err := g.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterTeam("peer:other-peer", "other-peer", teamTheirs, "pilot-2"); err != nil {
		t.Fatalf("same name under another peer: %v", err)
	}
	// A team UUID registered under another peer is refused, and audited.
	if r := g.reg(t, g.register, "aicrew-example", teamTheirs, "beta"); r.status != 403 || r.code() != "peer_forbidden" {
		t.Fatalf("another peer's team: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.refused.peer_forbidden", "peer:aicrew-example", teamTheirs)
	// Malformed input.
	for _, tc := range []struct{ team, name string }{
		{"pilot", "pilot"}, {strings.ToUpper(teamA), "x"}, {teamA, ""}, {teamA, " padded"}, {teamA, "line\nbreak"},
	} {
		if r := g.reg(t, g.register, "aicrew-example", tc.team, tc.name); r.status != 400 || r.code() != "invalid_request" {
			t.Fatalf("%q %q: %d %s", tc.team, tc.name, r.status, r.body)
		}
	}
	// A disabled profile is refused and stays disabled; the grant is untouched.
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamA+"/grants/alpha", ""); r.status != 200 {
		t.Fatalf("grant: %d %s", r.status, r.body)
	}
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamA, `{"disabled":true}`); r.status != 200 {
		t.Fatalf("disable: %d %s", r.status, r.body)
	}
	if r := g.reg(t, g.register, "aicrew-example", teamA, "pilot-3"); r.status != 403 || r.code() != "profile_disabled" {
		t.Fatalf("disabled: %d %s", r.status, r.body)
	}
	for _, p := range g.teams(t) {
		if p.TeamID == teamA && (!p.Disabled || len(p.Grants) != 1) {
			t.Fatalf("registration changed the disabled profile: %+v", p)
		}
	}
	// Only a team.register credential of the path's own peer registers.
	for name, bearer := range map[string]string{"team.read": g.read, "identity.redeem": g.redeem} {
		if r := g.reg(t, bearer, "aicrew-example", teamB, "x"); r.status != 403 || r.code() != "peer_forbidden" {
			t.Fatalf("%s credential: %d %s", name, r.status, r.body)
		}
	}
	if r := g.reg(t, g.register, "other-peer", teamB, "x"); r.status != 403 || r.code() != "peer_forbidden" {
		t.Fatalf("path names another peer: %d %s", r.status, r.body)
	}
	for _, bearer := range []string{g.alice, g.env} {
		if r := g.reg(t, bearer, "aicrew-example", teamB, "x"); r.status != 401 || r.code() != "peer_unauthenticated" {
			t.Fatalf("non-peer bearer: %d %s", r.status, r.body)
		}
	}
	if r := g.call(t, g.plain, "PUT", "/v1/identity/peers/aicrew-example/team-registrations/"+teamB, g.register, v1, `{"team_name":"x"}`, true); r.status != 403 || r.code() != "tls_required" {
		t.Fatalf("plain HTTP: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "PUT", "/v1/identity/peers/aicrew-example/team-registrations/"+teamB, g.register, nil, `{"team_name":"x"}`, true); r.status != 400 || r.code() != "unsupported_version" {
		t.Fatalf("no version: %d %s", r.status, r.body)
	}
	// A team.register credential reaches no other route.
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/team-reads", g.register, v1, "", true); r.status != 403 || r.code() != "peer_forbidden" {
		t.Fatalf("register credential on team.read: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/projects/alpha/repository", g.register, nil, "", true); r.status != 403 {
		t.Fatalf("register credential on a project route: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

func TestTeamRead(t *testing.T) {
	g := newTeamRegRig(t)
	if r := g.reg(t, g.register, "aicrew-example", teamA, "pilot"); r.status != 200 {
		t.Fatalf("register: %d %s", r.status, r.body)
	}
	if r := g.reg(t, g.register, "aicrew-example", teamB, "reviewers"); r.status != 200 {
		t.Fatalf("register: %d %s", r.status, r.body)
	}
	for _, p := range []string{"beta", "gamma"} {
		if _, err := g.s.reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"alpha", "beta"} {
		if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamA+"/grants/"+p, ""); r.status != 200 {
			t.Fatalf("grant %s: %d %s", p, r.status, r.body)
		}
	}
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamB+"/grants/gamma", ""); r.status != 200 {
		t.Fatalf("grant gamma: %d %s", r.status, r.body)
	}
	if r := g.admin(t, "PUT", "/v1/projects/alpha/repository", `{"kind":"gitea","url":"https://forge.example.org:3000/team/alpha.git","access":"read"}`); r.status != 200 {
		t.Fatalf("repository: %d %s", r.status, r.body)
	}
	commit := strings.Repeat("a", 40)
	if r := g.admin(t, "PUT", "/v1/projects/alpha/process", `{"repo":"https://example.com/p.git","commit":"`+commit+`","manifest":"m.json","expected_commit":""}`); r.status != 200 {
		t.Fatalf("process: %d %s", r.status, r.body)
	}
	// Another peer's team must never appear.
	db, err := g.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterTeam("peer:other-peer", "other-peer", teamTheirs, "theirs"); err != nil {
		t.Fatal(err)
	}
	if err := g.s.reg.Drop("gamma"); err != nil {
		t.Fatal(err)
	}

	var all struct {
		Teams []teamReadView `json:"teams"`
	}
	r := g.readAll(t, g.read)
	if r.status != 200 || json.Unmarshal(r.body, &all) != nil || len(all.Teams) != 2 || strings.Contains(string(r.body), "theirs") {
		t.Fatalf("read all: %d %s", r.status, r.body)
	}
	a := all.Teams[0]
	if a.TeamID != teamA || a.TeamName != "pilot" || !a.Enabled || len(a.Projects) != 2 || a.Projects[0].Project != "alpha" || a.Projects[1].Project != "beta" {
		t.Fatalf("team A: %+v", a)
	}
	if rp := a.Projects[0]; rp.Repository == nil || rp.Repository.Host != "forge.example.org:3000" || rp.Repository.Access != "read" || rp.Process == nil || rp.Process.Commit != commit {
		t.Fatalf("alpha: %+v", rp)
	}
	if rp := a.Projects[1]; rp.Repository != nil || rp.Process != nil {
		t.Fatalf("beta has no repository or pin: %+v", rp)
	}
	// gamma was dropped: its grant is left out.
	if b := all.Teams[1]; b.TeamID != teamB || len(b.Projects) != 0 {
		t.Fatalf("team B: %+v", b)
	}
	// One team; a disabled profile has no grants; unknown and foreign teams are not_found.
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamA, `{"disabled":true}`); r.status != 200 {
		t.Fatalf("disable: %d %s", r.status, r.body)
	}
	var one teamReadView
	r = g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/team-reads/"+teamA, g.read, v1, "", true)
	if r.status != 200 || json.Unmarshal(r.body, &one) != nil || one.Enabled || len(one.Projects) != 0 || one.TeamName != "pilot" {
		t.Fatalf("disabled team: %d %s", r.status, r.body)
	}
	for _, team := range []string{teamTheirs, "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aff"} {
		r := g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/team-reads/"+team, g.read, v1, "", true)
		if r.status != 404 || r.code() != "not_found" || strings.Contains(string(r.body), "theirs") {
			t.Fatalf("%s: %d %s", team, r.status, r.body)
		}
	}
	g.auditHas(t, "team.read", "peer:aicrew-example", "service=aicrew-example")
	g.auditHas(t, "team.read.refused.not_found", "peer:aicrew-example", teamTheirs)
	// Only a team.read credential of the path's own peer reads.
	for name, bearer := range map[string]string{"team.register": g.register, "identity.redeem": g.redeem} {
		if r := g.readAll(t, bearer); r.status != 403 || r.code() != "peer_forbidden" {
			t.Fatalf("%s credential: %d %s", name, r.status, r.body)
		}
	}
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/other-peer/team-reads", g.read, v1, "", true); r.status != 403 || r.code() != "peer_forbidden" {
		t.Fatalf("another peer's path: %d %s", r.status, r.body)
	}
	if r := g.readAll(t, g.alice); r.status != 401 || r.code() != "peer_unauthenticated" {
		t.Fatalf("individual bearer: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.plain, "GET", "/v1/identity/peers/aicrew-example/team-reads", g.read, v1, "", true); r.status != 403 || r.code() != "tls_required" {
		t.Fatalf("plain HTTP: %d %s", r.status, r.body)
	}
	// A team.read credential reaches nothing else, the admin routes included.
	for _, path := range []string{"/v1/identity/peers/aicrew-example/teams", "/v1/projects/alpha/repository", "/v1/projects/alpha/tasks"} {
		if r := g.call(t, g.tls, "GET", path, g.read, v1, "", true); r.status != 403 {
			t.Fatalf("read credential on %s: %d %s", path, r.status, r.body)
		}
	}
	g.assertNoSecretLeak(t)
}

// The two operations share the read scope's per-credential bound.
func TestTeamReadRateBound(t *testing.T) {
	g := newTeamRegRig(t)
	now := time.Now()
	g.s.readLimit = &readScopeLimiter{now: func() time.Time { return now }}
	for i := 0; i < readScopePerMinute; i++ {
		if r := g.readAll(t, g.read); r.status != 200 {
			t.Fatalf("read %d: %d %s", i, r.status, r.body)
		}
	}
	if r := g.readAll(t, g.read); r.status != 429 || r.code() != "rate_limited" {
		t.Fatalf("over the bound: %d %s", r.status, r.body)
	}
}

// Every refusal of an authenticated peer is audited under peer:<service>,
// whichever check refused it, and an invalid rename names the profile's
// current name.
func TestTeamOperationRefusalsAreAudited(t *testing.T) {
	g := newTeamRegRig(t)
	if r := g.reg(t, g.register, "aicrew-example", teamA, "pilot"); r.status != 200 {
		t.Fatalf("register: %d %s", r.status, r.body)
	}
	const actor = "peer:aicrew-example"
	regPath := "/v1/identity/peers/aicrew-example/team-registrations/" + teamA
	// A malformed body.
	if r := g.call(t, g.tls, "PUT", regPath, g.register, v1, `{"team_name":42}`, true); r.status != 400 || r.code() != "invalid_request" {
		t.Fatalf("malformed body: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.refused.invalid_request", actor, "credential="+g.registerID, teamA)
	// An invalid rename keeps the current name and profile in the record.
	if r := g.reg(t, g.register, "aicrew-example", teamA, ""); r.status != 400 {
		t.Fatalf("empty name: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.refused.invalid_request", actor, `old_name="pilot"`, `new_name=""`, "profile=01")
	// The path names another peer.
	if r := g.reg(t, g.register, "other-peer", teamA, "x"); r.status != 403 {
		t.Fatalf("other path: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.refused.peer_forbidden", actor, "credential="+g.registerID, "other-peer")
	// Another operation's credential, refused by the bearer gate.
	if r := g.reg(t, g.read, "aicrew-example", teamA, "x"); r.status != 403 || r.code() != "peer_forbidden" {
		t.Fatalf("read credential: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.refused.peer_forbidden", actor, "credential="+g.readID)
	if r := g.readAll(t, g.register); r.status != 403 {
		t.Fatalf("register credential on read: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.read.refused.peer_forbidden", actor, "credential="+g.registerID)
	// Plain HTTP and a missing version.
	if r := g.call(t, g.plain, "PUT", regPath, g.register, v1, `{"team_name":"x"}`, true); r.code() != "tls_required" {
		t.Fatalf("plain HTTP: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.register.refused.tls_required", actor, "credential="+g.registerID)
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/team-reads", g.read, nil, "", true); r.code() != "unsupported_version" {
		t.Fatalf("no version: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.read.refused.unsupported_version", actor, "credential="+g.readID)
	// The rate bound.
	now := time.Now()
	g.s.readLimit = &readScopeLimiter{now: func() time.Time { return now }}
	for i := 0; i < readScopePerMinute; i++ {
		g.readAll(t, g.read)
	}
	if r := g.readAll(t, g.read); r.code() != "rate_limited" {
		t.Fatalf("over the bound: %d %s", r.status, r.body)
	}
	g.auditHas(t, "team.read.refused.rate_limited", actor, "credential="+g.readID)
	g.assertNoSecretLeak(t)
}
