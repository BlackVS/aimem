package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"aimem/internal/provider"
	"aimem/internal/textcheck"
)

// Save-time resolution never touches real DNS in tests: every name is
// unknown unless a test says otherwise.
func TestMain(m *testing.M) {
	lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("no such host")
	}
	// The readability check's second stage would use a model the
	// developer's environment names; tests that want it inject a fake.
	os.Setenv(textcheck.ModelKey, "")
	os.Setenv(textcheck.FilterKey, "")
	os.Exit(m.Run())
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func loggedServer(t *testing.T) (http.Handler, *lockedBuf) {
	t.Helper()
	s, _ := testServer(t)
	logs := &lockedBuf{}
	s.log = slog.New(slog.NewTextHandler(logs, nil))
	return s.Handler(), logs
}

func setProvider(t *testing.T, h http.Handler, name, baseURL, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"set_provider": map[string]string{
		"name": name, "kind": "openai", "base_url": baseURL, "token": token}})
	return req(t, h, "PUT", "/v1/config/providers", string(body))
}

func storedProvider(t *testing.T, h http.Handler, name string) map[string]string {
	t.Helper()
	var got struct {
		Providers map[string]map[string]string `json:"providers"`
	}
	json.Unmarshal(req(t, h, "GET", "/v1/config/providers", "").Body.Bytes(), &got)
	return got.Providers[name]
}

// A-7: the stored token belongs to the endpoint it was saved with. A
// new base_url with a blank token is refused and changes nothing; with
// a token it is saved, and the log names the old URL.
func TestProviderURLChangeNeedsToken(t *testing.T) {
	t.Setenv(allowPrivateEnv, "")
	h, logs := loggedServer(t)
	if w := setProvider(t, h, "p", "https://a.example/v1", "secret-token-1"); w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	for _, moved := range []string{"https://b.example/v1", ""} { // "" is the default endpoint
		w := setProvider(t, h, "p", moved, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "send the token again") {
			t.Fatalf("URL change to %q with the stored token: %d %s", moved, w.Code, w.Body)
		}
	}
	if got := storedProvider(t, h, "p"); got["base_url"] != "https://a.example/v1" || got["token"] != "••••en-1" {
		t.Fatalf("a refused change must leave the provider as it was: %+v", got)
	}
	if !strings.Contains(logs.String(), `msg="admin provider set refused"`) ||
		!strings.Contains(logs.String(), "old_base_url=https://a.example/v1") {
		t.Fatalf("the refusal must be logged with the old URL:\n%s", logs)
	}

	// A trailing slash is the same endpoint: the stored token is kept.
	if w := setProvider(t, h, "p", "https://a.example/v1/", ""); w.Code != 200 {
		t.Fatalf("same endpoint with a blank token: %d %s", w.Code, w.Body)
	}
	if w := setProvider(t, h, "p", "https://b.example/v1", "secret-token-2"); w.Code != 200 {
		t.Fatalf("URL change with a new token: %d %s", w.Code, w.Body)
	}
	if got := storedProvider(t, h, "p"); got["base_url"] != "https://b.example/v1" || got["token"] != "••••en-2" {
		t.Fatalf("change with a token not saved: %+v", got)
	}
	if !strings.Contains(logs.String(), "base_url=https://b.example/v1 old_base_url=https://a.example/v1/ existed=true token=replaced") {
		t.Fatalf("the change must be logged with the old URL and the token state:\n%s", logs)
	}
	if strings.Contains(logs.String(), "secret-token") {
		t.Fatal("a token value reached the log")
	}

	// No stored token, nothing to leak: a tokenless provider may move.
	setProvider(t, h, "bare", "https://a.example/v1", "")
	if w := setProvider(t, h, "bare", "https://c.example/v1", ""); w.Code != 200 {
		t.Fatalf("tokenless provider move: %d %s", w.Code, w.Body)
	}
}

func TestProviderURLPolicy(t *testing.T) {
	t.Setenv(allowPrivateEnv, "")
	h, logs := loggedServer(t)
	refused := []string{
		"http://llm.example/v1",
		"https://127.0.0.1/v1",
		"https://[::1]:8443/v1",
		"https://169.254.169.254/latest",
		"https://10.1.2.3/v1",
		"https://192.168.1.10/v1",
		"https://[fd00::1]/v1",
		"https://[fe80::1]/v1",
		"https://[::ffff:127.0.0.1]/v1",
		"https://0.0.0.0/v1",
		"https://0.0.0.1/v1",
		"https://[::ffff:0.1.2.3]/v1",
		"https://localhost/v1",
		"https://api.localhost./v1",
		"https:///v1",
	}
	for _, u := range refused {
		if w := setProvider(t, h, "p", u, "k"); w.Code != http.StatusBadRequest {
			t.Errorf("%s accepted without %s: %d %s", u, allowPrivateEnv, w.Code, w.Body)
		}
	}
	if !strings.Contains(logs.String(), "base_url=https://169.254.169.254/latest") {
		t.Fatalf("a refused save must be logged:\n%s", logs)
	}

	// A name is judged by every address it resolves to.
	lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "inside.example" {
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.5")}, {IP: net.ParseIP("10.0.0.7")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.5")}}, nil
	}
	t.Cleanup(func() {
		lookupIP = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no such host") }
	})
	if w := setProvider(t, h, "p", "https://inside.example/v1", "k"); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "10.0.0.7") {
		t.Fatalf("a name resolving to a private address: %d %s", w.Code, w.Body)
	}
	if w := setProvider(t, h, "p", "https://public.example/v1", "k"); w.Code != 200 {
		t.Fatalf("a public name: %d %s", w.Code, w.Body)
	}

	t.Setenv(allowPrivateEnv, "1")
	for _, u := range []string{"http://127.0.0.1:11434/v1", "https://inside.example/v1", "http://10.1.2.3/v1"} {
		if w := setProvider(t, h, "local", u, "k"); w.Code != 200 {
			t.Errorf("%s refused with %s=1: %d %s", u, allowPrivateEnv, w.Code, w.Body)
		}
	}
}

// The admin buttons dial through the guard, so an endpoint that points
// at a blocked address now (saved with the flag, or re-pointed by DNS
// since) gets no token sent to it.
func TestProviderButtonsDialGuard(t *testing.T) {
	var hits int
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte(`{"data":[{"id":"m"}],"usage":{"total_tokens":1}}`))
	}))
	defer up.Close()
	t.Setenv(allowPrivateEnv, "1")
	s, _ := testServer(t)
	h := s.Handler()
	if w := setProvider(t, h, "p", up.URL, "k"); w.Code != 200 {
		t.Fatalf("set with the flag: %d %s", w.Code, w.Body)
	}
	req(t, h, "PUT", "/v1/config/providers", `{"bind":{"model":"p/m","provider":"p","upstream":"m"}}`)

	t.Setenv(allowPrivateEnv, "")
	if w := req(t, h, "GET", "/v1/config/providers/p/models", ""); w.Code != http.StatusBadGateway ||
		!strings.Contains(w.Body.String(), "loopback") {
		t.Fatalf("model list to loopback without the flag: %d %s", w.Code, w.Body)
	}
	if w := req(t, h, "POST", "/v1/config/providers/test", `{"model":"p/m","op":"chat"}`); w.Code != http.StatusBadGateway ||
		!strings.Contains(w.Body.String(), "loopback") {
		t.Fatalf("probe to loopback without the flag: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	if hits != 0 {
		t.Fatalf("the guarded client reached the upstream %d times", hits)
	}
	mu.Unlock()

	// An unbound model probes the environment's endpoint, which the
	// operator set and the admin API cannot change: no guard.
	t.Setenv("AIMEM_OPENAI_API_KEY", "envkey")
	t.Setenv("AIMEM_OPENAI_BASE_URL", up.URL)
	if w := req(t, h, "POST", "/v1/config/providers/test", `{"model":"unbound","op":"chat"}`); w.Code != 200 {
		t.Fatalf("probe of the environment's endpoint: %d %s", w.Code, w.Body)
	}

	// The client follows the resolved endpoint, never a later read of the
	// registry: a registry endpoint stays guarded when its binding goes
	// away between resolution and the probe (a concurrent unbind).
	afterProbeResolve = func() {
		reg := provider.Load(s.reg.Root())
		delete(reg.Models, "p/m")
		if err := reg.Save(s.reg.Root()); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterProbeResolve = func() {} })
	if w := req(t, h, "POST", "/v1/config/providers/test", `{"model":"p/m","op":"chat"}`); w.Code != http.StatusBadGateway ||
		!strings.Contains(w.Body.String(), "loopback") {
		t.Fatalf("registry endpoint probed unguarded after a concurrent unbind: %d %s", w.Code, w.Body)
	}
	afterProbeResolve = func() {}
	mu.Lock()
	if hits != 1 {
		t.Fatalf("only the environment probe may reach the upstream, got %d hits", hits)
	}
	mu.Unlock()

	t.Setenv(allowPrivateEnv, "1")
	if w := req(t, h, "GET", "/v1/config/providers/p/models", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"m"`) {
		t.Fatalf("model list with the flag: %d %s", w.Code, w.Body)
	}
}
