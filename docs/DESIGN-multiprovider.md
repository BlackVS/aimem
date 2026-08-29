# Design — Per-model providers (multiprovider), GUI-managed

Status: IMPLEMENTED 2026-08-28 (same session; see git log). Registry +
resolution in `internal/provider`, embed/curate wired through it,
admin endpoints at `/v1/config/providers` (+`/test`) — the design's
`/v1/admin/*` naming was corrected to the existing `/v1/config/*`
convention — GUI cards in the usage tab. Same-day additions from live
use: explicit "test chat"/"test embed" buttons (the single button
guessed the probe kind and guessed wrong for an embed model bound on
the gemini hub); model ALIASES — a binding is local-name → {provider,
upstream model}, so the same upstream model can ride several providers
under distinct local names (payloads and stored vectors use the
upstream name, run history keeps the alias); and a GUI dropdown of
real model ids proxied server-side from the provider's /v1/models
(OpenAI, Google's compat endpoint, and LiteLLM all serve it; the
claude kind returns its CLI aliases statically). Deferred: per-model
prices, `role` markers (open questions below stay open). Author
Written 2026-08-28. Companion to DESIGN.md.

## Problem

A hub can talk to exactly ONE OpenAI-compatible endpoint: the
`AIMEM_OPENAI_BASE_URL` / `AIMEM_OPENAI_API_KEY` pair serves both the
curate backend (`main.go:728-739`) and embeddings (`embed.go:32-39`).
The home hub already strains this: gemini embeddings ride Google's
OpenAI-compat endpoint while curation uses the claude CLI — that only
works because the claude backend needs no OpenAI env. The moment one
hub wants, say, LiteLLM-proxied curation AND Google embeddings, or two
models behind different proxies, the single env pair cannot express it.

## Decision sketch

A host-local provider registry, resolved per model name, editable in
the admin GUI (user request: "for each used model in hub set full
data — url, token etc; set via web gui").

### Storage: `<state-root>/providers.json`, mode 0600

Host-local like `hub.json`, NEVER synced (tokens are host secrets; the
existing posture — key files outside the repo, env file 0600 — stays).

```json
{
  "providers": {
    "proxy":  {"kind": "openai", "base_url": "https://llm.example.com/v1", "token": "sk-..."},
    "google": {"kind": "openai", "base_url": "https://generativelanguage.googleapis.com/v1beta/openai", "token": "..."},
    "claude": {"kind": "claude"}
  },
  "models": {
    "my-model":             {"provider": "proxy"},
    "gemini-embedding-001": {"provider": "google"},
    "haiku":                {"provider": "claude"}
  }
}
```

- `providers` — named endpoints. `kind: openai` needs `base_url` +
  `token`; `kind: claude` has neither (headless CLI, subscription auth).
- `models` — model name → provider binding. Optional per-model fields
  later: `price_in` / `price_out` (USD per 1M tokens), replacing the
  global `AIMEM_PRICE_IN/OUT` for mixed-model budgeting.

### Resolution order (full back-compat)

1. Model listed in `providers.json` → use its provider's endpoint.
2. Model unlisted / file absent → today's env vars, unchanged.

So existing hubs keep working with zero migration; the file is opt-in.
`embed.FromEnv` grows into `embed.ForModel(name)`; the curate openai
backend resolves the same way. The curate `kind` field also subsumes
`AIMEM_CURATE_BACKEND`: the backend is a property of the model binding,
not a host global.

### Admin GUI (usage tab, extending the existing "models" card)

- **Providers card**: list rows (name, kind, base_url, token shown as
  `••••last4`); add / edit / delete. Token input is write-only — the
  GET endpoint returns masked values ONLY; a blank token on save means
  "keep existing".
- **Model bindings card**: model name → provider dropdown. The current
  curate/embed model inputs stay; a bound model shows its provider.
- **Test button per model** (user request): each binding row gets
  "test" — the hub makes one minimal live call through the resolved
  provider and the row shows the result inline (ok + latency + tokens
  used, or the provider's error verbatim). What "minimal" means per
  kind: embed models embed one short fixed string; chat models request
  a 1-token completion; `kind: claude` runs the CLI with a trivial
  prompt. The call happens SERVER-side with the stored token (nothing
  secret reaches the browser); spend is a handful of tokens and is
  metered into run history like any other usage (model attribution
  included) so even test spend stays on the books. A test never
  writes facts or embeddings — it throws the response away.
- Endpoints under the existing admin namespace:
  `GET /v1/admin/providers` (masked), `PUT /v1/admin/providers`
  (full replace of one provider or binding, atomic tmp+rename write
  like `saveModels` at `admin.go:164-171`), and
  `POST /v1/admin/providers/test {"model": "..."}` (the live probe;
  bounded timeout ~15s so a dead endpoint fails fast). Audit log line
  on every change — provider name and base_url only, never the token.

### Security invariants

- Tokens never leave the hub host: masked in GET, absent from logs,
  absent from run history, file 0600, excluded from `sync` (which is
  event-level and never touched config files anyway).
- The GUI already sits behind hub bearer auth + TLS; no new surface.
- Validation mirrors `saveModels`: reject names/URLs with quotes,
  newlines, `$`, backticks.

## Non-goals

- No per-project provider routing (egress policy stays the existing
  LiteLLM-vs-direct decision; this registry only maps model → endpoint).
- No secret sync or vault integration; one hub = one registry file.
- No provider health checks in v1 (the first failing run says enough).

## Build order

1. Registry file + resolution in embed/curate paths (env fallback).
2. Admin endpoints (masked GET, atomic PUT).
3. GUI cards.
4. Later: per-model prices feeding the budget projector.

## Validation plan

Both target providers already exist with credentials in hand: the
a LiteLLM proxy and the Gemini free tier
(Google's OpenAI-compatible endpoint). Acceptance =
one hub running curation via the proxy and embeddings via Google FROM THE
REGISTRY (env pair unset), plus the env-fallback path proven by
deleting `providers.json` and seeing today's behavior return. A direct
api.openai.com key also exists (free tier, created 2026-08-28 for
these tests; the user can fund it later if needed) — a third,
non-proxy provider proving three simultaneous endpoints, and once
funded, the first real-money test of the budget gate. Like every
provider token it goes straight into the hub's `providers.json`
(0600), never the repo.

## Open questions

- Should `AIMEM_CURATE_MODEL` / `AIMEM_EMBED_MODEL` (which model to
  use) also move into the registry as role markers (`"role":
  "curate"|"embed"`), leaving env purely as fallback? Leaning yes —
  then the GUI's model inputs and provider bindings collapse into one
  table.
- Per-model prices here vs. LiteLLM's own cost reporting: keep both
  (registry price = worst-case for the budget gate, response cost =
  actual)?
