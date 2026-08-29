# aimem — Admin Manual

How to set up and operate aimem: workstations, the hub, curation,
embeddings, and releases. For daily usage see `USER-MANUAL.md`;
for architecture see `DESIGN.md`.

## 1. Install on a workstation (user install)

One-liner in any project directory (installs the binary if missing, then
wires the project):

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh | bash
```

Windows (PowerShell; supported, tested less than Linux):

```powershell
irm https://raw.githubusercontent.com/BlackVS/aimem/master/boot.ps1 | iex
```

Environment knobs for the boot script:

- `AIMEM_VERSION=vX.Y.Z` — pin a release instead of taking the latest.
- `AIMEM_REPO=owner/name` — install from a fork.
- `AIMEM_HUB_URL` / `AIMEM_HUB_TOKEN` — configure hub push at install time.
- `AIMEM_REINSTALL=1` — force reinstall of the user-level binary.
- `AIMEM_PREBUILT` — path to a prebuilt binary (skips download).
- `AIMEM_GROUPS=oboro,ai-infra` — pre-declare shared knowledge groups in
  the generated `.aimem.json` (default: `{"groups":[]}` = isolated;
  an existing `.aimem.json` is never overwritten).

What a user install does: binary → `~/.local/bin/aimem` (Windows:
`%LOCALAPPDATA%\aimem\bin`), systemd user unit `aimem.service` with
`EnvironmentFile=-%h/.config/aimem/env` (Windows: logon task via
`conhost --headless`), user-level Claude Code hooks
(Stop/StopFailure/PreCompact → `aimem submit-claude`), global OpenCode
plugin. The installer **restarts** the service (enable --now alone keeps a
stale binary running).

What a project install does (run boot.sh/install.sh in the project dir):
`docs/SESSION-STATE.md` template, `.claude/settings.json` with SessionStart
hook `aimem session-start`, MCP registration in `.mcp.json` and
`opencode.json`, handoff protocol stubs in `AGENTS.md`/`CLAUDE.md`.

Important: checkpoint hooks live at **user level only**. Never add
Stop/StopFailure/PreCompact hooks to a project's `.claude/settings.json` —
duplicate registration produces duplicate journal events.

## 2. Hub setup

The hub is the same binary run as a server. Throughout this manual
`hub.example.com` stands for your own host; a deployment may run several
physically separate hubs (see 3d) so that, say, work and personal
projects never share a server.

Fresh host (Debian/Ubuntu LXC or VM), as root — one command does user,
binary, env, units, timers:

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/install-hub.sh | bash
```

Knobs (env): `AIMEM_HUB_USER` (default sessiond), `AIMEM_HTTP_LISTEN`
(:8440), `AIMEM_HTTP_TOKEN` (generated if unset — printed at the end),
`AIMEM_DOMAIN=<fqdn>` (generates a **self-signed** cert for that name
into `~/.config/aimem/tls/` and serves TLS — clients connect with
`aimem hub add ... --insecure` until you install a real certificate;
then drop the flag),
`AIMEM_TLS_CERT`/`AIMEM_TLS_KEY` (explicit cert paths; nothing set =
plain HTTP), `AIMEM_OPENAI_API_KEY` + `AIMEM_OPENAI_BASE_URL` + models
(enables the curation timer's LLM work). Idempotent: re-run to upgrade
the binary. Everything stateful lives outside the binary — config
`~/.config/aimem/env`, certs `~/.config/aimem/tls/`, data
`~/.local/state/aimem/` — so an LXC backup captures the whole hub.

### What a hub serves

| Path | Auth | Contents |
|---|---|---|
| `/` | none | Status card: liveness, build, hostname, uptime. Nothing about what the hub holds. |
| `/v1/status` | none | The JSON behind that card. |
| `/admin` | none to load | The console shell. Holds no data; it asks for the hub token once (browser localStorage) and calls the API with it. |
| everything else | `Authorization: Bearer $AIMEM_HTTP_TOKEN` | The whole API — projects, memories, usage, logs, config. |

Those first three are the complete unauthenticated surface. Adding a
field to `/v1/status` is a disclosure decision: hubs listen on routable
names, so anything served there is served to the internet.

The port is 8440, not 443, because the hub runs as a **systemd `--user`**
service under an unprivileged account, and ports below 1024 need
`CAP_NET_BIND_SERVICE`. Moving to 443 means a root-owned socket unit, a
system unit with `AmbientCapabilities=CAP_NET_BIND_SERVICE`, or a
reverse proxy — not a change to `AIMEM_HTTP_LISTEN` alone. (`setcap` on
the binary would work until the next upgrade replaces it.)

Manual equivalent (what the script sets up):

1. Install binary (same install.sh, or copy the release binary).
2. `~/.config/aimem/env` (referenced by the unit's `EnvironmentFile`):

   ```sh
   AIMEM_HTTP_LISTEN=:8440
   AIMEM_HTTP_TOKEN=<bearer token>
   AIMEM_TLS_CERT=/path/fullchain.pem
   AIMEM_TLS_KEY=/path/privkey.pem
   # curation + embeddings (optional, enables LLM egress):
   AIMEM_OPENAI_API_KEY=<api key>
   AIMEM_OPENAI_BASE_URL=https://llm.example.com/v1   # omit for api.openai.com
   AIMEM_CURATE_MODEL=gpt-4o-mini
   AIMEM_EMBED_MODEL="text-embedding-3-large"
   ```

   Any OpenAI-compatible endpoint works here: the vendor API, a LiteLLM
   or vLLM proxy, Ollama. Values containing spaces MUST be quoted (the
   file is also shell-sourced by scripts). Some newer model families
   reject a `temperature` field — the client already omits it; don't add
   one.
3. TLS certificate. `AIMEM_DOMAIN` generates a self-signed one to get
   started; replace it with a real certificate by pointing
   `AIMEM_TLS_CERT`/`AIMEM_TLS_KEY` at the files and restarting. Prefer
   delivering renewed certificates onto the hub from elsewhere over
   putting ACME or DNS-provider credentials on it — the hub holds every
   project's memory and should hold as few other secrets as possible.
4. Client-side secrets: keep the bearer token in a 0600 file, never
   committed. If you enable ssh sync, its key is authorized for the
   service user on the hub.

### Named tokens (per-machine credentials)

One shared bearer token works, but named tokens are better the moment a
second machine connects: each is revocable alone, carries a role, and
stamps shared-document writes with its name (so "who overwrote my
handoff" is answered by authentication). On the hub host:

```sh
aimem token add dmbunker              # role writer (default); secret printed ONCE
aimem token add ops --role admin
aimem token list                      # names and roles — no secrets exist to show
aimem token rm dmbunker               # revoke = delete one line
```

A **writer** token covers what a workstation needs: events, sync,
recall, shared documents, MCP. **admin** adds config, providers,
rename, drop, retention, chapter tools, and logs. Secrets are stored as
sha256 digests in `<state-root>/tokens.json` (0600, host-local, never
synced); the `AIMEM_HTTP_TOKEN` env token keeps working as an implicit
admin. No restart needed — the registry is read per request. The full
route-by-route contract is served at `GET /v1/openapi.json` (bearer)
and shipped in the repo (`internal/server/openapi.json`).

### Hub timers

- `aimem-curate.timer` — hourly at :15 (+ <=5min jitter; drop-in
  `aimem-curate.timer.d/override.conf`) + `OnStartupSec=5min` +
  `Persistent=true`, runs `curate-all.sh`:
  `aimem curate --backend openai --all` then `aimem embed --all`.
  Hourly is cheap: projects with no new events cost zero LLM calls,
  re-extractions reassert, and semantic dedup (v0.1.33; threshold
  `AIMEM_DEDUP_SIM`, default 0.90, 0 disables) folds rephrased twins
  onto the existing fact instead of inserting duplicates. The 04:xx UTC
  run additionally sweeps pre-existing twins with `aimem dedup --all`
  (v0.1.35; retroactive fold, audited, pure local vector math).
  The hub VM is often powered off overnight, so the boot trigger and
  persistent catch-up guarantee maintenance still happens whenever the
  hub comes up (the service user has linger enabled, so its timers start
  at boot). If the model endpoint is on an internal network, the hub
  needs a route to it.
- Check runs: `journalctl --user -u aimem-curate.service`; per-project
  reports include token usage. Since v0.1.85 the headless `claude`
  backend counts its cached prompt tokens too (they are most of the
  run), so token totals are comparable across backends — with one
  caveat: cache-read tokens are much cheaper per token, so raw counts
  slightly overstate the claude backend's relative cost (the safe
  direction for the budget brake). For paid APIs the provider's own
  spend page remains the authoritative ledger — `aimem budget` is a
  brake, not an accountant.

## 3. Connect a workstation to the hub

```sh
aimem hub https://hub.example.com:8440 "$(cat ~/.config/aimem/hub_token)"
```

Real-time push is then automatic on every checkpoint (hub outage spools
locally, flushes on next contact). Anti-entropy sync — the leg that
also PULLS curated knowledge down to this machine — runs periodically
(`aimem sync --all-hubs`; Linux `aimem-sync.timer`, Windows the
`aimem-sync` scheduled task) **over the hub's HTTPS API** with the same
token: no ssh account, no keys. A hub that predates the `/v1/sync`
routes falls back to its configured ssh destination with a warning —
upgrade it and the ssh key can be retired.

### 3d. Multiple hubs (separate work/home data)

One machine can push different projects to different hubs so their data
lives on physically separate servers. Hubs are named in
`<state-root>/hub.json`; the legacy single-hub config keeps working as a
hub named `default`.

```sh
aimem hub add work https://hub.example.com:8440  <token> --default
aimem hub add home https://hub2.example.com:8440 <token>
aimem hub              # list (default marked *)
aimem hub default home # change default
aimem hub rm home
```

(`--sync <ssh-dest>` on `hub add` is only needed for a hub old enough
to lack the sync API.)

A project binds to a hub in its `.aimem.json`:
`{"project":"budget","hub":"home","groups":[...]}` — unbound projects
use the default hub. The binding rides on every event push (like
groups) and is stored as project meta, which is what partitions sync:

- **Real-time push** routes each checkpoint to its project's hub, with
  a per-hub spool (one hub down never blocks or misroutes another).
- **`aimem sync --hub <name>`** exchanges only that hub's bound
  projects (plus the `user` DB and the groups those projects declare)
  with that hub, both directions carrying the filter. `--all-hubs`
  loops every configured hub — that is what the timer/task runs. The
  plain `aimem sync <dest>` form refuses to run when several hubs are
  configured (it would cross-pollinate them).
- Group config edits (`aimem group`) push to every hub that has a
  member project of that group.

Hub names are machine-local config, but use the same names everywhere —
the binding stored in `.aimem.json` travels with the repo. The TUI Hub
tab shows every configured hub with its own gauges.

## 3b. Custom-provider model limits (OpenCode)

OpenCode does not read `max_input_tokens` from an OpenAI-compatible
`/v1/models`; custom-provider models default to context limit 0 (unknown).
Declare limits explicitly in `~/.config/opencode/opencode.jsonc` so both
OpenCode's auto-compaction and the aimem 80% context warning have the
right denominator:

```jsonc
"provider": { "myproxy": { "models": {
  "my-model": { "name": "My Model",
                "limit": { "context": 128000, "output": 16000 } }
} } }
```

The values come from the proxy itself (`GET /v1/models` →
`max_input_tokens`/`max_output_tokens`). The aimem plugin warns only when
the model's limit is known (or `AIMEM_CTX_LIMIT` is set) — unknown limits
produce no warning rather than a wrong one. With a known limit, three
knobs. Put them in `~/.config/aimem/env` — the plugin folds that file
exactly like the CLI does (process env wins), so the machine's one aimem
config file tunes everything; restart OpenCode to apply:

- `AIMEM_CTX_WARN_FRACTION` (default 0.8) — first warning threshold;
  warnings then repeat every 5%, so one missed toast is not the last
  word before the hard context error.
- `AIMEM_AUTO_COMPACT=<fraction>` (e.g. 0.9; unset = off) — trigger
  compaction automatically at that fill, while the summarizer still has
  room to run. The journal, compaction marker, and handoff injection
  make the "prepare" half automatic; this closes the loop for long
  unattended tasks.
- `AIMEM_CTX_LIMIT` — hard override of the model's context limit.

A project can override the host in its `.aimem.json` —
`{"auto_compact": 0.3, "ctx_warn_fraction": 0.25}` — for models that
degrade mid-context ("lost in the middle") long before the window
fills. Precedence: process env > project > env file > default; values
above 1 read as percent (30 == 0.3).

## 3c. Curation budgets (paid-API brake)

`aimem budget` caps curation spend per window; enforcement is pre-spend
(usage + worst-case projection of the next run must fit the cap), so a
cap can never be overrun — runs are refused with "budget exhausted"
(`curate --force` bypasses deliberately).

```sh
aimem budget --daily 500k --monthly '$10'   # combined tokens or USD
aimem budget --daily 'in:2M,out:300k'         # separate in/out caps (prices differ)
aimem budget                                # show usage vs limits
aimem budget --reset                        # restart windows (history kept)
aimem budget --unlimited                    # remove
aimem budget -p <project> --daily 100k      # per-project override
```

USD caps need prices (USD per 1M tokens) in the env:
`AIMEM_PRICE_IN` / `AIMEM_PRICE_OUT` — without them a USD cap refuses to
run at all (never spend unpriced); unpriced recorded runs are charged at
these prices when counting usage. The openai backend also records
LiteLLM's `x-litellm-response-cost` header when present. NOTE: budget
config is machine-local (meta does not sync) — set it on the hub too,
where the nightly curation actually runs. The TUI AI tab shows global
budget windows with a warning at 80%.

## 4. Embeddings on a workstation (optional)

Add to `~/.config/aimem/env` (then `systemctl --user restart aimem`):

```sh
AIMEM_OPENAI_API_KEY=<key>
AIMEM_EMBED_MODEL="Text Embedding 3 Large"
```

Backfill: `aimem embed --all`. The CLI folds `~/.config/aimem/env` into
its own environment for any `AIMEM_*` variable not already set (process
env wins), so interactive `aimem embed`/`curate`/`tui` see the same
config as the systemd units — no manual `export` needed.

Embeddings are machine-local derived data
and are not synced — but `aimem sync` runs an embed backfill
automatically after each successful memory sync (when the env above is
configured), and the hub does it nightly, so no manual step remains.
Embed spend is metered into the run history and budget-gated.
Unset env = BM25-only recall — everything still works.

## 5. Releases

Releases are cut by pushing a tag:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

`.github/workflows/release.yml` runs the tests, cross-builds static
binaries (CGO_ENABLED=0) for linux/darwin amd64+arm64 and windows-amd64
with `SHA256SUMS`, and publishes them as release assets. `boot.sh` and
`boot.ps1` always fetch the latest release. `.github/workflows/ci.yml`
runs vet and tests on Linux, Windows and macOS for every push and pull
request.

## 6. Troubleshooting

| Symptom | Check |
|---|---|
| No checkpoints appearing | `systemctl --user status aimem`; hooks in `~/.claude/settings.json`; `aimem health` |
| Hub push failing | token file perms; `curl -H "Authorization: Bearer $T" https://hub.example.com:8440/v1/health`; spool flushes automatically |
| Duplicate journal events | hooks registered at both user and project level — remove project-level ones |
| Curation produces nothing | hub → LiteLLM route (HTTP 200 from the hub?); key valid; cursor already past events |
| `... command not found` sourcing env | unquoted value with spaces in `~/.config/aimem/env` |
| Recall misses obvious semantic matches | embeddings backfilled for that DB? server restarted after env change? |
| A dropped project keeps reappearing on a hub | Some machine still holds a LOCAL database for that id. Memory sync is filtered by hub binding, so every sync re-pushes it and re-creates the project. Fix it at the source, in this order: drop it on BOTH hubs first, then `aimem drop-project -p <id> --yes` on the machine, then sync. Dropping locally while a hub still has it just lets the next pull restore it. |
| ...and it reappears on the *other* hub than you expected | The local copy's binding decides the destination: `meta hub=<name>` sends it there, while an UNBOUND copy goes to that machine's default hub. Two stale copies with different bindings resurrect on two different hubs. Check with `aimem meta -p <id> hub`. |
| Binary update doesn't take effect | service restart required; copy over a running binary via `cp new .new && mv` (text-busy) |
| Context toast shows absurd % (e.g. "296% of 200000") | OpenCode reports limit 0 for custom-provider models (it ignores `/v1/models` `max_input_tokens`) — declare `limit:{context,output}` per model in opencode.jsonc (section 3b); plugins from v0.1.4 stay silent when the limit is unknown |
| No 80% context warning at all | model limit unknown (see above) and `AIMEM_CTX_LIMIT` unset — declaring the limit re-enables the warning |

## 7. Data locations

- State root: `~/.local/state/aimem/` (per-project `*.db`, `sync/` cursors,
  `spool/`).
- `aimem state-root` prints it; `aimem project-id .` prints a project's ID.
- Backups: the hub holds a merged copy of all journals and memories;
  any machine can re-seed another via `sync`.
