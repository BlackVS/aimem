package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

// identityRig is a hub served over real TLS (httptest.NewTLSServer) through
// TCPHandler, the same bearer gate ListenTCP serves, plus a plain-HTTP
// listener on the same handler for the TLS-refusal checks.
type identityRig struct {
	s         *Server
	logs      *lockedBuffer
	tls       *httptest.Server
	plain     *httptest.Server
	env       string
	hub       string
	alice     string // user-scoped individual bearer
	aliceID   string
	aliceTok  string
	project   string // project-scoped bearer of the same user
	mcpHits   int
	mcpMu     sync.Mutex
	secrets   []string
	responses []string // every response body except the two that carry a secret
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newIdentityRig(t *testing.T) *identityRig {
	t.Helper()
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	g := &identityRig{logs: &lockedBuffer{}, env: "env-admin-secret"}
	g.s = New(reg, slog.New(slog.NewTextHandler(g.logs, nil)))
	t.Cleanup(func() { g.s.Close() })
	db, err := g.s.openAccess(true)
	if err != nil {
		t.Fatal(err)
	}
	if g.hub, err = db.HubID(); err != nil {
		t.Fatal(err)
	}
	u, err := db.CreateUser("admin", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := db.IssueScoped("admin", u.ID, "agent", access.ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g.alice, g.aliceID, g.aliceTok = secret, u.ID, tok.ID
	pdb, err := reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	_ = pdb
	instance, err := reg.ProjectAccessID("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetGrant("admin", instance, "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, g.project, err = db.Issue("admin", u.ID, "project", instance, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, g.alice, g.project, g.env)
	h := g.s.TCPHandler(g.env, map[string]http.Handler{"/mcp": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mcpMu.Lock()
		g.mcpHits++
		g.mcpMu.Unlock()
	})})
	g.tls = httptest.NewTLSServer(h)
	g.plain = httptest.NewServer(h)
	t.Cleanup(g.tls.Close)
	t.Cleanup(g.plain.Close)
	return g
}

type identityResp struct {
	status int
	body   []byte
	header http.Header
}

func (r identityResp) code() string {
	var e struct {
		Code string `json:"code"`
	}
	json.Unmarshal(r.body, &e)
	return e.Code
}

// call sends one request. Secret-bearing responses (proof, credential issue)
// are excluded from the later leak scan by the caller passing keep=false.
func (g *identityRig) call(t *testing.T, srv *httptest.Server, method, path, bearer string, headers map[string]string, body string, keep bool) identityResp {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if keep {
		g.responses = append(g.responses, string(b))
	}
	return identityResp{resp.StatusCode, b, resp.Header}
}

var v1 = map[string]string{identityVersionHeader: "1"}

func (g *identityRig) registerPeer(t *testing.T, service string) {
	t.Helper()
	body := `{"service_id":"` + service + `","introspection_endpoint":"https://aicrew.example/v1/crew/introspect","tls_trust":{"mode":"ca_dns","value":"aicrew.example"}}`
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers", g.env, nil, body, true); r.status != 201 {
		t.Fatalf("register peer: %d %s", r.status, r.body)
	}
}

func (g *identityRig) issueCredential(t *testing.T, service string, expires time.Time) (string, string) {
	t.Helper()
	r := g.call(t, g.tls, "POST", "/v1/identity/peers/"+service+"/credentials", g.env, nil,
		`{"expires_at":"`+expires.UTC().Format(time.RFC3339)+`"}`, false)
	if r.status != 201 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("issue credential: %d %s", r.status, r.header.Get("Cache-Control"))
	}
	var out struct {
		Credential struct {
			ID string `json:"id"`
		} `json:"credential"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || !strings.HasPrefix(out.Secret, "aimem_peer_") {
		t.Fatalf("credential response: %v", err)
	}
	g.secrets = append(g.secrets, out.Secret)
	return out.Credential.ID, out.Secret
}

func (g *identityRig) proof(t *testing.T, bearer, service, challenge string) (identityResp, string) {
	t.Helper()
	r := g.call(t, g.tls, "POST", "/v1/identity/proofs", bearer, v1,
		`{"peer_service_id":"`+service+`","hub_id":"`+g.hub+`","challenge_id":"`+challenge+`"}`, false)
	if r.status != 200 {
		g.responses = append(g.responses, string(r.body))
		return r, ""
	}
	var out struct {
		Receipt string `json:"receipt"`
	}
	json.Unmarshal(r.body, &out)
	g.secrets = append(g.secrets, out.Receipt)
	return r, out.Receipt
}

// fakeVerifier stands in for aicrew's production store.Verifier: it encodes
// aicrew's raw request key as k1_ and redeems over the TLS client.
type fakeVerifier struct {
	g       *identityRig
	service string
	bearer  string
}

func k1(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func (v fakeVerifier) redeem(t *testing.T, challenge, receipt, rawKey string) identityResp {
	t.Helper()
	return v.g.call(t, v.g.tls, "POST", "/v1/identity/peers/"+v.service+"/redemptions", v.bearer,
		map[string]string{identityVersionHeader: "1", "Idempotency-Key": k1(rawKey)},
		`{"hub_id":"`+v.g.hub+`","challenge_id":"`+challenge+`","receipt":"`+receipt+`"}`, true)
}

func TestIdentityRoutesRequireHubTLS(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	forwarded := map[string]string{identityVersionHeader: "1", "X-Forwarded-Proto": "https", "X-Forwarded-Ssl": "on", "Forwarded": "proto=https"}
	for name, c := range map[string]struct{ method, path, bearer string }{
		"proof":             {"POST", "/v1/identity/proofs", g.alice},
		"redemption":        {"POST", "/v1/identity/peers/aicrew-example/redemptions", peer},
		"list peers":        {"GET", "/v1/identity/peers", g.env},
		"register peer":     {"POST", "/v1/identity/peers", g.env},
		"update peer":       {"PUT", "/v1/identity/peers/aicrew-example", g.env},
		"list credentials":  {"GET", "/v1/identity/peers/aicrew-example/credentials", g.env},
		"issue credential":  {"POST", "/v1/identity/peers/aicrew-example/credentials", g.env},
		"revoke credential": {"DELETE", "/v1/identity/peers/aicrew-example/credentials/x", g.env},
	} {
		// Plain HTTP on the TCP listener, even with forwarded-TLS headers.
		if r := g.call(t, g.plain, c.method, c.path, c.bearer, forwarded, "{}", true); r.status != 403 || r.code() != "tls_required" {
			t.Errorf("%s over plain HTTP: %d %s", name, r.status, r.body)
		}
		// The unix socket serves Handler() without the bearer gate; it
		// never carries TLS, so identity routes refuse there too.
		w := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		for k, v := range forwarded {
			req.Header.Set(k, v)
		}
		g.s.Handler().ServeHTTP(w, req)
		if w.Code != 403 || !strings.Contains(w.Body.String(), `"tls_required"`) {
			t.Errorf("%s on the unix-socket handler: %d %s", name, w.Code, w.Body)
		}
	}
	// An unrelated route is unchanged over plain HTTP.
	if r := g.call(t, g.plain, "GET", "/v1/access/identity", g.alice, nil, "", true); r.status != 200 {
		t.Errorf("unrelated route changed: %d %s", r.status, r.body)
	}
}

func TestIdentityRoutesEndToEndWithFakeVerifier(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	oldID, oldSecret := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	v := fakeVerifier{g: g, service: "aicrew-example", bearer: oldSecret}

	// Peer listing is metadata only. This hub has no introspection credential
	// file, so introspection is not operational.
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers", g.env, nil, "", true); r.status != 200 ||
		!strings.Contains(string(r.body), `"introspection_operational":false`) {
		t.Fatalf("list peers: %d %s", r.status, r.body)
	}

	// Proof and redemption bind the verified hub, user and token.
	r, receipt := g.proof(t, g.alice, "aicrew-example", "challenge-1")
	if r.status != 200 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("proof: %d %s", r.status, r.body)
	}
	got := v.redeem(t, "challenge-1", receipt, "redeem:challenge-1:complete 1")
	var red struct {
		RedemptionID string `json:"redemption_id"`
		Replayed     bool   `json:"replayed"`
		Identity     struct {
			HubID   string `json:"hub_id"`
			UserID  string `json:"user_id"`
			TokenID string `json:"token_id"`
		} `json:"identity"`
		TokenState string `json:"token_state"`
	}
	if err := json.Unmarshal(got.body, &red); err != nil || got.status != 200 || red.Replayed ||
		red.Identity.HubID != g.hub || red.Identity.UserID != g.aliceID || red.Identity.TokenID != g.aliceTok || red.TokenState != "active" {
		t.Fatalf("redemption: %d %s", got.status, got.body)
	}

	// Lost reply: the same raw key replays the recorded redemption.
	again := v.redeem(t, "challenge-1", receipt, "redeem:challenge-1:complete 1")
	var rep struct {
		RedemptionID string `json:"redemption_id"`
		Replayed     bool   `json:"replayed"`
	}
	if json.Unmarshal(again.body, &rep); again.status != 200 || !rep.Replayed || rep.RedemptionID != red.RedemptionID {
		t.Fatalf("lost-reply retry: %d %s", again.status, again.body)
	}
	// Replay under another key, wrong challenge, and the wrong peer path.
	if r := v.redeem(t, "challenge-1", receipt, "redeem:challenge-1:other"); r.status != 403 || r.code() != "proof_invalid" {
		t.Errorf("second key: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-other/redemptions", oldSecret,
		map[string]string{identityVersionHeader: "1", "Idempotency-Key": k1("x")}, `{"hub_id":"h","challenge_id":"c","receipt":"r"}`, true); r.status != 403 || r.code() != "peer_forbidden" {
		t.Errorf("other peer's path: %d %s", r.status, r.body)
	}
	// Version and key shape are checked before any effect.
	if r := g.call(t, g.tls, "POST", "/v1/identity/proofs", g.alice, nil, `{}`, true); r.status != 400 || r.code() != "unsupported_version" {
		t.Errorf("missing version: %d %s", r.status, r.body)
	}
	_, fresh := g.proof(t, g.alice, "aicrew-example", "challenge-raw-key")
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-example/redemptions", oldSecret,
		map[string]string{identityVersionHeader: "1", "Idempotency-Key": "redeem:challenge-raw-key:k"},
		`{"hub_id":"`+g.hub+`","challenge_id":"challenge-raw-key","receipt":"`+fresh+`"}`, true); r.status != 400 || r.code() != "invalid_request" {
		t.Errorf("raw request key: %d %s", r.status, r.body)
	}
	// Host-managed admin and legacy writer credentials are not individual
	// installation credentials: they cannot get a receipt either.
	writer, wd, _ := NewTokenSecret()
	if err := SaveTokens(g.s.reg.Root(), []TokenEntry{{Name: "old-writer", Role: "writer", SHA256: wd}}); err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, writer)
	for name, bearer := range map[string]string{"env admin": g.env, "legacy writer": writer} {
		if r, _ := g.proof(t, bearer, "aicrew-example", "challenge-legacy"); r.status != 403 || r.code() != "credential_scope_forbidden" {
			t.Errorf("%s proof: %d %s", name, r.status, r.body)
		}
	}
	// A project-scoped token of the same user cannot get a receipt.
	if r, _ := g.proof(t, g.project, "aicrew-example", "challenge-scope"); r.status != 403 || r.code() != "credential_scope_forbidden" {
		t.Errorf("project token: %d %s", r.status, r.body)
	}

	// Racing keys: exactly one redemption of one receipt succeeds.
	_, race := g.proof(t, g.alice, "aicrew-example", "challenge-race")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := v.redeem(t, "challenge-race", race, "race-"+string(rune('a'+i)))
			mu.Lock()
			defer mu.Unlock()
			if r.status == 200 {
				wins++
			} else if r.code() != "proof_invalid" {
				t.Errorf("racing key: %d %s", r.status, r.body)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d winners across racing keys", wins)
	}

	// Rotation: a second credential overlaps, a third is refused, the old
	// one is revoked and stops authenticating; the list never shows secrets.
	_, newSecret := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-example/credentials", g.env, nil,
		`{"expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`, true); r.status != 409 {
		t.Errorf("third credential: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "DELETE", "/v1/identity/peers/aicrew-other/credentials/"+oldID, g.env, nil, "", true); r.status != 404 {
		t.Errorf("revoke under another peer: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "DELETE", "/v1/identity/peers/aicrew-example/credentials/"+oldID, g.env, nil, "", true); r.status != 200 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	_, rot := g.proof(t, g.alice, "aicrew-example", "challenge-rotation")
	if r := v.redeem(t, "challenge-rotation", rot, "rot"); r.status != 401 {
		t.Errorf("revoked peer credential: %d %s", r.status, r.body)
	}
	v.bearer = newSecret
	if r := v.redeem(t, "challenge-rotation", rot, "rot"); r.status != 200 {
		t.Errorf("rotated credential: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/credentials", g.env, nil, "", true); r.status != 200 ||
		strings.Contains(string(r.body), "aimem_peer_") || strings.Count(string(r.body), `"revoked":true`) != 1 {
		t.Errorf("credential list: %d %s", r.status, r.body)
	}

	// Revoking the individual token: its outstanding receipt is
	// credential_inactive, and the token no longer passes the gate.
	_, pending := g.proof(t, g.alice, "aicrew-example", "challenge-revoked")
	db, _ := g.s.openAccess(false)
	if err := db.Revoke("admin", g.aliceTok); err != nil {
		t.Fatal(err)
	}
	if r := v.redeem(t, "challenge-revoked", pending, "rev"); r.status != 403 || r.code() != "credential_inactive" {
		t.Errorf("receipt of a revoked token: %d %s", r.status, r.body)
	}
	if r, _ := g.proof(t, g.alice, "aicrew-example", "challenge-after"); r.status != 401 {
		t.Errorf("revoked token proof: %d", r.status)
	}

	// Disabling the peer refuses its credentials at the gate.
	if r := g.call(t, g.tls, "PUT", "/v1/identity/peers/aicrew-example", g.env, nil, `{"disabled":true}`, true); r.status != 200 {
		t.Fatalf("disable peer: %d %s", r.status, r.body)
	}
	if r := v.redeem(t, "challenge-x", rot, "dis"); r.status != 401 {
		t.Errorf("disabled peer credential: %d %s", r.status, r.body)
	}

	g.assertNoSecretLeak(t)
}

func TestPeerCredentialExpiresOverTheWire(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, secret := g.issueCredential(t, "aicrew-example", time.Now().Add(2*time.Second))
	v := fakeVerifier{g: g, service: "aicrew-example", bearer: secret}
	_, receipt := g.proof(t, g.alice, "aicrew-example", "challenge-exp")
	time.Sleep(2500 * time.Millisecond)
	if r := v.redeem(t, "challenge-exp", receipt, "k"); r.status != 401 {
		t.Errorf("expired peer credential: %d %s", r.status, r.body)
	}
}

// TestPeerCredentialReachesNoOtherRoute sweeps the whole route table and
// /mcp with a valid peer bearer over TLS: only the redemption route answers
// with anything but the gate's refusal, and the MCP dispatcher is never hit.
func TestPeerCredentialReachesNoOtherRoute(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	fill := strings.NewReplacer("{p}", "alpha", "{id}", uuidv7.New(), "{c}", uuidv7.New(), "{s}", "s1", "{key}", "about",
		"{name}", "RUNBOOK", "{instance}", "x", "{kind}", "user", "{g}", "g", "{u}", "u", "{id...}", "x", "{$}", "",
		"{e}", uuidv7.New(), "{team}", "t", "{attempt}", "a", "{task}", uuidv7.New(), "{c...}", "x",
		"{service_id}", "aicrew-example", "{credential_id}", uuidv7.New())
	public := g.s.publicGETs()
	for _, rt := range g.s.Routes() {
		path := fill.Replace(rt.Pattern)
		if path == "" {
			path = "/"
		}
		if public[path] != nil {
			continue
		}
		r := g.call(t, g.tls, rt.Method, path, peer, v1, "{}", true)
		if rt.Method == "POST" && rt.Pattern == "/v1/identity/peers/{service_id}/redemptions" {
			if r.status == 401 || (r.status == 403 && r.code() != "proof_invalid") {
				t.Errorf("redemption route refused its own peer: %d %s", r.status, r.body)
			}
			continue
		}
		if r.status != 403 {
			t.Errorf("%s %s reachable with a peer credential: %d %s", rt.Method, rt.Pattern, r.status, r.body)
		}
	}
	for _, method := range []string{"POST", "GET"} {
		if r := g.call(t, g.tls, method, "/mcp", peer, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, true); r.status != 403 {
			t.Errorf("%s /mcp with a peer credential: %d", method, r.status)
		}
	}
	g.mcpMu.Lock()
	hits := g.mcpHits
	g.mcpMu.Unlock()
	if hits != 0 {
		t.Fatalf("a peer credential reached the MCP dispatcher %d times", hits)
	}
	// An ordinary user credential cannot redeem.
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-example/redemptions", g.alice, v1, "{}", true); r.status != 401 || r.code() != "peer_unauthenticated" {
		t.Errorf("user token on the redemption route: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

// assertNoSecretLeak requires that no bearer, peer secret or receipt appears
// in any kept response body or in the server log.
func (g *identityRig) assertNoSecretLeak(t *testing.T) {
	t.Helper()
	logs := g.logs.String()
	for _, secret := range g.secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(logs, secret) {
			t.Error("server log contains a secret")
		}
		for _, body := range g.responses {
			if strings.Contains(body, secret) {
				t.Errorf("a response body contains a secret: %.80s", body)
			}
		}
	}
}

func TestIdentityWaitMapsToRequestInProgress(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("begin: %w", context.DeadlineExceeded)} {
		if code := identityStoreError(err); code != "request_in_progress" {
			t.Errorf("%v: %s", err, code)
		}
	}
	if ref := identityRefusals["request_in_progress"]; ref.status != 503 || !ref.retryable {
		t.Errorf("request_in_progress must be a retryable 503: %+v", ref)
	}
	if identityWait != 5*time.Second {
		t.Errorf("identity wait %v, contract says 5 s", identityWait)
	}
}

// checkEnvelope requires the complete identity refusal envelope.
func checkEnvelope(t *testing.T, name string, r identityResp, status int, code string) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(r.body, &env); err != nil || r.status != status || env["code"] != code {
		t.Errorf("%s: %d %s, want %d %s", name, r.status, r.body, status, code)
		return
	}
	for _, f := range []string{"message", "next_action", "correlation_id"} {
		if s, _ := env[f].(string); s == "" {
			t.Errorf("%s: envelope missing %s", name, f)
		}
	}
	if _, ok := env["retryable"].(bool); !ok {
		t.Errorf("%s: envelope missing retryable", name)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("%s: refusal is cacheable", name)
	}
}

// TestIdentityGateRefusalsUseTheEnvelope: every refusal the bearer gate makes
// on the two wire routes, before any identity handler runs, is the contract
// envelope with its code.
func TestIdentityGateRefusalsUseTheEnvelope(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	credID, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	proof, redeem := "/v1/identity/proofs", "/v1/identity/peers/aicrew-example/redemptions"
	for name, c := range map[string]struct {
		path, bearer string
		status       int
		code         string
	}{
		"proof without a bearer":       {proof, "", 401, "invalid_credential"},
		"proof with an unknown user":   {proof, "aimem_user_" + strings.Repeat("0", 64), 401, "invalid_credential"},
		"proof with a peer bearer":     {proof, peer, 403, "credential_scope_forbidden"},
		"redeem without a bearer":      {redeem, "", 401, "peer_unauthenticated"},
		"redeem with an unknown peer":  {redeem, "aimem_peer_" + strings.Repeat("0", 64), 401, "peer_unauthenticated"},
		"redeem with a user bearer":    {redeem, g.alice, 401, "peer_unauthenticated"},
		"redeem with a garbage bearer": {redeem, "nonsense", 401, "peer_unauthenticated"},
	} {
		checkEnvelope(t, name, g.call(t, g.tls, "POST", c.path, c.bearer, v1, "{}", true), c.status, c.code)
	}
	// A revoked peer credential and a revoked individual token.
	if r := g.call(t, g.tls, "DELETE", "/v1/identity/peers/aicrew-example/credentials/"+credID, g.env, nil, "", true); r.status != 200 {
		t.Fatal(r.status)
	}
	checkEnvelope(t, "redeem with a revoked peer", g.call(t, g.tls, "POST", redeem, peer, v1, "{}", true), 401, "peer_unauthenticated")
	db, _ := g.s.openAccess(false)
	if err := db.Revoke("admin", g.aliceTok); err != nil {
		t.Fatal(err)
	}
	checkEnvelope(t, "proof with a revoked token", g.call(t, g.tls, "POST", proof, g.alice, v1, "{}", true), 401, "invalid_credential")
	// Non-identity routes keep the gate's existing empty 401.
	if r := g.call(t, g.tls, "GET", "/v1/projects", "", nil, "", true); r.status != 401 || len(r.body) != 0 {
		t.Errorf("unrelated route's gate refusal changed: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

// TestIdentityGateStartsTheDeadlineBeforeAuthentication: over the real TLS
// listener, the context the gate authenticates with carries the wire routes'
// deadline (at most identityWait away); other routes are unchanged.
func TestIdentityGateStartsTheDeadlineBeforeAuthentication(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	var mu sync.Mutex
	seen := map[string]time.Duration{}
	gateAuthHook = func(r *http.Request) {
		left := time.Duration(-1)
		if d, ok := r.Context().Deadline(); ok {
			left = time.Until(d)
		}
		mu.Lock()
		seen[r.URL.Path] = left
		mu.Unlock()
	}
	defer func() { gateAuthHook = nil }()
	g.proof(t, g.alice, "aicrew-example", "challenge-deadline")
	g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-example/redemptions", peer, v1, "{}", true)
	g.call(t, g.tls, "GET", "/v1/projects", g.alice, nil, "", true)
	mu.Lock()
	defer mu.Unlock()
	for _, p := range []string{"/v1/identity/proofs", "/v1/identity/peers/aicrew-example/redemptions"} {
		if left, ok := seen[p]; !ok || left <= 0 || left > identityWait {
			t.Errorf("%s authenticated without the identity deadline (remaining %v)", p, left)
		}
	}
	if left := seen["/v1/projects"]; left != -1 {
		t.Errorf("an unrelated route gained a deadline: %v", left)
	}
}

// TestIdentityAuthenticationObservesTheDeadline: a wire request whose
// deadline is already spent is refused by the gate with request_in_progress.
// The request carries no TLS, so an authentication that ignored the deadline
// would succeed and reach the handler, which answers tls_required instead.
func TestIdentityAuthenticationObservesTheDeadline(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	h := g.s.TCPHandler(g.env, nil)
	for name, c := range map[string]struct{ path, bearer string }{
		"proof":      {"/v1/identity/proofs", g.alice},
		"redemption": {"/v1/identity/peers/aicrew-example/redemptions", peer},
	} {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		req := httptest.NewRequest("POST", c.path, strings.NewReader("{}")).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+c.bearer)
		req.Header.Set(identityVersionHeader, "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		cancel()
		checkEnvelope(t, name, identityResp{w.Code, w.Body.Bytes(), w.Header()}, 503, "request_in_progress")
	}
}

// TestIdentityEncodedPathsMatchTheMux: a percent-encoded spelling that the
// route mux dispatches to an identity wire handler gets exactly what the
// literal path gets from the gate: the refusal envelope, the deadline before
// authentication, and peer confinement (task 01a0dda8, CONFIRMED-RUNTIME
// before the gate classified with the mux's own matching).
func TestIdentityEncodedPathsMatchTheMux(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	var mu sync.Mutex
	remaining := map[string]time.Duration{}
	gateAuthHook = func(r *http.Request) {
		left := time.Duration(-1)
		if d, ok := r.Context().Deadline(); ok {
			left = time.Until(d)
		}
		mu.Lock()
		remaining[r.URL.EscapedPath()] = left
		mu.Unlock()
	}
	defer func() { gateAuthHook = nil }()
	proof, redeem := "/v1/identity/%70roofs", "/v1/identity/peers/aicrew-example/%72edemptions"
	body := `{"peer_service_id":"aicrew-example","hub_id":"` + g.hub + `","challenge_id":"c-encoded"}`

	checkEnvelope(t, "encoded proof, no bearer", g.call(t, g.tls, "POST", proof, "", v1, "{}", true), 401, "invalid_credential")
	checkEnvelope(t, "encoded proof, peer bearer", g.call(t, g.tls, "POST", proof, peer, v1, "{}", true), 403, "credential_scope_forbidden")
	checkEnvelope(t, "encoded proof, project token", g.call(t, g.tls, "POST", proof, g.project, v1, body, true), 403, "credential_scope_forbidden")
	if r := g.call(t, g.tls, "POST", proof, g.alice, v1, body, false); r.status != 200 {
		t.Errorf("encoded proof for a user token must behave like the literal path: %d %s", r.status, r.body)
	}
	checkEnvelope(t, "encoded redeem, no bearer", g.call(t, g.tls, "POST", redeem, "", v1, "{}", true), 401, "peer_unauthenticated")
	checkEnvelope(t, "encoded redeem, user bearer", g.call(t, g.tls, "POST", redeem, g.alice, v1, "{}", true), 401, "peer_unauthenticated")
	// The peer's own encoded path reaches its handler (here refused for the
	// empty body), not the gate's plain 403.
	checkEnvelope(t, "encoded redeem, own peer", g.call(t, g.tls, "POST", redeem, peer, v1, "{}", true), 400, "invalid_request")
	// An encoded slash stays inside {service_id}, as the mux reads it; the
	// handler then refuses a path peer that is not the credential's own.
	checkEnvelope(t, "encoded slash in service_id", g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew%2Fexample/redemptions", peer, v1, "{}", true), 403, "peer_forbidden")

	mu.Lock()
	defer mu.Unlock()
	for _, p := range []string{proof, redeem} {
		if left, ok := remaining[p]; !ok || left <= 0 || left > identityWait {
			t.Errorf("%s authenticated without the identity deadline (remaining %v)", p, left)
		}
	}
	g.assertNoSecretLeak(t)
}
