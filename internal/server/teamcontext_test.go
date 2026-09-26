package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/introspect"
	"aimem/internal/introspect/introspecttest"
	"aimem/internal/privatefile"
	"aimem/internal/store"
)

const testIntrospectionBearer = "aicrew_introspect_TESTSECRET_hub"

func teamHeader(h string) map[string]string { return map[string]string{teamContextHeader: h} }

func validHandle(t *testing.T) string {
	t.Helper()
	h, err := introspect.NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// concretePath fills a route pattern's wildcards with plausible values.
func concretePath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, "{") {
			parts[i] = "alpha"
			if strings.Contains(p, "id") || p == "{c}" || p == "{e}" || p == "{attempt}" || p == "{task}" {
				parts[i] = "01a0de81-0000-7000-8000-000000000001"
			}
		}
	}
	return strings.Join(parts, "/")
}

// TestTeamModeIsRefusedOnEveryRoute sends a well-formed team-mode request
// with a live individual credential to every route, the MCP endpoint and the
// public pages. Outside teamRoutes each is refused with the envelope before
// any handler runs; the team routes verify first, and this hub has no
// operational peer. None is ever served as a personal request.
func TestTeamModeIsRefusedOnEveryRoute(t *testing.T) {
	g := newIdentityRig(t)
	h := validHandle(t)
	var targets [][2]string
	for _, rt := range g.s.Routes() {
		targets = append(targets, [2]string{rt.Method, concretePath(rt.Pattern)})
	}
	targets = append(targets, [2]string{"POST", "/mcp"})
	for p := range g.s.publicGETs() {
		targets = append(targets, [2]string{"GET", p})
	}
	body := `{"title":"team write","peer_service_id":"x","hub_id":"` + g.hub + `","challenge_id":"c"}`
	for _, tg := range targets {
		hdr := teamHeader(h)
		hdr[identityVersionHeader] = "1"
		r := g.call(t, g.tls, tg[0], tg[1], g.alice, hdr, body, true)
		req := httptest.NewRequest(tg[0], tg[1], nil)
		want, status := "team_operation_unsupported", http.StatusForbidden
		if teamRouteServed(req) {
			want, status = "context_unavailable", http.StatusServiceUnavailable
		}
		if r.status != status || r.code() != want || !strings.Contains(string(r.body), `"active_mode":"team"`) {
			t.Errorf("%s %s in team mode: %d %s", tg[0], tg[1], r.status, r.body)
		}
		if r.header.Get("Cache-Control") != "no-store" || strings.Contains(string(r.body), h) {
			t.Errorf("%s %s: the refusal is cacheable or echoes the handle", tg[0], tg[1])
		}
	}
	if g.mcpHits != 0 {
		t.Fatalf("the MCP handler ran %d times for team-mode requests", g.mcpHits)
	}
	db, err := g.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range snap.Audit {
		if strings.HasPrefix(ev.Action, "identity.proof") {
			t.Fatalf("a team-mode request reached the proof ledger: %s", ev.Action)
		}
	}
	pdb, err := g.s.reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := pdb.ListTasks(store.TaskFilter{}); err != nil || len(tasks.Tasks) != 0 {
		t.Fatalf("a team-mode request wrote a task: %v %v", tasks, err)
	}
	g.assertNoSecretLeak(t)
}

func TestTeamModeRefusalOrder(t *testing.T) {
	g := newIdentityRig(t)
	h := validHandle(t)
	_, peerSecret := func() (string, string) {
		g.registerPeer(t, "aicrew-example")
		return g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	}()
	get := func(bearer string, headers map[string]string) identityResp {
		return g.call(t, g.tls, "GET", "/v1/access/identity", bearer, headers, "", true)
	}
	for name, tc := range map[string]struct {
		bearer string
		hdr    map[string]string
		status int
		code   string
	}{
		"no bearer":            {"", teamHeader(h), 401, "invalid_credential"},
		"unknown bearer":       {"aimem_user_unknown", teamHeader(h), 401, "invalid_credential"},
		"unknown admin bearer": {"not-a-token", teamHeader(h), 401, "invalid_credential"},
		"empty handle":         {g.alice, teamHeader(""), 400, "invalid_request"},
		"malformed handle":     {g.alice, teamHeader("acs1_short"), 400, "invalid_request"},
		"proof receipt":        {g.alice, teamHeader("amr1_" + strings.Repeat("A", 43)), 400, "invalid_request"},
		"admin credential":     {g.env, teamHeader(h), 403, "credential_scope_forbidden"},
		"project credential":   {g.project, teamHeader(h), 403, "credential_scope_forbidden"},
		"peer credential":      {peerSecret, teamHeader(h), 403, "credential_scope_forbidden"},
		"individual, no peer":  {g.alice, teamHeader(h), 503, "context_unavailable"},
	} {
		r := get(tc.bearer, tc.hdr)
		if r.status != tc.status || r.code() != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, r.status, r.body, tc.status, tc.code)
		}
	}
	// Two header values are one malformed request, not a choice.
	req, _ := http.NewRequest("GET", g.tls.URL+"/v1/access/identity", nil)
	req.Header.Set("Authorization", "Bearer "+g.alice)
	req.Header.Add(teamContextHeader, h)
	req.Header.Add(teamContextHeader, validHandle(t))
	resp, err := g.tls.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("two team-context headers: %d", resp.StatusCode)
	}
	// Plain HTTP is refused the same way; team mode never degrades to
	// personal mode on any listener.
	if r := g.call(t, g.plain, "GET", "/v1/tasks/x", g.alice, teamHeader(h), "", true); r.code() != "context_unavailable" {
		t.Fatalf("plain HTTP team read: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.plain, "POST", "/v1/projects/alpha/tasks", g.alice, teamHeader(h), "{}", true); r.code() != "team_operation_unsupported" {
		t.Fatalf("plain HTTP team request: %d %s", r.status, r.body)
	}
	// Without the header nothing changes.
	if r := get(g.alice, nil); r.status != 200 {
		t.Fatalf("personal identity check: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/status", "", nil, "", true); r.status != 200 {
		t.Fatalf("public status page: %d", r.status)
	}
	if r := g.call(t, g.tls, "GET", "/v1/access/identity", "", nil, "", true); r.status != 401 || len(r.body) != 0 {
		t.Fatalf("personal request without a bearer: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

// TestLocalSocketRefusesTeamMode serves the hub on its real local socket.
// Its caller is the operator, never an individual credential, so a team-mode
// request is refused there as well.
func TestLocalSocketRefusesTeamMode(t *testing.T) {
	root := t.TempDir()
	sock := filepath.Join(root, "t.sock")
	t.Setenv("AIMEM_SOCKET", sock)
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	s := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer s.Close()
	srv, ln, err := s.ListenAndServe(root)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}
	defer func() { srv.Close(); ln.Close(); os.Remove(sock) }()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	get := func(hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", "http://aimem/v1/status", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Code string `json:"code"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Code
	}
	if st, code := get(teamHeader(validHandle(t))); st != 403 || code != "credential_scope_forbidden" {
		t.Fatalf("team-mode request on the socket: %d %s", st, code)
	}
	if st, code := get(teamHeader("garbage")); st != 400 || code != "invalid_request" {
		t.Fatalf("malformed team-mode request on the socket: %d %s", st, code)
	}
	if st, _ := get(nil); st != 200 {
		t.Fatalf("personal request on the socket: %d", st)
	}
}

// introspectionRig is an identity rig whose registered peer is a fake aicrew
// that the hub trusts, with the hub's introspection credential in a private
// file.
type introspectionRig struct {
	*identityRig
	fake      *introspecttest.Fake
	tokenFile string
}

func newIntrospectionRig(t *testing.T, endpointPath string) *introspectionRig {
	t.Helper()
	g := newIdentityRig(t)
	f := introspecttest.New(t, "aicrew-example", g.hub)
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
	tokenFile := filepath.Join(t.TempDir(), "introspection.token")
	fh, err := privatefile.Create(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(testIntrospectionBearer + "\n")
	fh.Close()
	g.s.SetIntrospectionClient(&introspect.Client{TokenFile: tokenFile, RootCAs: f.CA.Pool})
	g.secrets = append(g.secrets, testIntrospectionBearer)
	body := `{"service_id":"aicrew-example","introspection_endpoint":"` + f.Srv.URL + endpointPath + `","tls_trust":{"mode":"spki_sha256","value":"` + f.Pin + `"}}`
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers", g.env, nil, body, true); r.status != 201 {
		t.Fatalf("register peer: %d %s", r.status, r.body)
	}
	return &introspectionRig{g, f, tokenFile}
}

type checkResult struct {
	ServiceID string `json:"service_id"`
	OK        bool   `json:"ok"`
	Outcome   string `json:"outcome"`
}

func (g *introspectionRig) check(t *testing.T) checkResult {
	t.Helper()
	r := g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-example/check", g.env, nil, "", true)
	if r.status != 200 {
		t.Fatalf("peer check: %d %s", r.status, r.body)
	}
	var out checkResult
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (g *introspectionRig) operational(t *testing.T) bool {
	t.Helper()
	r := g.call(t, g.tls, "GET", "/v1/identity/peers", g.env, nil, "", true)
	var out struct {
		Peers []struct {
			Introspection bool `json:"introspection_operational"`
		} `json:"peers"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &out) != nil || len(out.Peers) != 1 {
		t.Fatalf("list peers: %d %s", r.status, r.body)
	}
	return out.Peers[0].Introspection
}

func TestPeerCheckEndToEnd(t *testing.T) {
	g := newIntrospectionRig(t, introspecttest.Path)
	if !g.operational(t) {
		t.Fatal("a complete peer with a private credential file is not operational")
	}
	if got := g.check(t); !got.OK || got.Outcome != "inactive" || got.ServiceID != "aicrew-example" {
		t.Fatalf("healthy peer: %+v", got)
	}
	seen := g.fake.Last()
	if g.fake.Calls() != 1 || seen.Header.Get("Authorization") != "Bearer "+testIntrospectionBearer ||
		seen.Header.Get("X-Aimem-Identity-Version") != "1" || seen.Body.HubID != g.hub || !introspect.ValidHandle(seen.Body.Handle) {
		t.Fatalf("the check's request: %d calls, %+v", g.fake.Calls(), seen)
	}
	probe := seen.Body.Handle
	g.secrets = append(g.secrets, probe)

	// A handle nobody holds must never be active.
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.ActiveReply(got.Nonce, "aicrew-example", g.hub))
	})
	if got := g.check(t); got.OK || got.Outcome != "unexpected_active" {
		t.Fatalf("peer answering active for a random handle: %+v", got)
	}
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		w.WriteHeader(http.StatusBadRequest)
		introspecttest.WriteJSON(w, map[string]string{"code": "unsupported_version"})
	})
	if got := g.check(t); got.OK || got.Outcome != "version_rejected" {
		t.Fatalf("peer refusing the version: %+v", got)
	}
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply("n-00000000000000000000000000000000"))
	})
	if got := g.check(t); got.OK || got.Outcome != "nonce_mismatch" {
		t.Fatalf("peer answering another nonce: %+v", got)
	}

	// Every check is audited with its outcome, and nothing secret leaks.
	db, err := g.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range snap.Audit {
		if strings.HasPrefix(ev.Action, "identity_peer.check.") {
			actions = append(actions, ev.Action+"@"+ev.Subject)
		}
		if strings.Contains(ev.Action+ev.Subject+ev.Actor, "acs1_") || strings.Contains(ev.Action+ev.Subject, testIntrospectionBearer) {
			t.Fatalf("the audit records a secret: %+v", ev)
		}
	}
	want := "identity_peer.check.inactive@aicrew-example"
	if len(actions) != 4 || actions[len(actions)-1] != want {
		t.Fatalf("audited checks: %v", actions)
	}
	if strings.Contains(g.logs.String(), "acs1_") {
		t.Fatal("a log line carries a handle")
	}
	g.assertNoSecretLeak(t)

	// The check is an admin operation over hub TLS only.
	if r := g.call(t, g.plain, "POST", "/v1/identity/peers/aicrew-example/check", g.env, nil, "", true); r.code() != "tls_required" {
		t.Fatalf("check over plain HTTP: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-example/check", g.alice, nil, "", true); r.status != 403 {
		t.Fatalf("check by an ordinary credential: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers/unknown/check", g.env, nil, "", true); r.status != 404 {
		t.Fatalf("check of an unknown peer: %d %s", r.status, r.body)
	}
	if g.fake.Calls() != 4 {
		t.Fatalf("the fake saw %d calls; refused checks must not call it", g.fake.Calls())
	}
}

func TestPeerCheckNotOperational(t *testing.T) {
	g := newIntrospectionRig(t, introspecttest.Path)
	cases := []struct {
		name    string
		setup   func()
		undo    func()
		outcome string
	}{
		{"no credential file configured", func() {
			g.s.SetIntrospectionClient(&introspect.Client{RootCAs: g.fake.CA.Pool})
		}, func() {
			g.s.SetIntrospectionClient(&introspect.Client{TokenFile: g.tokenFile, RootCAs: g.fake.CA.Pool})
		}, "not_configured"},
		{"credential file readable by others", func() {
			if err := privatefile.Expose(g.tokenFile); err != nil {
				t.Fatal(err)
			}
		}, func() {
			os.Remove(g.tokenFile)
			fh, err := privatefile.Create(g.tokenFile)
			if err != nil {
				t.Fatal(err)
			}
			fh.WriteString(testIntrospectionBearer)
			fh.Close()
		}, "credential_file"},
		{"peer disabled", func() {
			if r := g.call(t, g.tls, "PUT", "/v1/identity/peers/aicrew-example", g.env, nil, `{"disabled":true}`, true); r.status != 200 {
				t.Fatalf("disable: %d %s", r.status, r.body)
			}
		}, func() {
			if r := g.call(t, g.tls, "PUT", "/v1/identity/peers/aicrew-example", g.env, nil, `{"disabled":false}`, true); r.status != 200 {
				t.Fatalf("enable: %d %s", r.status, r.body)
			}
		}, "peer_disabled"},
	}
	for _, tc := range cases {
		tc.setup()
		if g.operational(t) {
			t.Errorf("%s: reported operational", tc.name)
		}
		if got := g.check(t); got.OK || got.Outcome != tc.outcome {
			t.Errorf("%s: %+v", tc.name, got)
		}
		tc.undo()
		if !g.operational(t) {
			t.Errorf("%s: not operational after the fix", tc.name)
		}
	}
	if g.fake.Calls() != 0 {
		t.Fatalf("a non-operational peer was called %d times", g.fake.Calls())
	}
	// An unreachable peer is operational on paper; the check shows the fault.
	g.fake.Srv.Close()
	if got := g.check(t); got.OK || got.Outcome != "transport" {
		t.Fatalf("unreachable peer: %+v", got)
	}
}

func TestPeerWithBaseEndpointIsNotOperational(t *testing.T) {
	g := newIntrospectionRig(t, "")
	if g.operational(t) {
		t.Fatal("an endpoint without the introspection path is operational")
	}
	if got := g.check(t); got.OK || got.Outcome != "peer_record" || g.fake.Calls() != 0 {
		t.Fatalf("base-URL endpoint: %+v after %d calls", got, g.fake.Calls())
	}
}

// TestServedRefusalsMatchTheContract pins every refusal the hub serves to the
// identity.v1 contract's status table.
func TestServedRefusalsMatchTheContract(t *testing.T) {
	var spec struct {
		RefusalStatus map[string]int `json:"x-refusal-status"`
	}
	readIdentityFixture(t, "openapi-proposal.json", &spec)
	for code, ref := range identityRefusals {
		want, ok := spec.RefusalStatus[code]
		if !ok || want != ref.status || ref.retryable != (want == 429 || want == 503) {
			t.Errorf("%s: served %d (retryable %v), contract %d", code, ref.status, ref.retryable, want)
		}
		if strings.Contains(strings.ToLower(ref.next), "personal") {
			t.Errorf("%s: the next action points at personal credentials", code)
		}
	}
}
