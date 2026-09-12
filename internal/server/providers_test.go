package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"aimem/internal/provider"
)

// The provider test button must tell the truth about the configured
// provider: a model bound to a tokenless provider is reported as exactly
// that — never quietly served by the host's env endpoint (which once
// produced another vendor's model-name error for a Hetzner binding).
func TestProviderTestNamesTokenlessProvider(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	t.Setenv("AIMEM_OPENAI_API_KEY", "envkey") // the trap: an env fallback IS available
	t.Setenv("AIMEM_OPENAI_BASE_URL", "http://127.0.0.1:9/never")

	w := req(t, h, "PUT", "/v1/config/providers",
		`{"set_provider":{"name":"hz","kind":"openai","base_url":"https://hz.example/v1","token":""}}`)
	if w.Code != 200 {
		t.Fatalf("set_provider: %d %s", w.Code, w.Body)
	}
	w = req(t, h, "PUT", "/v1/config/providers", `{"bind":{"model":"hz/qwen","provider":"hz","upstream":"qwen"}}`)
	if w.Code != 200 {
		t.Fatalf("bind: %d %s", w.Code, w.Body)
	}

	w = req(t, h, "POST", "/v1/config/providers/test", `{"model":"hz/qwen","op":"chat"}`)
	if w.Code != 400 {
		t.Fatalf("tokenless binding must be refused up front, got %d %s", w.Code, w.Body)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &e)
	if !strings.Contains(e.Error, `provider "hz"`) || !strings.Contains(e.Error, "no token") {
		t.Fatalf("error must name the provider and the missing token: %s", w.Body)
	}

	w = req(t, h, "GET", "/v1/config/providers/hz/models", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "no token") {
		t.Fatalf("model list must explain the missing token, got %d %s", w.Code, w.Body)
	}

	// The list surfaces the state so the console can show it.
	w = req(t, h, "GET", "/v1/config/providers", "")
	var got struct {
		Providers map[string]map[string]string `json:"providers"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Providers["hz"]["token"] != "" {
		t.Fatalf("masked token should be empty for a tokenless provider: %+v", got.Providers["hz"])
	}
}

// A probe that reaches the provider and fails carries the elapsed time
// in the exact "(after Nms)" shape the console's regex keys on.
func TestProviderTestFailureCarriesElapsed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
	}))
	defer up.Close()
	s, _ := testServer(t)
	h := s.Handler()
	req(t, h, "PUT", "/v1/config/providers",
		`{"set_provider":{"name":"p","kind":"openai","base_url":"`+up.URL+`","token":"k"}}`)
	req(t, h, "PUT", "/v1/config/providers", `{"bind":{"model":"p/m","provider":"p","upstream":"m"}}`)
	w := req(t, h, "POST", "/v1/config/providers/test", `{"model":"p/m","op":"chat"}`)
	if w.Code != 502 {
		t.Fatalf("probe failure status: %d %s", w.Code, w.Body)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &e)
	if !strings.Contains(e.Error, "upstream exploded") || !regexp.MustCompile(`\(after \d+ms\)$`).MatchString(e.Error) {
		t.Fatalf("probe error must end with the elapsed stamp: %q", e.Error)
	}
}

// Blank token on save keeps the stored one (server side). The console
// half — not clearing its field on a rejected save — is pinned in
// TestConsoleProviderFormRules.
func TestProviderSaveBlankTokenKeepsStored(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	w := req(t, h, "PUT", "/v1/config/providers",
		`{"set_provider":{"name":"p","kind":"openai","base_url":"https://p.example/v1","token":"secret-token-1"}}`)
	if w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	// Uppercase name is rejected — the rejection that ate a real token.
	w = req(t, h, "PUT", "/v1/config/providers",
		`{"set_provider":{"name":"Bad","kind":"openai","base_url":"https://p.example/v1","token":"secret-token-2"}}`)
	if w.Code != 400 {
		t.Fatalf("uppercase name accepted: %d", w.Code)
	}
	w = req(t, h, "PUT", "/v1/config/providers",
		`{"set_provider":{"name":"p","kind":"openai","base_url":"https://p.example/v1","token":""}}`)
	if w.Code != 200 {
		t.Fatalf("re-save: %d %s", w.Code, w.Body)
	}
	var got struct {
		Providers map[string]map[string]string `json:"providers"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Providers["p"]["token"] != "••••en-1" {
		t.Fatalf("blank token did not keep the stored one: %+v", got.Providers["p"])
	}
}

// The hub's curate-extractor factory reports the real reason too.
func TestCurateSynthExplains(t *testing.T) {
	s, reg := testServer(t)
	t.Setenv("AIMEM_CURATE_BACKEND", "openai")
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	t.Setenv("AIMEM_CURATE_MODEL", "")
	if _, _, err := s.curateSynth(); err == nil || !strings.Contains(err.Error(), "AIMEM_CURATE_MODEL is unset") {
		t.Fatalf("unset model: %v", err)
	}
	r := provider.Load(reg.Root())
	r.Providers["hollow"] = provider.Provider{Kind: "openai"}
	r.Bind("hollow/m", "hollow", "m")
	r.Save(reg.Root())
	t.Setenv("AIMEM_CURATE_MODEL", "hollow/m")
	if _, _, err := s.curateSynth(); err == nil || !strings.Contains(err.Error(), `provider "hollow"`) || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("tokenless binding: %v", err)
	}
}

// Console-to-server couplings that drift silently, pinned by scraping
// the embedded page (same precedent as TestReviewWindowsMatchConsole):
// the token field clears only on a confirmed save AND only if it still
// holds the value that was sent; the badge's "tokenless is fine" kind
// is the same one the resolver exempts.
func TestConsoleProviderFormRules(t *testing.T) {
	page := string(adminHTML)
	save := page[strings.Index(page, "async function saveProvider"):]
	save = save[:strings.Index(save, "\n}")]
	if !regexp.MustCompile(`if\(ok && \$\("pvTok"\)\.value\.trim\(\)===token\) \$\("pvTok"\)\.value=""`).MatchString(save) {
		t.Fatalf("token field must clear only on a confirmed save of the same value:\n%s", save)
	}
	if strings.Count(save, `$("pvTok").value=""`) != 1 {
		t.Fatalf("token field cleared on an unguarded path:\n%s", save)
	}
	if !strings.Contains(page, `p.kind==="claude"`) {
		t.Fatal("badge kind rule not found in console")
	}
	// The Go rule the badge mirrors: a tokenless claude-kind provider is
	// healthy; a tokenless openai-kind one is not.
	root := t.TempDir()
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	r := provider.Load(root)
	r.Providers["cl"] = provider.Provider{Kind: "claude"}
	r.Providers["oa"] = provider.Provider{Kind: "openai"}
	r.Bind("a", "cl", "")
	r.Bind("b", "oa", "")
	r.Save(root)
	if why := provider.Explain(root, "a", ""); why != "" {
		t.Fatalf("tokenless claude provider must be healthy: %q", why)
	}
	if why := provider.Explain(root, "b", ""); !strings.Contains(why, "no token") {
		t.Fatalf("tokenless openai provider must be flagged: %q", why)
	}
}
