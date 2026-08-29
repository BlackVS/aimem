package server

// Admin surface for the per-model provider registry
// (docs/DESIGN-multiprovider.md): masked GET, atomic PUT, and a live
// per-model test probe. Tokens never leave this host — the GET masks
// them, logs carry name and base_url only, and the registry file itself
// is 0600 outside any sync path.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"aimem/internal/curate"
	"aimem/internal/provider"
	"aimem/internal/store"
)

var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func maskToken(t string) string {
	if t == "" {
		return ""
	}
	if len(t) >= 8 {
		return "••••" + t[len(t)-4:]
	}
	return "••••"
}

func (s *Server) getProviders(w http.ResponseWriter, _ *http.Request) {
	reg := provider.Load(s.reg.Root())
	provs := map[string]map[string]string{}
	for name, p := range reg.Providers {
		provs[name] = map[string]string{
			"kind": p.Kind, "base_url": p.BaseURL, "token": maskToken(p.Token),
		}
	}
	models := map[string]map[string]string{}
	for m, b := range reg.Models {
		models[m] = map[string]string{"provider": b.Provider, "model": b.Model}
	}
	s.ok(w, map[string]any{"providers": provs, "models": models})
}

// putProviders applies exactly one mutation per request: set_provider
// (blank token keeps the stored one), delete_provider, bind, or unbind.
func (s *Server) putProviders(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetProvider *struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			BaseURL string `json:"base_url"`
			Token   string `json:"token"`
		} `json:"set_provider"`
		DeleteProvider string `json:"delete_provider"`
		Bind           *struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
			Upstream string `json:"upstream"` // payload model name; "" = same as model
		} `json:"bind"`
		Unbind string `json:"unbind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	reg := provider.Load(s.reg.Root())
	switch {
	case req.SetProvider != nil:
		p := req.SetProvider
		if !providerName.MatchString(p.Name) {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("provider name must be lowercase [a-z0-9._-]"))
			return
		}
		if p.Kind != "openai" && p.Kind != "claude" {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("kind must be openai or claude"))
			return
		}
		if strings.ContainsAny(p.BaseURL+p.Token, "\n\"$`\\ ") {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("base_url/token must not contain spaces, quotes, or shell metacharacters"))
			return
		}
		if p.BaseURL != "" && !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://") {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("base_url must be http(s)"))
			return
		}
		token := p.Token
		if token == "" { // blank on save means: keep what's stored
			token = reg.Providers[p.Name].Token
		}
		reg.Providers[p.Name] = provider.Provider{Kind: p.Kind, BaseURL: p.BaseURL, Token: token}
		s.log.Warn("admin provider set", "name", p.Name, "kind", p.Kind, "base_url", p.BaseURL)
	case req.DeleteProvider != "":
		delete(reg.Providers, req.DeleteProvider)
		for m, b := range reg.Models {
			if b.Provider == req.DeleteProvider {
				delete(reg.Models, m)
			}
		}
		s.log.Warn("admin provider deleted", "name", req.DeleteProvider)
	case req.Bind != nil:
		if req.Bind.Model == "" {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("bind wants a model name"))
			return
		}
		if _, ok := reg.Providers[req.Bind.Provider]; !ok {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("no provider %q", req.Bind.Provider))
			return
		}
		reg.Bind(req.Bind.Model, req.Bind.Provider, req.Bind.Upstream)
		s.log.Warn("admin model bound", "model", req.Bind.Model, "provider", req.Bind.Provider, "upstream", req.Bind.Upstream)
	case req.Unbind != "":
		delete(reg.Models, req.Unbind)
		s.log.Warn("admin model unbound", "model", req.Unbind)
	default:
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body wants one of set_provider, delete_provider, bind, unbind"))
		return
	}
	if err := reg.Save(s.reg.Root()); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.getProviders(w, r)
}

// testProvider makes one minimal live call through the model's resolved
// endpoint and reports ok+latency+tokens or the provider's error verbatim.
// It never writes facts or embeddings; the few tokens spent are metered
// into the user DB's run history so even test spend stays on the books.
func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Op    string `json:"op"` // "embed" | "chat" (default chat)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body wants {\"model\": \"...\", \"op\": \"embed|chat\"}"))
		return
	}
	ep, ok := provider.Resolve(s.reg.Root(), req.Model)
	if !ok {
		s.log.Warn("provider test unresolved", "model", req.Model)
		s.fail(w, http.StatusBadRequest, fmt.Errorf("no endpoint for model %q (no binding, no env fallback)", req.Model))
		return
	}
	start := time.Now()
	var tokens int64
	var err error
	if ep.Kind == "claude" {
		workDir := filepath.Join(s.reg.Root(), "curator-workdir")
		os.MkdirAll(workDir, 0o700)
		ex := &curate.ClaudeExtractor{Model: ep.Model, WorkDir: workDir}
		var u curate.Usage
		_, u, err = ex.Complete("Reply with the single word: ok")
		tokens = u.InputTokens + u.OutputTokens
	} else {
		tokens, err = probeOpenAI(ep, req.Op)
	}
	if err != nil {
		// Same shape as the success line below: a failure nobody can
		// attribute to a model is nearly useless in the Log tab.
		s.log.Warn("provider test failed", "model", req.Model, "op", req.Op,
			"ms", time.Since(start).Milliseconds(), "err", err)
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	if db, derr := s.reg.Open(store.UserScopeProject); derr == nil {
		host, _ := os.Hostname()
		_ = db.AddCurateRun(&store.CurateRun{
			TS: time.Now().UTC().Format(time.RFC3339), Host: host,
			Model: req.Model, InputTokens: tokens,
		})
	}
	s.log.Info("provider test ok", "model", req.Model, "op", req.Op, "ms", time.Since(start).Milliseconds())
	s.ok(w, map[string]any{"ok": true, "latency_ms": time.Since(start).Milliseconds(), "tokens": tokens})
}

// providerModels proxies GET {base}/models for one provider so the GUI
// can offer a dropdown of real model ids. Server-side with the stored
// token — the browser never sees it. Kind claude has no HTTP list; it
// gets the CLI's known aliases.
func (s *Server) providerModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	reg := provider.Load(s.reg.Root())
	p, ok := reg.Providers[name]
	if !ok {
		s.fail(w, http.StatusNotFound, fmt.Errorf("no provider %q", name))
		return
	}
	if p.Kind == "claude" {
		s.ok(w, map[string]any{"models": []string{"haiku", "sonnet", "opus"}})
		return
	}
	base := p.BaseURL
	if base == "" {
		base = provider.DefaultBaseURL
	}
	hreq, err := http.NewRequest("GET", strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	hreq.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(hreq)
	if err != nil {
		s.log.Warn("provider model list failed", "provider", name, "base_url", base, "err", err)
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode >= 400 {
		s.log.Warn("provider model list failed", "provider", name, "base_url", base,
			"http", resp.StatusCode)
		s.fail(w, http.StatusBadGateway, fmt.Errorf("model list failed (HTTP %d)", resp.StatusCode))
		return
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		// Google's compat endpoint lists ids as "models/<name>" but its
		// chat route rejects that prefix — offer the bare name (both
		// routes accept it).
		ids = append(ids, strings.TrimPrefix(d.ID, "models/"))
	}
	s.ok(w, map[string]any{"models": ids})
}

// probeOpenAI is the HTTP half of the test button: one tiny embeddings or
// chat request with a hard 15s bound so a dead endpoint fails fast.
func probeOpenAI(ep provider.Endpoint, op string) (int64, error) {
	path, payload := "/chat/completions", map[string]any{
		"model":                 ep.Model,
		"messages":              []map[string]string{{"role": "user", "content": "Reply with the single word: ok"}},
		"max_completion_tokens": 16,
	}
	if op == "embed" {
		path, payload = "/embeddings", map[string]any{"model": ep.Model, "input": []string{"aimem provider test"}}
	}
	body, _ := json.Marshal(payload)
	hreq, err := http.NewRequest("POST", strings.TrimRight(ep.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+ep.Token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(hreq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	type apiError struct {
		Message string `json:"message"`
	}
	var out struct {
		Error *apiError `json:"error"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Error == nil {
		// Google's compat endpoint wraps errors in an ARRAY of objects.
		var arr []struct {
			Error *apiError `json:"error"`
		}
		if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
			out.Error = arr[0].Error
		}
	}
	if out.Error != nil {
		return out.Usage.TotalTokens, fmt.Errorf("provider error (HTTP %d): %s", resp.StatusCode, out.Error.Message)
	}
	if resp.StatusCode >= 400 {
		snip := string(raw)
		if len(snip) > 200 {
			snip = snip[:200] + "..."
		}
		return out.Usage.TotalTokens, fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, snip)
	}
	return out.Usage.TotalTokens, nil
}
