package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/introspect/introspecttest"
	"aimem/internal/store"
)

// teamRig is a hub over real TLS with a fake aicrew registered as its peer,
// a team access profile for (aicrew-example, team-1) granted on alpha only,
// and task data in alpha and beta. Alice has a personal grant on alpha too.
type teamRig struct {
	*introspectionRig
	db                *access.Store
	profile           access.TeamProfile
	alphaInstance     string
	alphaTask, betaTk string
	comment, epic     string
}

func newTeamRig(t *testing.T) *teamRig {
	t.Helper()
	g := &teamRig{introspectionRig: newIntrospectionRig(t, introspecttest.Path)}
	db, err := g.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	g.db = db
	for _, p := range []string{"alpha", "beta"} {
		pdb, err := g.s.reg.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := pdb.SetMeta(store.TasksMetaKey, "on"); err != nil {
			t.Fatal(err)
		}
	}
	if g.alphaInstance, err = g.s.reg.ProjectAccessID("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.reg.ProjectAccessID("beta"); err != nil {
		t.Fatal(err)
	}
	if g.profile, err = db.CreateTeamProfile("admin", "aicrew-example", "team-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTeamGrant("admin", g.alphaInstance, g.profile.ID, true); err != nil {
		t.Fatal(err)
	}
	g.alphaTask = g.create(t, "POST", "/v1/projects/alpha/tasks", `{"title":"alpha task"}`)
	g.betaTk = g.create(t, "POST", "/v1/projects/beta/tasks", `{"title":"beta task"}`)
	g.comment = g.create(t, "POST", "/v1/tasks/"+g.alphaTask+"/comments", `{"body":"first"}`)
	g.epic = g.create(t, "POST", "/v1/projects/alpha/epics", `{"title":"alpha epic"}`)
	g.answerActive(nil)
	return g
}

var createSeq int

// create makes one record as the hub admin and returns its ID.
func (g *teamRig) create(t *testing.T, method, path, body string) string {
	t.Helper()
	createSeq++
	r := g.call(t, g.tls, method, path, g.env, map[string]string{"Idempotency-Key": "team-rig-" + strconv.Itoa(createSeq)}, body, true)
	if r.status != 201 {
		t.Fatalf("%s %s: %d %s", method, path, r.status, r.body)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(r.body, &out)
	return out.ID
}

// answerActive makes the fake report an active worker session of alice in
// team-1; edit adjusts the reply.
func (g *teamRig) answerActive(edit func(m map[string]any)) {
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		m := introspecttest.ActiveReply(got.Nonce, "aicrew-example", g.hub)
		m["identity"] = map[string]any{"user_id": g.aliceID, "token_id": g.aliceTok}
		if edit != nil {
			edit(m)
		}
		introspecttest.WriteJSON(w, m)
	})
}

func (g *teamRig) team(t *testing.T, method, path string) identityResp {
	t.Helper()
	h := validHandle(t)
	g.secrets = append(g.secrets, h)
	return g.call(t, g.tls, method, path, g.alice, teamHeader(h), "", true)
}

// readPaths are the seven task and epic read routes, in alpha (granted) or
// beta (not granted), plus the context report.
func (g *teamRig) readPaths(granted bool) []string {
	if granted {
		return []string{
			"/v1/projects/alpha/tasks", "/v1/tasks/" + g.alphaTask, "/v1/tasks/" + g.alphaTask + "/history",
			"/v1/tasks/" + g.alphaTask + "/comments", "/v1/tasks/" + g.alphaTask + "/comments/" + g.comment,
			"/v1/projects/alpha/epics", "/v1/projects/alpha/epics/" + g.epic,
		}
	}
	return []string{
		"/v1/projects/beta/tasks", "/v1/tasks/" + g.betaTk, "/v1/tasks/" + g.betaTk + "/history",
		"/v1/tasks/" + g.betaTk + "/comments", "/v1/projects/beta/epics", "/v1/projects/beta/epics/missing",
	}
}

func (g *teamRig) audit(t *testing.T) []access.Event {
	t.Helper()
	snap, err := g.db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap.Audit
}

func refusal(t *testing.T, r identityResp) (code, mode, cid string) {
	t.Helper()
	var e struct {
		Code          string `json:"code"`
		ActiveMode    string `json:"active_mode"`
		CorrelationID string `json:"correlation_id"`
	}
	json.Unmarshal(r.body, &e)
	return e.Code, e.ActiveMode, e.CorrelationID
}

// TestTeamRoutesAreTheApprovedMatrix pins teamRoutes to the E4 matrix and to
// real GET routes of the hub.
func TestTeamRoutesAreTheApprovedMatrix(t *testing.T) {
	want := []string{
		"GET /v1/access/identity", "GET /v1/projects/{p}/tasks", "GET /v1/tasks/{id}", "GET /v1/tasks/{id}/history",
		"GET /v1/tasks/{id}/comments", "GET /v1/tasks/{id}/comments/{c}", "GET /v1/projects/{p}/epics", "GET /v1/projects/{p}/epics/{e}",
	}
	if !reflect.DeepEqual(teamRoutes, want) {
		t.Fatalf("teamRoutes changed without review: %v", teamRoutes)
	}
	s, _ := testServer(t)
	routes := map[string]bool{}
	for _, rt := range s.Routes() {
		routes[rt.Method+" "+rt.Pattern] = true
	}
	for _, p := range teamRoutes {
		if !routes[p] {
			t.Errorf("team route %s is not a hub route", p)
		}
	}
}

func TestTeamReadsServeTheGrantedProject(t *testing.T) {
	g := newTeamRig(t)
	for _, p := range g.readPaths(true) {
		before := g.fake.Calls()
		team := g.team(t, "GET", p)
		personal := g.call(t, g.tls, "GET", p, g.alice, nil, "", true)
		if team.status != 200 || personal.status != 200 || string(team.body) != string(personal.body) {
			t.Errorf("GET %s: team %d, personal %d\nteam:     %s\npersonal: %s", p, team.status, personal.status, team.body, personal.body)
		}
		if n := g.fake.Calls() - before; n != 1 {
			t.Errorf("GET %s made %d introspection calls; exactly one per team request, none for personal", p, n)
		}
	}
	verified := 0
	for _, ev := range g.audit(t) {
		if ev.Action == "team.verified" {
			verified++
			for _, want := range []string{"service=aicrew-example", "team=team-1", "session=sess-1", "generation=4", "role=worker", "route=", "correlation="} {
				if !strings.Contains(ev.Subject, want) {
					t.Errorf("audit subject %q lacks %s", ev.Subject, want)
				}
			}
			if ev.Actor != "user:"+g.aliceID {
				t.Errorf("audit actor %q", ev.Actor)
			}
		}
	}
	if verified != len(g.readPaths(true)) {
		t.Fatalf("%d verified team requests audited, want %d", verified, len(g.readPaths(true)))
	}
	g.assertNoSecretLeak(t)
}

// TestTeamReadsRefuseAnUngrantedProject also proves there is no union: alice
// has a personal grant on alpha, and team mode ignores it.
func TestTeamReadsRefuseAnUngrantedProject(t *testing.T) {
	g := newTeamRig(t)
	for _, p := range g.readPaths(false) {
		r := g.team(t, "GET", p)
		code, mode, cid := refusal(t, r)
		if r.status != 403 || code != "grant_denied" || mode != "team" || cid == "" {
			t.Errorf("GET %s in team mode: %d %s", p, r.status, r.body)
		}
		subject := g.assertRefusalAudited(t, r, "user:"+g.aliceID)
		for _, want := range []string{"service=aicrew-example", "team=team-1", "session=sess-1", "generation=4", "role=worker", "route=", `project="beta"`} {
			if !strings.Contains(subject, want) {
				t.Errorf("GET %s: grant refusal audit %q lacks %s", p, subject, want)
			}
		}
	}
	if err := g.db.SetTeamGrant("admin", g.alphaInstance, g.profile.ID, false); err != nil {
		t.Fatal(err)
	}
	r := g.team(t, "GET", "/v1/tasks/"+g.alphaTask)
	code, _, cid := refusal(t, r)
	if code != "grant_denied" {
		t.Fatalf("a personal grant filled a missing profile grant: %d %s", r.status, r.body)
	}
	if p := g.call(t, g.tls, "GET", "/v1/tasks/"+g.alphaTask, g.alice, nil, "", true); p.status != 200 {
		t.Fatalf("personal read lost: %d", p.status)
	}
	found := false
	for _, ev := range g.audit(t) {
		if ev.Action == "team.refused.grant_denied" && strings.Contains(ev.Subject, "correlation="+cid) {
			found = true
		}
	}
	if !found {
		t.Fatal("the grant refusal is not audited under its correlation ID")
	}
	g.assertNoSecretLeak(t)
}

func TestTeamContextReport(t *testing.T) {
	g := newTeamRig(t)
	r := g.team(t, "GET", "/v1/access/identity?project=beta")
	var out struct {
		Mode      string            `json:"mode"`
		UserID    string            `json:"user_id"`
		Team      map[string]string `json:"team"`
		Projects  []string          `json:"projects"`
		Knowledge string            `json:"knowledge"`
		TaskWrite bool              `json:"task_write"`
		Project   string            `json:"project"`
		Read      bool              `json:"project_read"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil {
		t.Fatalf("report: %d %s", r.status, r.body)
	}
	if out.Mode != "team" || out.UserID != g.aliceID || out.Team["team_id"] != "team-1" || out.Team["session_id"] != "sess-1" ||
		out.Team["role"] != "worker" || !reflect.DeepEqual(out.Projects, []string{"alpha"}) || out.Knowledge != "unavailable" ||
		out.TaskWrite || out.Project != "beta" || out.Read {
		t.Fatalf("report: %s", r.body)
	}
	if r := g.team(t, "GET", "/v1/access/identity?project=alpha"); !strings.Contains(string(r.body), `"project_read":true`) {
		t.Fatalf("report for alpha: %s", r.body)
	}
	g.assertNoSecretLeak(t)
}

func TestTeamVerifierRefusals(t *testing.T) {
	g := newTeamRig(t)
	path := "/v1/tasks/" + g.alphaTask
	check := func(name, code string, status int) {
		t.Helper()
		r := g.team(t, "GET", path)
		got, mode, _ := refusal(t, r)
		if r.status != status || got != code || mode != "team" {
			t.Errorf("%s: %d %s, want %d %s", name, r.status, r.body, status, code)
		}
		if subject := g.assertRefusalAudited(t, r, "user:"+g.aliceID); !strings.Contains(subject, "reason=") {
			t.Errorf("%s: the audit does not name the reason: %q", name, subject)
		}
	}
	for name, tc := range map[string]struct {
		edit   func(m map[string]any)
		code   string
		status int
	}{
		"another user": {func(m map[string]any) {
			m["identity"] = map[string]any{"user_id": "user-other", "token_id": g.aliceTok}
		}, "identity_mismatch", 403},
		"another token": {func(m map[string]any) {
			m["identity"] = map[string]any{"user_id": g.aliceID, "token_id": "tok-rotated"}
		}, "context_stale", 403},
		"another hub":   {func(m map[string]any) { m["hub_id"] = "hub-other" }, "identity_mismatch", 403},
		"unlinked team": {func(m map[string]any) { m["team_id"] = "team-2" }, "context_stale", 403},
		"expired handle": {func(m map[string]any) {
			m["handle_expires_at"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
		}, "context_stale", 403},
		"untrusted reply": {func(m map[string]any) { m["grant"] = "all" }, "context_unavailable", 503},
	} {
		g.answerActive(tc.edit)
		check(name, tc.code, tc.status)
	}
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
	check("inactive session", "context_stale", 403)

	g.answerActive(nil)
	if err := g.db.SetTeamProfileDisabled("admin", g.profile.ID, true); err != nil {
		t.Fatal(err)
	}
	check("disabled profile", "context_stale", 403)
	if err := g.db.SetTeamProfileDisabled("admin", g.profile.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := g.team(t, "GET", path); r.status != 200 {
		t.Fatalf("re-enabled profile: %d %s", r.status, r.body)
	}

	// Refusals that never reach aicrew.
	calls := g.fake.Calls()
	if r := g.call(t, g.tls, "PUT", "/v1/identity/peers/aicrew-example", g.env, nil, `{"disabled":true}`, true); r.status != 200 {
		t.Fatal(r.status)
	}
	check("peer disabled", "context_unavailable", 503)
	g.call(t, g.tls, "PUT", "/v1/identity/peers/aicrew-example", g.env, nil, `{"disabled":false}`, true)
	os.Rename(g.tokenFile, g.tokenFile+".moved")
	check("credential file gone", "context_unavailable", 503)
	os.Rename(g.tokenFile+".moved", g.tokenFile)
	if g.fake.Calls() != calls {
		t.Fatalf("a non-operational peer was called %d times", g.fake.Calls()-calls)
	}
	g.fake.Srv.Close()
	check("aicrew down", "context_unavailable", 503)
	g.assertNoSecretLeak(t)
}

// TestTeamVerifierRechecksEveryRequest: nothing is cached; a revocation on
// either side denies the very next request.
func TestTeamVerifierRechecksEveryRequest(t *testing.T) {
	g := newTeamRig(t)
	path := "/v1/tasks/" + g.alphaTask
	if r := g.team(t, "GET", path); r.status != 200 {
		t.Fatalf("first read: %d %s", r.status, r.body)
	}
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
	if r := g.team(t, "GET", path); r.status != 403 {
		t.Fatalf("aicrew ended the session, the next read got %d", r.status)
	}
	g.answerActive(nil)
	if r := g.team(t, "GET", path); r.status != 200 {
		t.Fatalf("session active again: %d", r.status)
	}
	if err := g.db.SetTeamGrant("admin", g.alphaInstance, g.profile.ID, false); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := refusal(t, g.team(t, "GET", path)); code != "grant_denied" {
		t.Fatal("a removed profile grant still allowed the next read")
	}
	if err := g.db.SetTeamGrant("admin", g.alphaInstance, g.profile.ID, true); err != nil {
		t.Fatal(err)
	}
	calls := g.fake.Calls()
	if err := g.db.Revoke("admin", g.aliceTok); err != nil {
		t.Fatal(err)
	}
	r := g.team(t, "GET", path)
	if code, _, _ := refusal(t, r); r.status != 401 || code != "invalid_credential" || g.fake.Calls() != calls {
		t.Fatalf("revoked token: %d %s after %d calls", r.status, r.body, g.fake.Calls()-calls)
	}
}

func TestTeamVerifierCancellationReachesAicrew(t *testing.T) {
	g := newTeamRig(t)
	abandoned := g.fake.StallUntilAbandoned()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", g.tls.URL+"/v1/tasks/"+g.alphaTask, nil)
	req.Header.Set("Authorization", "Bearer "+g.alice)
	req.Header.Set(teamContextHeader, validHandle(t))
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if resp, err := g.tls.Client().Do(req); err == nil {
		resp.Body.Close()
	}
	select {
	case <-abandoned:
		if d := time.Since(start); d > time.Second {
			t.Fatalf("aicrew's call was abandoned only after %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the introspection outlived the cancelled request")
	}
}

// TestPersonalModeNeverContactsAicrew runs personal requests on every team
// route with a fully operational peer.
func TestPersonalModeNeverContactsAicrew(t *testing.T) {
	g := newTeamRig(t)
	for _, p := range append(g.readPaths(true), "/v1/access/identity?project=alpha") {
		if r := g.call(t, g.tls, "GET", p, g.alice, nil, "", true); r.status != 200 || strings.Contains(string(r.body), `"mode":"team"`) {
			t.Errorf("personal GET %s: %d %s", p, r.status, r.body)
		}
	}
	if g.fake.Calls() != 0 {
		t.Fatalf("personal requests made %d introspection calls", g.fake.Calls())
	}
}
