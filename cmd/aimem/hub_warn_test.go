package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/adapter"
)

// identityHub answers GET /v1/access/identity as the hub does for the env
// token, a named admin token and a writer token, and refuses anything else.
func identityHub(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/access/identity" {
		http.NotFound(w, r)
		return
	}
	ids := map[string][2]string{
		"Bearer env-secret":    {"env", "admin"},
		"Bearer ops-secret":    {"ops", "admin"},
		"Bearer writer-secret": {"laptop", "writer"},
	}
	id, ok := ids[r.Header.Get("Authorization")]
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"name": id[0], "role": id[1]})
}

func pinOf(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func warnings(t *testing.T, h *adapter.HubConfig) string {
	t.Helper()
	var b strings.Builder
	hubAddWarnings(&b, "home", h)
	if strings.Contains(b.String(), h.Token) {
		t.Fatalf("a warning printed the token:\n%s", b.String())
	}
	return b.String()
}

// `aimem hub add` warns, through the entry's own trust, when the token is
// the hub's env token or another admin token, and stays silent for a writer
// token on a verified https hub.
func TestHubAddWarnsOnAnAdminToken(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(identityHub))
	defer srv.Close()
	pin := pinOf(srv)
	for token, want := range map[string]string{
		"env-secret":    "env token (AIMEM_HTTP_TOKEN), its admin credential: it belongs on no client",
		"ops-secret":    `authenticates as role admin (token "ops")`,
		"writer-secret": "",
		"stale-secret":  "the hub refused the token (HTTP 401)",
	} {
		got := warnings(t, &adapter.HubConfig{URL: srv.URL, Token: token, Pin: pin})
		if want == "" && got != "" || !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want %q", token, got, want)
		}
		if strings.Contains(got, "not https") || strings.Contains(got, "--insecure") {
			t.Errorf("%s: a pinned https hub got a transport warning: %q", token, got)
		}
	}
}

// A plain-http URL and --insecure are warned about, and the role is still
// checked; nothing in the warnings is a refusal.
func TestHubAddWarnsOnPlainHTTPAndInsecure(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(identityHub))
	defer plain.Close()
	got := warnings(t, &adapter.HubConfig{URL: plain.URL, Token: "env-secret"})
	if !strings.Contains(got, "is not https: its token crosses the network in clear") || !strings.Contains(got, "env token") {
		t.Errorf("plain http: %q", got)
	}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(identityHub))
	defer tlsSrv.Close()
	got = warnings(t, &adapter.HubConfig{URL: tlsSrv.URL, Token: "writer-secret", Insecure: true})
	if !strings.Contains(got, "--insecure skips certificate verification") || strings.Contains(got, "not https") || strings.Contains(got, "role") {
		t.Errorf("insecure: %q", got)
	}
}

// An unreachable hub, a certificate the entry does not trust and a redirect
// are warnings that the role could not be checked; a redirect is not
// followed, so the token never reaches the redirect's target.
func TestHubAddWarnsWhenTheRoleCannotBeChecked(t *testing.T) {
	gone := httptest.NewTLSServer(http.HandlerFunc(identityHub))
	goneURL, gonePin := gone.URL, pinOf(gone)
	gone.Close()
	got := warnings(t, &adapter.HubConfig{URL: goneURL, Token: "env-secret", Pin: gonePin})
	if !strings.Contains(got, "could not check the role") || !strings.Contains(got, "unreachable") {
		t.Errorf("unreachable: %q", got)
	}

	// httptest's certificate is in no system root store, and the entry
	// records no trust of its own.
	srv := httptest.NewTLSServer(http.HandlerFunc(identityHub))
	defer srv.Close()
	got = warnings(t, &adapter.HubConfig{URL: srv.URL, Token: "env-secret"})
	if !strings.Contains(got, "could not check the role") || strings.Contains(got, "env token") {
		t.Errorf("untrusted certificate: %q", got)
	}

	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		identityHub(w, r)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL+"/v1/access/identity", http.StatusFound))
	defer redirect.Close()
	got = warnings(t, &adapter.HubConfig{URL: redirect.URL, Token: "env-secret"})
	if !strings.Contains(got, "the hub answered HTTP 302") || reached.Load() {
		t.Errorf("redirect: %q (target reached: %v)", got, reached.Load())
	}
}

// The warnings never block `hub add`: the entry is stored whatever the
// probe finds, including when the hub cannot be reached.
func TestHubAddStoresTheEntryWhateverTheWarnings(t *testing.T) {
	t.Setenv("AIMEM_STATE_DIR", t.TempDir())
	var ran atomic.Int32
	quiet := hubAddWarn
	hubAddWarn = func(w io.Writer, name string, h *adapter.HubConfig) {
		ran.Add(1)
		hubAddWarnings(w, name, h)
	}
	t.Cleanup(func() { hubAddWarn = quiet })
	srv := httptest.NewTLSServer(http.HandlerFunc(identityHub))
	defer srv.Close()
	if err := hubCmd([]string{"add", "home", srv.URL, "env-secret", "--pin", pinOf(srv)}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "home"); h == nil || h.Token != "env-secret" {
		t.Fatalf("an admin token was not stored: %+v", h)
	}
	gone := httptest.NewServer(http.HandlerFunc(identityHub))
	goneURL := gone.URL
	gone.Close()
	if err := hubCmd([]string{"add", "away", goneURL, "writer-secret"}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "away"); h == nil || h.URL != goneURL {
		t.Fatalf("an unreachable hub was not stored: %+v", h)
	}
	if ran.Load() != 2 {
		t.Fatalf("hub add ran the warnings %d times, want 2", ran.Load())
	}
}
