package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

	// Peer listing is metadata only and says introspection is not operational.
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
	// Legacy writer and ordinary user credentials cannot redeem.
	if r := g.call(t, g.tls, "POST", "/v1/identity/peers/aicrew-example/redemptions", g.alice, v1, "{}", true); r.status != 403 {
		t.Errorf("user token on the redemption route: %d", r.status)
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
