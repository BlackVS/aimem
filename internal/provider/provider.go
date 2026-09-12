// Package provider resolves which endpoint serves a model. A host-local
// registry (<state-root>/providers.json, mode 0600, never synced — tokens
// are host secrets) maps model names to named providers. The legacy
// AIMEM_OPENAI_* env pair serves models that have NO binding, so hosts
// without the file keep the pre-registry behavior; a model that IS bound
// resolves only through its binding (docs/DESIGN-multiprovider.md,
// "Correction 2026-09-12").
package provider

import (
	"encoding/json"
	"fmt"
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
	// LoadErr is set when the file exists but does not parse. A corrupt
	// registry must fail CLOSED: treating it as empty would make every
	// bound model "unbound" and quietly re-enable the env routing the
	// bindings exist to override — and a Save over it would erase the
	// operator's providers.
	LoadErr error `json:"-"`
}

// Path returns the registry location for a state root.
func Path(root string) string { return filepath.Join(root, "providers.json") }

// Load reads the registry; a missing file yields an empty registry
// (fail-open, like unset env — every model is unbound and resolution
// goes to env). A present-but-unparseable file is recorded in LoadErr
// and resolves nothing.
func Load(root string) *Registry {
	r := &Registry{}
	if raw, err := os.ReadFile(Path(root)); err == nil {
		if uerr := json.Unmarshal(raw, r); uerr != nil {
			r = &Registry{LoadErr: fmt.Errorf("%s: %w", Path(root), uerr)}
		}
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

// bound classifies a model's registry binding: the endpoint it names,
// or the reason it cannot serve. isBound=false means the model has no
// binding at all (the env pair's territory). ONE decision tree, so the
// verdict and its explanation cannot drift apart.
func (r *Registry) bound(model string) (ep Endpoint, isBound bool, reason string) {
	b, ok := r.Models[model]
	if !ok {
		return Endpoint{}, false, ""
	}
	upstream := b.Model
	if upstream == "" {
		upstream = model
	}
	p, ok := r.Providers[b.Provider]
	switch {
	case !ok:
		return Endpoint{}, true, fmt.Sprintf("model %q is bound to provider %q, which no longer exists", model, b.Provider)
	case p.Kind == "claude":
		return Endpoint{Kind: "claude", Model: upstream}, true, ""
	case p.Token == "":
		// The registry has never served a tokenless endpoint; auth-free
		// proxies take any non-empty placeholder.
		return Endpoint{}, true, fmt.Sprintf("model %q is bound to provider %q, which has no token stored — save the provider with its token (any non-empty value for an endpoint that needs none)", model, b.Provider)
	}
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return Endpoint{Kind: "openai", BaseURL: base, Token: p.Token, Model: upstream}, true, ""
}

// Lookup is the one resolution verdict for a model: its endpoint, or a
// reason (in words an operator can act on) why none serves it. A bound
// model resolves only through its binding — an explicit binding is the
// operator's statement of where the model lives, and quietly serving it
// from the env endpoint made a misconfigured provider look healthy while
// an unrelated service answered in its place. Unbound models use the
// AIMEM_OPENAI_* env pair. wantKind, when set, also rejects an endpoint
// of the wrong kind (a claude binding where an OpenAI-compatible HTTP
// endpoint is required), with the reason saying so.
func Lookup(root, model, wantKind string) (Endpoint, string) {
	r := Load(root)
	if r.LoadErr != nil {
		return Endpoint{}, fmt.Sprintf("provider registry could not be parsed (%v) — fix the file by hand; no model resolves until it does", r.LoadErr)
	}
	ep, isBound, reason := r.bound(model)
	if !isBound {
		key := os.Getenv("AIMEM_OPENAI_API_KEY")
		if key == "" {
			return Endpoint{}, fmt.Sprintf("model %q has no binding and AIMEM_OPENAI_API_KEY is unset", model)
		}
		base := os.Getenv("AIMEM_OPENAI_BASE_URL")
		if base == "" {
			base = DefaultBaseURL
		}
		ep = Endpoint{Kind: "openai", BaseURL: base, Token: key, Model: model}
	}
	if reason != "" {
		return Endpoint{}, reason
	}
	if wantKind != "" && ep.Kind != wantKind {
		return Endpoint{}, fmt.Sprintf("model %q resolves to a %s endpoint, but a %s one is required here", model, ep.Kind, wantKind)
	}
	return ep, ""
}

// Resolve is Lookup without a kind requirement; ok=false means no
// endpoint serves the model (callers treat that as "LLM off").
func Resolve(root, model string) (Endpoint, bool) {
	ep, reason := Lookup(root, model, "")
	return ep, reason == ""
}

// Explain is the reason half of Lookup: "" when the model resolves.
func Explain(root, model, wantKind string) string {
	_, reason := Lookup(root, model, wantKind)
	return reason
}

// ResolveBound resolves only through an explicit registry binding — no
// env fallback. Callers that let the binding's kind PICK the backend
// (curate: claude CLI vs openai HTTP) must use this: the env pair may
// complete an endpoint, but it must never flip a backend choice.
func ResolveBound(root, model string) (Endpoint, bool) {
	r := Load(root)
	if r.LoadErr != nil {
		return Endpoint{}, false
	}
	ep, isBound, reason := r.bound(model)
	return ep, isBound && reason == ""
}
