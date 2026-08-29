// Package provider resolves which endpoint serves a model. A host-local
// registry (<state-root>/providers.json, mode 0600, never synced — tokens
// are host secrets) maps model names to named providers; the legacy
// AIMEM_OPENAI_* env pair remains the fallback so hosts without the file
// keep today's behavior unchanged (docs/DESIGN-multiprovider.md).
package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// DefaultBaseURL is where an "openai" provider points when it names no
// URL of its own, and what the AIMEM_OPENAI_* env fallback assumes.
// Override it per provider (base_url) or with AIMEM_OPENAI_BASE_URL to
// reach a compatible proxy - LiteLLM, vLLM, Ollama, a vendor gateway -
// instead of OpenAI itself.
const DefaultBaseURL = "https://api.openai.com/v1"

// Provider is one named endpoint.
type Provider struct {
	Kind    string `json:"kind"`               // "openai" (compat HTTP) | "claude" (headless CLI)
	BaseURL string `json:"base_url,omitempty"` // openai kind; empty = DefaultBaseURL
	Token   string `json:"token,omitempty"`    // openai kind; never leaves this host
}

// Binding maps a local model name to a provider. Model is the upstream
// name sent in API payloads; empty means the local name IS the upstream
// name. Distinct local aliases let the same upstream model ride several
// providers (e.g. gpt4o-cw and gpt4o-oa both -> "gpt-4o").
type Binding struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

// Registry is the on-disk shape of providers.json.
type Registry struct {
	Providers map[string]Provider `json:"providers"`
	Models    map[string]Binding  `json:"models"`
}

// Path returns the registry location for a state root.
func Path(root string) string { return filepath.Join(root, "providers.json") }

// Load reads the registry; a missing or unreadable file yields an empty
// registry (fail-open, like unset env), never an error — resolution then
// falls through to env.
func Load(root string) *Registry {
	r := &Registry{}
	if raw, err := os.ReadFile(Path(root)); err == nil {
		_ = json.Unmarshal(raw, r)
	}
	if r.Providers == nil {
		r.Providers = map[string]Provider{}
	}
	if r.Models == nil {
		r.Models = map[string]Binding{}
	}
	return r
}

// Save writes the registry atomically at mode 0600.
func (r *Registry) Save(root string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(root) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(root))
}

// Bind maps a local model name to a named provider; upstream is the
// payload model name ("" = same as the local name).
func (r *Registry) Bind(model, providerName, upstream string) {
	if upstream == model {
		upstream = ""
	}
	r.Models[model] = Binding{Provider: providerName, Model: upstream}
}

// Endpoint is a resolved model → endpoint mapping. Model is the name to
// put in API payloads (the binding's upstream name, or the requested
// name itself).
type Endpoint struct {
	Kind    string
	BaseURL string
	Token   string
	Model   string
}

// Resolve returns the endpoint serving model: its registry binding when
// one names a usable provider, else the env pair. ok=false means no
// endpoint is configured anywhere (callers treat that as "LLM off",
// matching the old missing-env contract).
func Resolve(root, model string) (Endpoint, bool) {
	if ep, bound := ResolveBound(root, model); bound {
		return ep, true
	}
	if key := os.Getenv("AIMEM_OPENAI_API_KEY"); key != "" {
		base := os.Getenv("AIMEM_OPENAI_BASE_URL")
		if base == "" {
			base = DefaultBaseURL
		}
		return Endpoint{Kind: "openai", BaseURL: base, Token: key, Model: model}, true
	}
	return Endpoint{}, false
}

// ResolveBound resolves only through an explicit registry binding — no
// env fallback. Callers that let the binding's kind PICK the backend
// (curate: claude CLI vs openai HTTP) must use this: the env pair may
// complete an endpoint, but it must never flip a backend choice.
func ResolveBound(root, model string) (Endpoint, bool) {
	r := Load(root)
	b, ok := r.Models[model]
	if !ok {
		return Endpoint{}, false
	}
	upstream := b.Model
	if upstream == "" {
		upstream = model
	}
	p, ok := r.Providers[b.Provider]
	if !ok {
		return Endpoint{}, false
	}
	if p.Kind == "claude" {
		return Endpoint{Kind: "claude", Model: upstream}, true
	}
	if p.Token == "" {
		return Endpoint{}, false
	}
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return Endpoint{Kind: "openai", BaseURL: base, Token: p.Token, Model: upstream}, true
}
