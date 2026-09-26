package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/server"
	"aimem/internal/store"
)

const identityAdminToken = "env-admin-secret-for-identity-cli-tests"

// identityCLIRig is a real hub behind httptest's TLS server, driven through
// server.TCPHandler (the gate ListenTCP serves), plus the operator's files.
type identityCLIRig struct {
	ts        *httptest.Server
	caFile    string
	pin       string
	tokenFile string
	dir       string
	requests  atomic.Int64
	outputs   []string
}

func newIdentityCLIRig(t *testing.T, wrap func(http.Handler) http.Handler) *identityCLIRig {
	t.Helper()
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	s := server.New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { s.Close() })
	g := &identityCLIRig{dir: t.TempDir()}
	var h http.Handler = s.TCPHandler(identityAdminToken, nil)
	if wrap != nil {
		h = wrap(h)
	}
	counted := h
	g.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		counted.ServeHTTP(w, r)
	}))
	t.Cleanup(g.ts.Close)
	g.caFile = filepath.Join(g.dir, "hub-ca.pem")
	if err := os.WriteFile(g.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.ts.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(g.ts.Certificate().RawSubjectPublicKeyInfo)
	g.pin = "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
	g.tokenFile = filepath.Join(g.dir, "admin.token")
	f, err := createPrivateFile(g.tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(identityAdminToken + "\n")
	f.Close()
	return g
}

// run executes one identity command with the rig's hub flags (CA trust)
// appended, and records its output and error text for the leak scan.
func (g *identityCLIRig) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append(append([]string{}, args...), "--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-ca-file", g.caFile)
	return g.runRaw(t, full...)
}

func (g *identityCLIRig) runRaw(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runIdentity(args, &out)
	g.outputs = append(g.outputs, out.String())
	if err != nil {
		g.outputs = append(g.outputs, err.Error())
	}
	return out.String(), err
}

func (g *identityCLIRig) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := g.run(t, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func (g *identityCLIRig) register(t *testing.T) {
	t.Helper()
	g.mustRun(t, "peer", "register", "aicrew-example", "--endpoint", "https://aicrew.example/v1/crew/introspect", "--peer-trust-dns")
}

func (g *identityCLIRig) secretPath(name string) string { return filepath.Join(g.dir, name) }

// creds lists the peer's credentials as "id state" lines.
func (g *identityCLIRig) creds(t *testing.T) []string {
	t.Helper()
	out := g.mustRun(t, "cred", "list", "aicrew-example")
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && len(f[0]) > 30 {
			lines = append(lines, f[0]+" "+f[1])
		}
	}
	return lines
}

// assertNoSecrets: neither the admin bearer nor any peer bearer ever appears
// in anything the CLI printed or returned.
func (g *identityCLIRig) assertNoSecrets(t *testing.T) {
	t.Helper()
	for _, o := range g.outputs {
		if strings.Contains(o, identityAdminToken) || strings.Contains(o, "aimem_peer_") {
			t.Errorf("CLI output leaks a secret: %.120q", o)
		}
	}
}

func TestIdentityCLIHubTrust(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	if out, err := g.run(t, "peer", "list"); err != nil || !strings.Contains(out, "no identity peer") {
		t.Fatalf("CA-file trust: %v %s", err, out)
	}
	if out, err := g.runRaw(t, "peer", "list", "--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-pin", g.pin); err != nil {
		t.Fatalf("pin trust: %v %s", err, out)
	}
	// httptest serves every TLS server with one built-in certificate, so a
	// different CA and pin come from a freshly generated certificate.
	otherCert := newSelfSignedCert(t)
	otherCA := filepath.Join(g.dir, "other-ca.pem")
	os.WriteFile(otherCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherCert.Raw}), 0o644)
	otherSum := sha256.Sum256(otherCert.RawSubjectPublicKeyInfo)
	otherPin := "sha256-" + base64.StdEncoding.EncodeToString(otherSum[:])
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	before := g.requests.Load()
	for name, args := range map[string][]string{
		"system roots only":    {"--hub", g.ts.URL, "--admin-token-file", g.tokenFile},
		"wrong CA":             {"--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-ca-file", otherCA},
		"wrong pin":            {"--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-pin", otherPin},
		"malformed pin":        {"--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-pin", "sha256-short"},
		"both CA and pin":      {"--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-ca-file", g.caFile, "--hub-pin", g.pin},
		"plain http URL":       {"--hub", strings.Replace(g.ts.URL, "https", "http", 1), "--admin-token-file", g.tokenFile, "--hub-ca-file", g.caFile},
		"https to plain port":  {"--hub", strings.Replace(plain.URL, "http", "https", 1), "--admin-token-file", g.tokenFile, "--hub-ca-file", g.caFile},
		"missing hub":          {"--admin-token-file", g.tokenFile, "--hub-ca-file", g.caFile},
		"hub with a path":      {"--hub", g.ts.URL + "/elsewhere", "--admin-token-file", g.tokenFile, "--hub-ca-file", g.caFile},
		"insecure flag exists": {"--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--insecure"},
	} {
		if out, err := g.runRaw(t, append([]string{"peer", "list"}, args...)...); err == nil {
			t.Errorf("%s: accepted\n%s", name, out)
		}
	}
	if got := g.requests.Load() - before; got != 0 {
		t.Errorf("%d requests reached the hub through refused trust settings", got)
	}
	g.assertNoSecrets(t)
}

func TestIdentityCLIAdminTokenFile(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	loose := g.secretPath("loose.token")
	os.WriteFile(loose, []byte(identityAdminToken+"\n"), 0o600)
	makeWorldReadable(t, loose)
	empty := g.secretPath("empty.token")
	f, _ := createPrivateFile(empty)
	f.Close()
	two := g.secretPath("two.token")
	f, _ = createPrivateFile(two)
	f.WriteString("one two\n")
	f.Close()
	before := g.requests.Load()
	for name, args := range map[string][]string{
		"no token file":       {"--hub", g.ts.URL, "--hub-ca-file", g.caFile},
		"missing token file":  {"--hub", g.ts.URL, "--hub-ca-file", g.caFile, "--admin-token-file", g.secretPath("nope")},
		"readable by others":  {"--hub", g.ts.URL, "--hub-ca-file", g.caFile, "--admin-token-file", loose},
		"empty token file":    {"--hub", g.ts.URL, "--hub-ca-file", g.caFile, "--admin-token-file", empty},
		"two words":           {"--hub", g.ts.URL, "--hub-ca-file", g.caFile, "--admin-token-file", two},
		"token as a argument": {"--hub", g.ts.URL, "--hub-ca-file", g.caFile, "--admin-token", identityAdminToken},
	} {
		if out, err := g.runRaw(t, append([]string{"peer", "list"}, args...)...); err == nil {
			t.Errorf("%s: accepted\n%s", name, out)
		}
	}
	if got := g.requests.Load() - before; got != 0 {
		t.Errorf("%d requests sent without an acceptable token file", got)
	}
	// There is no environment fallback.
	t.Setenv("AIMEM_HTTP_TOKEN", identityAdminToken)
	if _, err := g.runRaw(t, "peer", "list", "--hub", g.ts.URL, "--hub-ca-file", g.caFile); err == nil ||
		!strings.Contains(err.Error(), "--admin-token-file is required") {
		t.Errorf("the admin token must come from --admin-token-file only: %v", err)
	}
	// A wrong token is the hub's refusal, shown without the token.
	wrong := g.secretPath("wrong.token")
	f, _ = createPrivateFile(wrong)
	f.WriteString("not-the-admin-token\n")
	f.Close()
	if _, err := g.runRaw(t, "peer", "list", "--hub", g.ts.URL, "--hub-ca-file", g.caFile, "--admin-token-file", wrong); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: %v", err)
	}
	g.assertNoSecrets(t)
}

func TestIdentityCLIPeerLifecycle(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	for name, args := range map[string][]string{
		"no trust":   {"peer", "register", "aicrew-example", "--endpoint", "https://aicrew.example/x"},
		"both trust": {"peer", "register", "aicrew-example", "--endpoint", "https://aicrew.example/x", "--peer-trust-dns", "--peer-trust-pin", "sha256-x"},
	} {
		if _, err := g.run(t, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	g.register(t)
	out := g.mustRun(t, "peer", "list")
	if !strings.Contains(out, "aicrew-example  enabled") || !strings.Contains(out, "ca_dns aicrew.example") || !strings.Contains(out, "not operational") {
		t.Errorf("peer list: %s", out)
	}
	pin := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := g.run(t, "peer", "register", "aicrew-second", "--endpoint", "https://aicrew.example/x", "--peer-trust-pin", pin); err == nil ||
		!strings.Contains(err.Error(), "another active") {
		t.Errorf("second active peer: %v", err)
	}
	g.mustRun(t, "peer", "disable", "aicrew-example")
	if out := g.mustRun(t, "peer", "list"); !strings.Contains(out, "aicrew-example  disabled") {
		t.Errorf("after disable: %s", out)
	}
	g.mustRun(t, "peer", "enable", "aicrew-example")
	if out := g.mustRun(t, "peer", "list"); !strings.Contains(out, "aicrew-example  enabled") {
		t.Errorf("after enable: %s", out)
	}
	g.assertNoSecrets(t)
}

func TestIdentityCLIIssueDeliversOnlyToTheSecretFile(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	g.register(t)
	path := g.secretPath("peer.secret")
	out := g.mustRun(t, "cred", "issue", "aicrew-example", "--expires", "90d", "--secret-file", path)
	if !strings.Contains(out, "issued credential") || !strings.Contains(out, path) {
		t.Errorf("issue output: %s", out)
	}
	if err := checkPrivateFile(path); err != nil {
		t.Errorf("secret file is not private: %v", err)
	}
	raw, err := os.ReadFile(path)
	secret := strings.TrimSpace(string(raw))
	if err != nil || !strings.HasPrefix(secret, "aimem_peer_") {
		t.Fatalf("secret file content: %v", err)
	}
	// The delivered bearer is a live peer credential: the redemption route
	// gets past the gate (400 for the empty body, not 401).
	req, _ := http.NewRequest("POST", g.ts.URL+"/v1/identity/peers/aicrew-example/redemptions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Aimem-Identity-Version", "1")
	resp, err := g.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("the delivered bearer does not authenticate")
	}
	if got := g.creds(t); len(got) != 1 || !strings.HasSuffix(got[0], "active") {
		t.Fatalf("credentials after issue: %v", got)
	}

	// The destination is checked before issuing: an existing file and a
	// missing directory are refused with nothing issued or overwritten.
	existing := g.secretPath("existing.secret")
	os.WriteFile(existing, []byte("keep me"), 0o600)
	for name, p := range map[string]string{"existing file": existing, "missing directory": filepath.Join(g.dir, "no-such-dir", "s")} {
		if _, err := g.run(t, "cred", "issue", "aicrew-example", "--expires", "90d", "--secret-file", p); err == nil || !strings.Contains(err.Error(), "nothing was issued") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep me" {
		t.Error("an existing file was overwritten")
	}
	if got := g.creds(t); len(got) != 1 {
		t.Fatalf("a refused destination still issued a credential: %v", got)
	}
	// A hub refusal leaves no reserved file behind.
	if _, err := g.run(t, "cred", "issue", "aicrew-unknown", "--expires", "90d", "--secret-file", g.secretPath("refused.secret")); err == nil {
		t.Error("issue for an unknown peer succeeded")
	}
	if _, err := os.Stat(g.secretPath("refused.secret")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the reserved secret file survived a refusal")
	}
	g.assertNoSecrets(t)
}

func TestIdentityCLIConfirmedButUndeliverableIsRevoked(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	g.register(t)
	saved := deliverSecret
	deliverSecret = func(*os.File, string) error { return errors.New("disk full") }
	defer func() { deliverSecret = saved }()
	path := g.secretPath("lost.secret")
	_, err := g.run(t, "cred", "issue", "aicrew-example", "--expires", "90d", "--secret-file", path)
	if err == nil || !strings.Contains(err.Error(), "has been revoked") {
		t.Fatalf("undeliverable credential: %v", err)
	}
	if got := g.creds(t); len(got) != 1 || !strings.HasSuffix(got[0], "revoked") {
		t.Fatalf("the undeliverable credential is not revoked: %v", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("the secret file survived an undeliverable issue")
	}
	g.assertNoSecrets(t)
}

func TestIdentityCLIRevokeFailureIsReported(t *testing.T) {
	failDelete := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	g := newIdentityCLIRig(t, failDelete)
	g.register(t)
	saved := deliverSecret
	deliverSecret = func(*os.File, string) error { return errors.New("disk full") }
	defer func() { deliverSecret = saved }()
	_, err := g.run(t, "cred", "issue", "aicrew-example", "--expires", "90d", "--secret-file", g.secretPath("s"))
	got := g.creds(t)
	if err == nil || !strings.Contains(err.Error(), "revoking it FAILED") || len(got) != 1 ||
		!strings.Contains(err.Error(), "aimem identity cred revoke aicrew-example "+strings.Fields(got[0])[0]) {
		t.Fatalf("revoke failure: %v (%v)", err, got)
	}
	g.assertNoSecrets(t)
}

func TestIdentityCLIUnknownOutcomeNeverGuesses(t *testing.T) {
	// The hub commits the credential, then the connection drops before any
	// answer reaches the CLI. A credential issued earlier must never be named.
	var drop atomic.Bool
	var issueRequests atomic.Int64
	dropAfterIssue := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if drop.Load() && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/credentials") {
				issueRequests.Add(1)
				h.ServeHTTP(httptest.NewRecorder(), r)
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	g := newIdentityCLIRig(t, dropAfterIssue)
	g.register(t)
	g.mustRun(t, "cred", "issue", "aicrew-example", "--expires", "90d", "--secret-file", g.secretPath("earlier.secret"))
	earlier := strings.Fields(g.creds(t)[0])[0]
	drop.Store(true)
	path := g.secretPath("unknown.secret")
	out, err := g.run(t, "cred", "issue", "aicrew-example", "--expires", "90d", "--secret-file", path)
	drop.Store(false)
	if n := issueRequests.Load(); n != 1 {
		t.Errorf("an unknown outcome sent %d issue requests; it must never reissue", n)
	}
	got := g.creds(t)
	if err == nil || len(got) != 2 || !strings.HasSuffix(got[0], "active") || !strings.HasSuffix(got[1], "active") {
		t.Fatalf("unknown outcome: %v, credentials %v", err, got)
	}
	var id string
	for _, c := range got {
		if f := strings.Fields(c)[0]; f != earlier {
			id = f
		}
	}
	if !strings.Contains(out, "unknown") || !strings.Contains(out, "nothing was reissued and nothing was revoked") ||
		!strings.Contains(out, "aimem identity cred revoke aicrew-example "+id) {
		t.Errorf("unknown-outcome guidance: %s", out)
	}
	if strings.Contains(out, earlier) {
		t.Errorf("a credential that existed before the request was named as a candidate: %s", out)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("the secret file survived an unknown outcome")
	}

	// A 5xx without any credential created names no candidates.
	g2 := newIdentityCLIRig(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/credentials") {
				http.Error(w, "busy", http.StatusBadGateway)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	g2.register(t)
	out, err = g2.run(t, "cred", "issue", "aicrew-example", "--expires", "90d", "--secret-file", g2.secretPath("s"))
	if err == nil || !strings.Contains(out, "safe to issue again") || len(g2.creds(t)) != 0 {
		t.Errorf("5xx outcome: %v %s", err, out)
	}
	g.assertNoSecrets(t)
	g2.assertNoSecrets(t)
}

func TestIdentityCLIRotationAndRevoke(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	g.register(t)
	if _, err := g.run(t, "cred", "rotate", "aicrew-example", "--expires", "30d", "--secret-file", g.secretPath("r0")); err == nil ||
		!strings.Contains(err.Error(), "no active credential") {
		t.Errorf("rotate with none active: %v", err)
	}
	g.mustRun(t, "cred", "issue", "aicrew-example", "--expires", "30d", "--secret-file", g.secretPath("first"))
	oldID := strings.Fields(g.creds(t)[0])[0]
	out := g.mustRun(t, "cred", "rotate", "aicrew-example", "--expires", "30d", "--secret-file", g.secretPath("second"))
	if !strings.Contains(out, "aimem identity cred revoke aicrew-example "+oldID) {
		t.Errorf("rotate does not name the explicit revoke: %s", out)
	}
	got := g.creds(t)
	if len(got) != 2 || !strings.HasSuffix(got[0], "active") || !strings.HasSuffix(got[1], "active") {
		t.Fatalf("rotate must leave both active until the operator revokes: %v", got)
	}
	third := g.secretPath("third")
	if _, err := g.run(t, "cred", "rotate", "aicrew-example", "--expires", "30d", "--secret-file", third); err == nil ||
		!strings.Contains(err.Error(), "two active") {
		t.Errorf("rotate with two active: %v", err)
	}
	if _, err := os.Stat(third); !errors.Is(err, os.ErrNotExist) || len(g.creds(t)) != 2 {
		t.Error("a refused rotation issued or kept a secret file")
	}
	if _, err := g.run(t, "cred", "revoke", "aicrew-other", oldID); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("revoke under the wrong peer: %v", err)
	}
	g.mustRun(t, "cred", "revoke", "aicrew-example", oldID)
	active := 0
	for _, c := range g.creds(t) {
		if strings.HasSuffix(c, "active") {
			active++
		}
	}
	if active != 1 {
		t.Errorf("after revoking the old credential: %v", g.creds(t))
	}
	g.assertNoSecrets(t)
}

// TestIdentityCLIScrubsAnEchoedAdminToken: even a hub that echoes the
// presented bearer in a refusal cannot make the CLI print it.
func TestIdentityCLIScrubsAnEchoedAdminToken(t *testing.T) {
	echo := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"rejected `+r.Header.Get("Authorization")+`"}`, http.StatusForbidden)
		})
	}
	g := newIdentityCLIRig(t, echo)
	_, err := g.run(t, "peer", "list")
	if err == nil || !strings.Contains(err.Error(), "[admin token]") {
		t.Fatalf("echoed token not scrubbed: %v", err)
	}
	g.assertNoSecrets(t)
}

func TestIdentityCLIUsage(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{nil, {"peer"}, {"cred", "issue"}, {"cred", "frob", "x"}, {"cred", "revoke", "only-one"}} {
		if err := runIdentity(args, &out); err == nil || !strings.Contains(err.Error(), "usage: aimem identity") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := parseIdentityExpiry("0d", time.Now()); err == nil {
		t.Error("zero-day expiry accepted")
	}
}

// newSelfSignedCert makes a CA certificate unrelated to httptest's built-in
// one, valid for the same loopback address.
func newSelfSignedCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "other test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
