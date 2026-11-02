package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/textcheck"
)

func TestMain(m *testing.M) {
	// The readability check's second stage would use a model the
	// developer's environment names; tests that want it inject a fake.
	os.Setenv(textcheck.ModelKey, "")
	os.Setenv(textcheck.FilterKey, "")
	// On a loaded machine the store can take longer than the identity
	// routes' wait; the test about that wait sets the contract value back.
	identityWait = time.Minute
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

// The hub does not judge where a provider points: a model server on this
// machine, the local network or the container host saves like a public
// one, plain http included, with no setting. The scheme rule is the one
// kept: base_url is http(s) with a host, anything else is refused.
func TestProviderURLAcceptsAnyHTTPEndpoint(t *testing.T) {
	h, _ := loggedServer(t)
	for _, u := range []string{
		"http://127.0.0.1:11434/v1",
		"http://10.0.0.5:4000/v1",
		"http://host.docker.internal:11434/v1",
		"http://localhost:8080/v1",
		"https://[::1]:8443/v1",
		"http://llm.example/v1",
		"https://llm.example/v1",
	} {
		if w := setProvider(t, h, "p", u, "k"); w.Code != 200 {
			t.Errorf("%s refused: %d %s", u, w.Code, w.Body)
		} else if got := storedProvider(t, h, "p"); got["base_url"] != u {
			t.Errorf("%s not stored: %+v", u, got)
		}
	}
	for _, u := range []string{"ftp://llm.example/v1", "llm.example/v1", "https:///v1", "http://"} {
		if w := setProvider(t, h, "q", u, "k"); w.Code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d %s", u, w.Code, w.Body)
		}
	}
}

// The admin buttons reach a model server on loopback with no setting:
// the model list and the test probe of a model bound to it both answer.
func TestProviderButtonsReachALocalServer(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"data":[{"id":"m"}],"usage":{"total_tokens":1}}`))
	}))
	defer up.Close()
	s, _ := testServer(t)
	h := s.Handler()
	if w := setProvider(t, h, "p", up.URL, "k"); w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	req(t, h, "PUT", "/v1/config/providers", `{"bind":{"model":"p/m","provider":"p","upstream":"m"}}`)
	if w := req(t, h, "GET", "/v1/config/providers/p/models", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"m"`) {
		t.Fatalf("model list of a loopback server: %d %s", w.Code, w.Body)
	}
	if w := req(t, h, "POST", "/v1/config/providers/test", `{"model":"p/m","op":"chat"}`); w.Code != 200 {
		t.Fatalf("probe of a loopback server: %d %s", w.Code, w.Body)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("%d requests reached the server, want 2", n)
	}
}
