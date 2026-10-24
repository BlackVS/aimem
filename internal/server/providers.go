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
	"net"
	"net/http"
	"net/url"
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

func (s *Server) getProviders(w http.ResponseWriter, _ *http.Request) { s.providersView(w, "") }

// dataExposureNote is the one line a save of a non-local endpoint returns:
// the place the operator makes the setting is where the data decision is
// taken, so the pointer is said there.
const dataExposureNote = "This endpoint is outside this machine: the text aimem sends it (curation's journal batches, the text check's flagged task text) leaves the hub under that vendor's terms, retention and training rules. See docs/ADMIN-MANUAL.md, \"Data sent to model providers\"."

// localEndpoint reports a base URL whose host is this machine or a private
// network, by name or literal address; a name that only resolves to one
// counts as remote, which errs toward saying the note.
func localEndpoint(raw string) bool {
	if raw == "" {
		return false // the default is the vendor's own API
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && blockedIP(ip)
}

func (s *Server) providersView(w http.ResponseWriter, note string) {
	reg := provider.Load(s.reg.Root())
	if reg.LoadErr != nil {
		s.fail(w, http.StatusConflict, fmt.Errorf("provider registry could not be read (%v) — fix the file by hand", reg.LoadErr))
		return
	}
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
	out := map[string]any{"providers": provs, "models": models}
	if note != "" {
		out["note"] = note
	}
	s.ok(w, out)
}

// putProviders applies exactly one mutation per request: set_provider
// (blank token keeps the stored one, for the same base_url only),
// delete_provider, bind, or unbind.
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
	if reg.LoadErr != nil {
		// Saving would replace the operator's whole registry with this
		// one mutation on top of an empty one.
		s.fail(w, http.StatusConflict, fmt.Errorf("provider registry could not be read (%v) — fix the file by hand before saving", reg.LoadErr))
		return
	}
	note := ""
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
		if p.BaseURL != "" {
			if err := checkProviderURL(r.Context(), p.BaseURL); err != nil {
				s.log.Warn("admin provider set refused", "name", p.Name, "base_url", p.BaseURL, "why", err)
				s.fail(w, http.StatusBadRequest, err)
				return
			}
		}
		prev, existed := reg.Providers[p.Name]
		token, tokenState := p.Token, "new"
		switch {
		case p.Token == "" && prev.Token != "" && !sameEndpoint(prev.BaseURL, p.BaseURL):
			// The stored token was issued for the old endpoint; reusing it
			// would hand it to whatever the new base_url names.
			s.log.Warn("admin provider set refused", "name", p.Name, "base_url", p.BaseURL,
				"old_base_url", prev.BaseURL, "why", "base_url changed without a token")
			s.fail(w, http.StatusConflict, fmt.Errorf("base_url changed from %q: send the token again (the stored token is not reused for a new endpoint)", prev.BaseURL))
			return
		case p.Token == "" && prev.Token == "":
			tokenState = "none"
		case p.Token == "": // blank on save means: keep what's stored
			token, tokenState = prev.Token, "kept"
		case prev.Token != "":
			tokenState = "replaced"
		}
		reg.Providers[p.Name] = provider.Provider{Kind: p.Kind, BaseURL: p.BaseURL, Token: token}
		s.log.Warn("admin provider set", "name", p.Name, "kind", p.Kind, "base_url", p.BaseURL,
			"old_base_url", prev.BaseURL, "existed", existed, "token", tokenState)
		if p.Kind == "openai" && !localEndpoint(p.BaseURL) {
			note = dataExposureNote
		}
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
	s.providersView(w, note)
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
	// One lookup yields the endpoint OR the reason — a test button that
	// cannot name the misconfiguration it hit is the same lie as one
	// that quietly succeeds elsewhere.
	ep, why := provider.Lookup(s.reg.Root(), req.Model, "")
	if why != "" {
		s.log.Warn("provider test unresolved", "model", req.Model, "why", why)
		s.fail(w, http.StatusBadRequest, fmt.Errorf("%s", why))
		return
	}
	afterProbeResolve()
	start := time.Now()
	var tokens int64
	var err error
	if ep.Kind == "claude" {
		workDir := filepath.Join(s.reg.Root(), "curator-workdir")
		os.MkdirAll(workDir, 0o700)
		// A human-triggered one-word probe: short bound, and NoPace so a
		// diagnostic doesn't queue up to the full penalty behind the
		// batch pacer during the very outage it is diagnosing.
		ex := &curate.ClaudeExtractor{Model: ep.Model, WorkDir: workDir,
			Timeout: 30 * time.Second, NoPace: true}
		var u curate.Usage
		_, u, err = ex.Complete("Reply with the single word: ok")
		tokens = u.InputTokens + u.OutputTokens
	} else {
		tokens, err = probeOpenAI(probeClient(ep), ep, req.Op)
	}
	if err != nil {
		// Same shape as the success line below: a failure nobody can
		// attribute to a model is nearly useless in the Log tab. The
		// elapsed time rides in the message too — a 15s timeout and a
		// 100ms rejection call for different fixes.
		ms := time.Since(start).Milliseconds()
		s.log.Warn("provider test failed", "model", req.Model, "op", req.Op, "ms", ms, "err", err)
		s.fail(w, http.StatusBadGateway, fmt.Errorf("%v (after %dms)", err, ms))
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
	if p.Token == "" {
		// Upstream would answer 401 — name the actual cause instead. The
		// registry has never served a tokenless endpoint (bindings to
		// one do not resolve), so this is the same rule, said early.
		s.fail(w, http.StatusBadRequest, fmt.Errorf("provider %q has no token stored — save it with its token first (any non-empty value for an endpoint that needs none)", name))
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
	resp, err := providerClient().Do(hreq)
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

// afterProbeResolve lets a test change the registry between resolution
// and the probe, the window a concurrent admin request can hit.
var afterProbeResolve = func() {}

// probeClient guards every registry endpoint. The AIMEM_OPENAI_* pair
// is the host operator's own setting, which the admin API cannot change.
// The choice rests on ep alone, resolved in one registry read: a second
// read could see a concurrent unbind and mistake a registry endpoint for
// the environment's.
func probeClient(ep provider.Endpoint) *http.Client {
	if ep.FromEnv {
		return &http.Client{Timeout: 15 * time.Second}
	}
	return providerClient()
}

// probeOpenAI is the HTTP half of the test button: one tiny embeddings or
// chat request with a hard 15s bound so a dead endpoint fails fast.
func probeOpenAI(client *http.Client, ep provider.Endpoint, op string) (int64, error) {
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
	resp, err := client.Do(hreq)
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
