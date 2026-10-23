# aimem — Admin Manual

How to set up and operate aimem: workstations, the hub, curation,
embeddings, and releases. For daily usage see `USER-MANUAL.md`;
for architecture see `DESIGN.md`.

## 1. Install on a workstation (user install)

One-liner in any project directory (installs the binary if missing, then
wires the project):

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.13/boot.sh | bash
```

Windows (supported, tested less than Linux), in a PowerShell window
that is already open:

```powershell
irm https://raw.githubusercontent.com/BlackVS/aimem/v0.10.13/boot.ps1 | iex
```

From cmd, or from the Run dialog (Win+R) for an upgrade:

```bat
powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aimem/v0.10.13/boot.ps1 | iex"
```

Environment knobs for the boot script:

- `AIMEM_VERSION=vX.Y.Z` — install another release than the one the
  one-liner names (the boot script still comes from the URL's tag).
- `AIMEM_UPGRADE_WAIT=30` — seconds an upgrade waits for health at the new
  version before it rolls back.
- `AIMEM_REPO=owner/name` — install from a fork.
- `AIMEM_HUB_URL` / `AIMEM_HUB_TOKEN` — configure hub push at install time.
- `AIMEM_REINSTALL=1` — force reinstall of the user-level binary. Not needed
  for upgrades: an installed aimem older than the release is replaced anyway.
- `AIMEM_PREBUILT` — path to a prebuilt binary (skips download).
- `AIMEM_GROUPS=oboro,ai-infra` — pre-declare shared knowledge groups in
  the generated `.aimem.json` (default: `{"groups":[]}` = isolated;
  an existing `.aimem.json` is never overwritten).

What a user install does: binary → `~/.local/bin/aimem` (Windows:
`%LOCALAPPDATA%\aimem\bin`), systemd user unit `aimem.service` with
`EnvironmentFile=-%h/.config/aimem/env` (Windows: logon task via
`conhost --headless`), user-level Claude Code hooks
(Stop/StopFailure/PreCompact → `aimem submit-claude`; PostToolUseFailure
for Bash → `aimem failure-recall`), global OpenCode
plugin. The installer **restarts** the service (enable --now alone keeps a
stale binary running).

What a project install does (run boot.sh/install.sh in the project dir):
`docs/SESSION-STATE.md` template, `.claude/settings.json` with SessionStart
hook `aimem session-start`, MCP registration in `.mcp.json` and
`opencode.json`, handoff protocol stubs in `AGENTS.md`/`CLAUDE.md`.

Important: checkpoint hooks live at **user level only**. Never add
Stop/StopFailure/PreCompact hooks to a project's `.claude/settings.json` —
duplicate registration produces duplicate journal events.

### Upgrades

Upgrade with the same one-liner, naming the newer release; do not swap
binaries by hand. Each one-liner fetches its boot script from a release
tag, and the script installs that release's binary, refusing one whose
hash is not in the release's `SHA256SUMS`. Run it from any directory:
when aimem is already installed the one-liner wires only a directory
that already holds `.aimem.json`, and it never wires the home directory.
`AIMEM_USER_ONLY=1` upgrades the user level and wires nothing:

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.13/boot.sh | AIMEM_USER_ONLY=1 bash
```

In a PowerShell window that is already open (the setting stays for that
window):

```powershell
$env:AIMEM_USER_ONLY = '1'; irm https://raw.githubusercontent.com/BlackVS/aimem/v0.10.13/boot.ps1 | iex
```

From cmd (likewise for that window):

```bat
set AIMEM_USER_ONLY=1
powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aimem/v0.10.13/boot.ps1 | iex"
```

On an existing installation the installer:

1. reads the installed version and asks the running service for its
   state root (a stopped service's binary answers `aimem state-root`,
   which reads the same environment and env file the service would);
2. stops only this installation's service (the systemd user unit, or on
   Windows the `aimem-serve` task and the serve processes of this
   binary), and the sync job that opens the same databases;
3. copies the state root, without the socket, to
   **`<state root>.backup-<UTC time>`** beside it: by default
   `~/.local/state/aimem.backup-20261007T170945Z`, on Windows
   `%USERPROFILE%\.local\state\aimem.backup-…`, on a hub
   `~sessiond/.local/state/aimem.backup-…`;
4. swaps the binary, starts the service, and waits for health at the new
   version (`AIMEM_UPGRADE_WAIT`, 30 seconds);
5. if the new release does not answer at its version in time, stops it,
   puts the previous binary back, moves the state it left to
   `<state root>.failed-<UTC time>`, restores the copy, starts the
   previous release, and exits with an error that says so.

A schema move is one-way (a binary refuses a database newer than it
understands), which is why the copy is taken before the new release
first opens the state. The backup stays after a successful upgrade.
It holds everything the state root holds, credentials included, with
the same permissions; remove it once the new release has proved itself.
On macOS the service is a launchd user agent, which the upgrade stops
and starts the same way. Without a service manager (`AIMEM_NO_SERVICE=1`
or `AIMEM_NO_SYSTEMD=1`, or no login session on a Mac) the installer
still takes the backup and swaps, but it refuses to run while an
unmanaged `aimem serve` answers, and leaves starting the service to you.

## 2. Hub setup

The hub is the same binary run as a server. Throughout this manual
`hub.example.com` stands for your own host; a deployment may run several
physically separate hubs (see 3d) so that, say, work and personal
projects never share a server.

Fresh host (Debian/Ubuntu LXC or VM), as root — one command does user,
binary, env, units, timers:

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.13/install-hub.sh | bash
```

Knobs (env): `AIMEM_HUB_USER` (default sessiond), `AIMEM_HTTP_LISTEN`
(:8440), `AIMEM_HTTP_TOKEN` (generated if unset; the hub's admin
credential, kept in the env file, never printed and never put on a
client), `AIMEM_DOMAIN=<fqdn>` (generates a **self-signed** cert for that
name into `~/.config/aimem/tls/` and serves TLS — the installer prints
the certificate's pin at the end, and clients record it with
`aimem hub add ... --pin sha256-BASE64` (or the certificate with
`--ca-file cert.pem`) and keep verification on; `--insecure` skips
verification, is a last resort, and every path that carries an
individual credential (task tools, team setup, team mode) refuses it),
`AIMEM_TLS_CERT`/`AIMEM_TLS_KEY` (explicit cert paths; nothing set =
plain HTTP, which carries checkpoints but not the task tools, team setup
or team mode), `AIMEM_OPENAI_API_KEY` + `AIMEM_OPENAI_BASE_URL` + models
(enables the curation timer's LLM work). Idempotent: run the newer
release's one-liner to upgrade, with the backup and rollback of
[Upgrades](#upgrades) (the hub's backup lands in
`~sessiond/.local/state/aimem.backup-<UTC time>`). Everything stateful
lives outside the binary — config
`~/.config/aimem/env`, certs `~/.config/aimem/tls/`, data
`~/.local/state/aimem/` — so an LXC backup captures the whole hub.

### What a hub serves

| Path | Auth | Contents |
|---|---|---|
| `/` | none | Status card: liveness, build, hostname, uptime. Nothing about what the hub holds. |
| `/v1/status` | none | The JSON behind that card. |
| `/admin` | none to load | The console shell. Holds no data; it asks for the hub token once (browser localStorage) and calls the API with it. |
| `/tasks` | none to load | The task page (list, detail, history, discussion). Holds no data; it asks for a token in the browser — an ordinary project token works, and sees exactly what the task service allows it. The list sorts by update time (newest first, the default), title, state or creation, a choice kept per browser; the board shows the newest tasks first in each column. **↻ refresh** reloads the list or the board from the hub without reloading the page, and the view shows when it was last loaded; nothing refreshes on its own. The list route takes the same choice as `order` (`id`, the API's default, `updated_desc`, `updated_asc`, `title`, `state`). The list's search field finds tasks by words in their title, objective, acceptance criteria, non-goals or comments, best match first, with the hit shown under each row; the sort is off while a search is active (the route's `q`, which takes no `order`). `/tasks?task=<id>` and `/tasks?task=<id>&comment=<id>` are the copyable links. |
| everything else | `Authorization: Bearer $AIMEM_HTTP_TOKEN` | The whole API — projects, memories, usage, logs, config. |

Those first four are the complete unauthenticated surface. Adding a
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
   # Provider registry (console, Usage tab): base URLs must be public
   # https endpoints. Set this to allow http and loopback, private or
   # link-local addresses (a local Ollama, a LAN gateway). The admin API
   # cannot set it.
   #AIMEM_PROVIDER_ALLOW_PRIVATE=1
   # LLM call pacing (optional): outbound curate/embed calls are spaced
   # AIMEM_LLM_INTERVAL seconds apart (default 2; 0 disables) and retry
   # transient upstream blocks AIMEM_LLM_RETRIES times (default 3) with
   # backoff. On a detected rate-block the spacing widens adaptively
   # (doubling, capped, decaying on success) — Cloudflare-fronted
   # provider chains block bursts, not clients. Current pacing shows in
   # `aimem health` (llm_pace) and the TUI's Hub tab; block/retry events
   # log to stderr with an "aimem llmrate:" prefix.
   #AIMEM_LLM_INTERVAL=2
   #AIMEM_LLM_RETRIES=3
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

Every machine gets its own named token; the `AIMEM_HTTP_TOKEN` env token
is the hub's admin credential and belongs on no client. A named token is
revocable alone, carries a role, and stamps shared-document writes with
its name (so "who overwrote my handoff" is answered by authentication).
On the hub host:

```sh
aimem token add laptop                # role writer (default); secret printed ONCE
aimem token add ops --role admin
aimem token list                      # names and roles — no secrets exist to show
aimem token rm laptop                 # revoke = delete one line
```

A **writer** token covers what a workstation needs: events, sync,
recall, shared documents, MCP. **admin** adds config, providers,
rename, drop, retention, chapter tools, and logs. Secrets are stored as
sha256 digests in `<state-root>/tokens.json` (0600, host-local, never
synced); the `AIMEM_HTTP_TOKEN` env token keeps working as an implicit
admin. No restart needed — the registry is read per request. The full
route-by-route contract is served at `GET /v1/openapi.json` (bearer)
and shipped in the repo (`internal/server/openapi.json`).

#### Clients that hold the env token

Installers before 0.10.12 printed the env token as the client
credential, so a client set up from that output acts as hub admin: its
checkpoint pushes, syncs and document tools all carry admin authority. A
client holds the env token when the token in its hub entry (`hub.json`
in the client's aimem state directory; `aimem hub list` shows its first
six characters) is the hub's `AIMEM_HTTP_TOKEN`. To move each such
client to its own writer token:

1. On the hub host, as the service user: `aimem token add <machine>`.
2. On the client: `aimem hub add <name> <url> "<writer-token>"`, with
   `--pin sha256-...` (as the installer prints it) for a self-signed
   certificate. The entry keeps its task credential and its recorded
   trust; an `--insecure` flag that is not restated is dropped.
3. When no client holds the env token any more, replace
   `AIMEM_HTTP_TOKEN` in `~/.config/aimem/env` on the hub with a new
   random value, restart the service (`systemctl --user -M sessiond@
   restart aimem`), and paste the new value into the console.

Step 3 ends the old token everywhere at once, so finish steps 1 and 2 on
every client first.

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

Mint the machine a writer token on the hub (`aimem token add
<machine>` — see "Named tokens" above), then:

```sh
aimem hub add home https://hub.example.com:8440 "$(cat ~/.config/aimem/hub_token)"
```

Add `--pin sha256-...`, as the hub installer printed it, when the hub
serves its self-signed certificate. The token is the machine's own
writer token, never the hub's env token.

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
  word before the hard context error. OpenCode 2 has no toast API:
  there the warning goes to the model, as a system note on each
  request past the threshold, and to the OpenCode server log.
- `AIMEM_AUTO_COMPACT=<fraction>` (e.g. 0.9; unset = off) — trigger
  compaction automatically at that fill, while the summarizer still has
  room to run. The journal, compaction marker, and handoff injection
  make the "prepare" half automatic; this closes the loop for long
  unattended tasks. OpenCode 1.x only: OpenCode 2 plugins cannot
  request compaction, so on 2.x use its `compaction` settings
  (`auto`, `buffer`, `keep.tokens`) instead.
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
where the hourly curation actually runs. The TUI AI tab shows global
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
configured), and the hub does it after each hourly curation run, so no
manual step remains.
Embed spend is metered into the run history and budget-gated.
Unset env = BM25-only recall — everything still works.

### Enabling tasks for a project

For the complete enablement, user/grant/token and agent setup sequence,
see the [Kanban quickstart](KANBAN-QUICKSTART.md).

Tasks (the Kanban work: the `/tasks` page, the task routes, the MCP task
tools) are off for a project until an admin switches them on. Most
projects on a hub are journals and memories and never need them.

```sh
aimem tasks on  -p <project>     # on the hub host, as the service user
aimem tasks off -p <project>
```

The console offers the same switch in a project's menu ("tasks (Kanban):
switch on / off"), and the admin API is `PUT /v1/projects/<p>/meta/tasks`
with `{"value":"on"}` or `"off"`; a writer token cannot set it. Switching
off refuses every task mutation immediately, for every credential, and
keeps the tasks readable through their links; switching on again resumes.
On upgrade to the first release with this setting, every project that
already held a task was switched on once; nothing switches a project on
after that except an admin. An agent whose session started with tasks on
gets one notice when the hub turns them off, and needs a session restart
to see the tools again after they are switched back on.

### Selecting a project's process documents

When a project's tasks are on, its agents receive the process documents
at session start: the handbook (states, who moves a task, evidence and
review rules), the checklists that gate READY and DONE, and where the
task templates are. The files live in a Git repository; the hub stores
the selection (repository, commit, manifest) and, with it, the files of
that version (the set), and serves them to each member under its project
grant. A member machine needs neither Git nor access to the repository.

```sh
aimem process select https://example.com/team/process.git v1.4.0 -p <project>
aimem process select https://example.com/team/process.git v1.5.0 --expect v1.4.0 -p <project>   # change it later
aimem process select https://example.com/team/process.git \
    3f2a…(40-hex commit) process/manifest.json --ref main -p <project>   # by commit, another manifest
aimem process clear --expect <current tag or commit>
aimem process show -p <project>                      # what a session gets
aimem process show --full -p <project>               # the whole unit, plus the repository URL
aimem process list                                   # every project's selection, and whether its set is stored
aimem process refresh                                # store the sets of selections that have none
```

`select` takes a tag (or a branch) or a full commit, and the manifest
path, which defaults to `processes/aimem/manifest.json`. A tag is
resolved on the hub host with `git ls-remote` to the commit it holds at
that moment (an annotated tag to the commit it points at); without `git`,
or when Git cannot resolve it, from the forge's ref advertisement over
https (`<repository>/info/refs?service=git-upload-pack`, what `git
ls-remote` reads, with the same result); the selection
stores both, the tag for people and the commit as the pin. A name that is
both a tag and a branch, on different commits, is refused. `select` runs
on the hub host and reads the set at the commit from Git
with that host's access; on a host without `git`, or when Git cannot read
it, from the forge's tarball of the commit over https
(`<repository>/archive/<commit>.tar.gz`, the GitHub and Gitea form),
which must record that commit (`git archive` writes
it in the tarball's header), and the report names the source. With
`--from-archive FILE` it reads a tar.gz
verified against the `SHA256SUMS` beside it (`--sums` names another
file); a forge's source tarball of the commit works as the archive. For
a hub host without access to the repository, `aimem process export
<repo> <commit> <manifest>` on a machine that has it writes
`<name>-<commit12>.tar.gz` and its `SHA256SUMS` line, to carry over and
import. The hub keeps the sets of the last five selections, so an earlier
one is re-selected without an import; the hub itself never runs Git.
The prerequisites on the hub host are therefore those of these admin
commands only: `git`, or https to the forge.

The forge reads (the tarball and the ref advertisement) are anonymous
unless `AIMEM_PROCESS_FORGE_TOKEN` is set in the hub user's
`~/.config/aimem/env`: a read-only token the forge accepts for the
process repository. It is sent as `Authorization: token
<token>` to the repository's own host only, and a redirect to another
host (GitHub sends tarballs to `codeload.github.com`) is followed
without it. That one token goes to the host of every selected
repository (`refresh` reads them all), so it must be accepted by, and
safe to send to, every forge the hub's projects use. When they use more
than one forge, give each host its own token in
`AIMEM_PROCESS_FORGE_TOKENS`, as `host=token` entries separated by commas
(the env file is also read by a shell, so a value with spaces would need
quotes):

```sh
AIMEM_PROCESS_FORGE_TOKENS=github.com=<token>,git.example.org:3000=<token>
```

A host is matched without case and with its port, `:443` implied, so
`github.com` covers `https://github.com/...` and a forge on another port
is named with that port. A host with its own entry gets that token; any
other host gets `AIMEM_PROCESS_FORGE_TOKEN` when it is set, and an
anonymous read otherwise. A malformed entry fails the read with the
entry's position, never its text. A token is never
printed or stored with the selection, and Git
does not use it: with `git` installed, Git reads with the host's own
credentials first. A repository reached only over ssh, or a forge the
host cannot reach over https, still needs the archive import. A
`.gitattributes` with `export-subst` or `eol` rules can make a forge
tarball's set differ from the one Git gives; the hub serves the set it
stored.

The manifest is JSON (`version` 1 or 2): `handbook` (Markdown path),
`checklists` (task state to a JSON file of `{state, items:[{id,text}]}`),
`templates` (kind to a JSON file), `skills` (the skills the process
requires: plain names in version 1; in version 2 each one pinned as
`{"name", "version", "source"}`, the source `release` or `instance`, see
[Importing the skills sets](#importing-the-skills-sets)) and an optional
`budget_bytes` for the injected unit (24 KiB by default). An aimem from
before version 2 refuses a version 2 manifest outright (a decoding error
on the skill objects, not a message naming the version), so move to
version 2 once every member runs a release that reads it. A selection is
pinned to a full commit id, so what a session sees is exactly what was
reviewed, even if the tag it was selected by later moved (release tags of
a process repository are best protected as immutable); the session's
`Process set:` line names the tag and the commit. With a commit, `--ref`
lets a machine fetch from a server that refuses fetch-by-hash, and the
commit must be what that ref holds. Changing the selection needs the
current one named in `--expect`, by its commit or by its tag, so two
admins cannot overwrite each other unseen; re-selecting the commit and
manifest already selected needs no `--expect`. Every selection stays in the
project's history.

A session names the set by its name (the manifest's directory, else the
repository's name), commit and manifest, never by the repository URL,
which `--full` prints for the admin after the unit. Each machine keeps a
cache per set digest under its state directory; the first session after
a selection reads the set from the hub once, verifies it against the
selection's digest and caches it, later sessions read the cache. A member
the hub refuses (an expired credential, a grant removed) sees "denied" and
nothing else; a hub that cannot be reached leaves the session with the
selection as last observed and a warning; a unit larger than the budget is
not injected and the notice says to read it with `aimem process show`. A
hub from before sets were stored, or a selection it recorded then, serves
no set, and the member fetches the commit from Git with its own access, as
before. `aimem process refresh` on the hub host stores those sets from
the recorded repository, commit, manifest and ref, with the same
compare-and-swap a re-select makes, and reports each project; the hub
installer runs it after an upgrade, and warns at its start when the host
has no `git`. A set the host can read neither from Git nor from the
forge's tarball is reported with the `aimem process export` command that writes its
archive elsewhere, and `aimem process refresh -p <project> --from-archive
FILE` imports it. Because the commit comes from the record and is not
typed, the archive's file name must carry the commit's first 12
characters, as the exported `<name>-<commit12>.tar.gz` does.
`aimem process list` shows which selections still lack a set.

`aimem process show` prints what a session receives for the project. In
a checkout bound to a hub, or on a machine with a default hub, it asks
that hub with this machine's credential, exactly as the session-start hook
does. In a directory with no hub binding, on a machine with no hub
configured, such as the service user's home on the hub host where
`select` runs, it reads the project's selection and its stored set from
this host's service, as `list` and `select` do, and prints the same unit;
`--full` and `--template` work there too. A project with no selection, or
a selection without a stored set, is an error that says so, never an
empty line.

### Importing the skills sets

The skills a process requires reach a member from the hub too, never from
a URL. Two sources, one stored form: the public skills release
(`release`), and the instance's own skills and any modified copy of a
public one (`instance`, kept in the instance's configuration repository).
The effective set is the release plus the instance set; an instance skill
replaces a release skill of the same name, and every listing names each
skill's source, so an override is visible.

```sh
aimem skills import --from-archive aiskills-1.33.13.tar.gz             # the release (SHA256SUMS beside it)
aimem skills import --from-archive skills.tar.gz --source instance --version site-3
aimem skills list --hub                                                # this hub's imports and the effective set
aimem skills export [--source instance] --out /srv/transfer            # the archive pair, to carry to another hub
```

`import` runs on the hub host: it verifies the archive against the
`SHA256SUMS` beside it (`--sums` names another file), reads
`skills/<name>/…` below the archive's leading directory (a skills release
archive is laid out so; its `VERSION` file is the version, `--version`
names it otherwise, and an instance archive without one needs it),
requires `SKILL.md` in every skill directory, bounds the sizes and stores
the set on the hub with its digest; a set that does not validate stores
nothing. The hub keeps the last five imports per source, so a process
that pins an earlier version is still served, and removes the files of
sets no source keeps. The archive never leaves the hub host's disk for the
import, and the API imports from no URL and no file: `PUT
/v1/skills/{source}` takes the files from the admin's command only.

A member runs `aimem skills install` in the project checkout. It reads the
process manifest's pins, takes each skill from the hub at the pinned
version (the source's current import, else a kept one), caches the sets by
digest under its state root and writes the skills into the skills
directories of the agent clients found on the machine, the same the
aiskills installers use (`--tool claude,codex,opencode,openhands` names
them, `--user` installs for the user instead of the checkout, `--all`
installs every effective skill); it replaces a skill directory whole and
records each skill's version and source in the directory's
`.ai-skills.json`, which is also what the session bootstrap reads to say
`installed`, `installed 1.33.12, run aimem skills install` or `NOT FOUND`.
`aimem skills list` shows the effective set and what the machine has.

The skills store is hub state: `skills/index.json` and `skills/sets/` under
the state root travel with a backup or a migration of the state root
(section 7). `aimem health` reports it under `skills`: `none` before any
import, `ok` with the version per source, or `fault` with the error when
the index exists and cannot be read, which is a damaged state root to
restore, not an empty store.

## 5. Releases

Releases are cut by pushing a tag:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

`.github/workflows/release.yml` runs the tests, cross-builds static
binaries (CGO_ENABLED=0) for linux/darwin amd64+arm64 and windows-amd64.
It publishes them as release assets with `LICENSE` and a `SHA256SUMS` that
covers both. The release notes are the tag's CHANGELOG section, followed by
the license URL and the `Required Notice:` lines from `LICENSE`.

The installers are pinned to a release. The commit that rolls the
CHANGELOG into the new version also bumps `RELEASE` in `boot.sh`,
`install-hub.sh` and `$release` in `boot.ps1`, and the tag in every
one-liner in `README.md` and `docs/`. `internal/installer` fails while
these disagree with the newest CHANGELOG version, and the release
workflow refuses a tag the installers do not name.
`.github/workflows/ci.yml`
runs vet and tests on Linux, Windows and macOS for every push and pull
request.

## 6. Troubleshooting

| Symptom | Check |
|---|---|
| Anything silently wrong on a client | **`aimem logs`** — client-side warnings (spooled checkpoints, projects bound to unconfigured hubs, doc conflicts; persisted in `<state-root>/adapter.log`) plus the service log ring. This is the first command to run: the submit process's stderr is discarded by OpenCode and buried on Windows |
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
  `spool/`, `process/sets/` the cached process sets, and on a hub
  `skills/` the imported skills sets with their `index.json`, hub state a
  backup must carry).
- `aimem state-root` prints it; `aimem project-id .` prints a project's ID.
- Backups: the hub holds a merged copy of all journals and memories;
  any machine can re-seed another via `sync`.
