package server

// The suite loops the security audit of 2026-10-18 (section 2b) found
// missing: a legacy writer token on every admin route, /mcp at the hub's
// gate with a revoked and an expired token, and a process set asked of one
// project by a commit selected only on another.

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every route the table marks admin refuses the legacy writer token before
// its handler runs: at requireAdmin, or, for an enrollment route, at that
// route's own admin check. The requests go over the hub's TLS listener,
// because the enrollment routes refuse plain HTTP before either check. The
// admin routes named below must stay marked, so the loop cannot pass by
// finding none.
func TestLegacyWriterTokenIsRefusedOnEveryAdminRoute(t *testing.T) {
	g := newIdentityRig(t)
	writer, wd, _ := NewTokenSecret()
	if err := SaveTokens(g.s.reg.Root(), []TokenEntry{{Name: "old-writer", Role: "writer", SHA256: wd}}); err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, writer)
	admin := map[string]bool{}
	for _, rt := range g.s.Routes() {
		if rt.Admin {
			admin[rt.Method+" "+rt.Pattern] = true
		}
	}
	for _, want := range []string{"GET /v1/access", "POST /v1/access/users", "GET /v1/logs", "PUT /v1/config/providers", "GET /v1/identity/enrollments"} {
		if !admin[want] {
			t.Errorf("%s is no longer an admin route", want)
		}
	}
	// The writer is a live token: it reads where a writer may. The env
	// admin token passes the admin gate.
	if r := g.call(t, g.tls, "GET", "/v1/projects", writer, nil, "", true); r.status != 200 {
		t.Fatalf("the legacy writer cannot read /v1/projects: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/access", g.env, nil, "", true); r.status != 200 {
		t.Fatalf("the admin token on GET /v1/access: %d %s", r.status, r.body)
	}
	for _, rt := range g.s.Routes() {
		if !rt.Admin {
			continue
		}
		body := ""
		if rt.Method != "GET" && rt.Method != "DELETE" {
			body = "{}"
		}
		r := g.call(t, g.tls, rt.Method, concretePath(rt.Pattern), writer, nil, body, true)
		// The enrollment routes check the hub admin credential themselves,
		// before requireAdmin, and answer with the identity envelope.
		want := "this route needs an admin token"
		if strings.HasPrefix(rt.Pattern, "/v1/identity/enrollments") {
			want = `"code":"credential_scope_forbidden"`
		}
		if r.status != http.StatusForbidden || !strings.Contains(string(r.body), want) {
			t.Errorf("%s %s with the legacy writer token: %d %s", rt.Method, rt.Pattern, r.status, r.body)
		}
	}
}

// /mcp is mounted behind the hub's gate (TCPHandler): an ordinary token
// that is revoked, or past its expiry, is refused there with 401 and never
// reaches the MCP handler.
func TestMCPAtTheHubGateRefusesARevokedAndAnExpiredToken(t *testing.T) {
	f := newTaskFixture(t)
	hits := 0
	h := f.s.TCPHandler(f.env, map[string]http.Handler{"/mcp": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})})
	call := func(token string) int {
		t.Helper()
		return authedReq(t, h, "POST", "/mcp", token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Code
	}
	db, err := f.s.openAccess(true)
	if err != nil {
		t.Fatal(err)
	}
	// Two live tokens of Alice's reach /mcp: the gate admits them.
	revokedTok, revoked, err := db.Issue("admin", f.aliceUser, "mcp-revoked", f.alphaInstance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, short, err := db.Issue("admin", f.aliceUser, "mcp-short", f.alphaInstance, time.Now().Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if code := call(revoked); code != 200 || hits != 1 {
		t.Fatalf("a live token: %d, %d hits", code, hits)
	}
	if code := call(short); code != 200 || hits != 2 {
		t.Fatalf("a fresh short token: %d, %d hits", code, hits)
	}

	if err := db.Revoke("admin", revokedTok.ID); err != nil {
		t.Fatal(err)
	}
	if code := call(revoked); code != http.StatusUnauthorized || hits != 2 {
		t.Fatalf("a revoked token on /mcp: %d, %d hits", code, hits)
	}
	// The store keeps expiry in whole seconds, so the token lives two to
	// three seconds; wait past the third.
	time.Sleep(3100 * time.Millisecond)
	if code := call(short); code != http.StatusUnauthorized || hits != 2 {
		t.Fatalf("an expired token on /mcp: %d, %d hits", code, hits)
	}
}

// A process set asked of alpha by a commit selected only on beta is
// refused with 404: alpha's own history has no such selection, and the
// answer is never beta's set nor alpha's current one.
func TestProcessSetByACommitSelectedOnlyOnAnotherProjectIsRefused(t *testing.T) {
	f := newTaskFixture(t)
	const repo = "https://example.com/team/p.git"
	alphaCommit, betaCommit := strings.Repeat("a", 40), strings.Repeat("b", 40)
	sel := func(project, commit, handbook string) {
		t.Helper()
		body := withSet(t, `{"repo":"`+repo+`","commit":"`+commit+`","manifest":"m.json","expected_commit":""}`, handbook)
		if w := taskReq(t, f.h, "PUT", "/v1/projects/"+project+"/process", f.admin, "", body); w.Code != 200 {
			t.Fatalf("select on %s: %d %s", project, w.Code, w.Body)
		}
	}
	// The set's files are served base64-encoded.
	alphaRules, betaRules := base64.StdEncoding.EncodeToString([]byte("alpha rules")), base64.StdEncoding.EncodeToString([]byte("beta rules"))
	sel("alpha", alphaCommit, "alpha rules")
	sel("beta", betaCommit, "beta rules")
	set := func(project, token, commit string) (int, string) {
		w := taskReq(t, f.h, "GET", "/v1/projects/"+project+"/process/set?commit="+commit+"&manifest=m.json", token, "", "")
		return w.Code, w.Body.String()
	}
	// Each project serves its own selection by commit.
	if code, b := set("alpha", f.alice, alphaCommit); code != 200 || !strings.Contains(b, alphaRules) {
		t.Fatalf("alpha by its own commit: %d %s", code, b)
	}
	if code, b := set("beta", f.admin, betaCommit); code != 200 || !strings.Contains(b, betaRules) {
		t.Fatalf("beta by its own commit: %d %s", code, b)
	}
	code, b := set("alpha", f.alice, betaCommit)
	if code != http.StatusNotFound || !strings.Contains(b, "no selection of that commit and manifest is recorded for this project") {
		t.Fatalf("alpha by beta's commit: %d %s", code, b)
	}
	if strings.Contains(b, betaRules) || strings.Contains(b, alphaRules) {
		t.Fatalf("the refusal carries a set: %s", b)
	}
}
