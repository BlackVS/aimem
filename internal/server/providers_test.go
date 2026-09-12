package server

import (
	"encoding/json"
	"strings"
	"testing"
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

// Blank token on save keeps the stored one — but only when one IS
// stored; the console must not clear its field on a rejected save.
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
