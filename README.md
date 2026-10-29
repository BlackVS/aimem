# aimem

**Session resilience and shared memory for AI coding agents.**

Your agent forgets everything when a session ends, crashes, or compacts.
aimem journals every turn to local SQLite as it happens, distils the
durable facts into a knowledge base, and serves both back on the next
session — on any machine.

Works with **Claude Code** and **OpenCode**. One static Go binary, no
runtime dependencies, no external database, no cloud service.

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.17/boot.sh | bash
```

---

## Why

Long agent sessions lose context in three ordinary ways: the session
compacts and detail is summarized away, the process dies mid-task, or you
move to another machine. The usual answers are a hand-written notes file
that goes stale, or asking the model to summarize itself — which costs
tokens, takes seconds, and produces a different summary every time.

aimem separates the two jobs that keep getting conflated:

- **Capture is mechanical.** Every turn is journaled by a hook, with no
  LLM in the loop: milliseconds, no cost, deterministic, and it still
  works when the model is down or the machine is offline.
- **Understanding is asynchronous.** A curator runs later, on a schedule,
  turning raw turns into typed, sourced, confidence-scored facts. Nothing
  in your interactive loop waits for it.

Recovery from a crash never depends on an LLM being available.

## Four ways to run it

aimem is useful at every step below, and each step adds a layer to the
one before it. Stop at whichever one you need.

| | What you get | What it needs | Turn it on | Read |
|---|---|---|---|---|
| **1. Local only** | Every turn journaled, the session handoff re-injected at session start and after compaction, the knowledge base and recall over MCP. All of it offline. | One workstation. No hub, no LLM. | The workstation one-liner, run in the project directory ([Install](#install)) | [QUICKSTART](docs/QUICKSTART.md) stage 1 |
| **2. With a hub** | Checkpoints pushed as they happen and synced back, so every machine shares one memory. The shared handoff and documents, structured collections, the web console. Several hubs, with projects bound to them individually. | An always-on Debian or Ubuntu host, and one writer token per workstation. | `install-hub.sh` on the host; `aimem token add <machine>` there; `aimem hub add <name> <url> <token>` on each workstation, with `--pin sha256-...` for a self-signed certificate | [QUICKSTART](docs/QUICKSTART.md) stage 2, [INSTALL-HUB](docs/INSTALL-HUB.md), [QUICKSTART-NEW-PROJECT](docs/QUICKSTART-NEW-PROJECT.md) |
| **3. A hub with tasks** | A project task board that people use in the browser and agents use through the MCP task tools. A selected process gives the agents their handbook, checklists and templates. | Step 2, tasks enabled for the project, a user with a grant on it, and that user's own task credential on the agent machine. The hub entry must be https and verified: by the system roots, `--pin` or `--ca-file`. | `aimem tasks on -p <project>` and `aimem process select https://github.com/BlackVS/aimem.git <release tag> -p <project>` (the reference process, [`processes/aimem`](processes/aimem/)) on the hub; `aimem hub task-token <hub> <token>` or `aimem task-token set` on the agent machine | [KANBAN-QUICKSTART](docs/KANBAN-QUICKSTART.md), [TASK-CREDENTIALS](docs/TASK-CREDENTIALS.md), [INSTALL-CLIENT](docs/INSTALL-CLIENT.md) |
| **4. A hub in a team** | A coordinator and workers, each an agent session in its own checkout: registration and roster, offers and assignments, each worker's reserved attempt and its execution, and messages through each member's inbox. | Step 3, and a team whose members each have their own checkout and ordinary task credential. | `aimem teams provision create` and `add` on the hub; `/join_team` in each member's client | [TEAM-QUICKSTART](docs/TEAM-QUICKSTART.md), [TEAM-AGENT-QUICKSTART](docs/TEAM-AGENT-QUICKSTART.md); the aimem and aicrew boundary is in [DESIGN-AIFORGE](docs/DESIGN-AIFORGE.md) |

**Curation and embeddings are optional at every step, and separate.**
Curation has an LLM distil journal turns into typed, sourced facts. A hub
runs it on a schedule; on a workstation `aimem curate` runs it by hand.
Without it, the knowledge base holds what agents `remember`. Embeddings
give semantic recall over the facts, whether curated or remembered: they
need an embed model, and `aimem embed` embeds new facts. Without
embeddings, recall is full-text only. Budgets cap the spend before each
call ([QUICKSTART](docs/QUICKSTART.md) stage 3,
[USER-MANUAL](docs/USER-MANUAL.md#curation-manual-runs)).

## What you get

### Journals — the mechanical layer

*Every mode. Sharing the handoff and the collections needs a hub.*

- One append-only SQLite database per project, under
  `~/.local/state/aimem/`, with an FTS5 index for search.
- A checkpoint per turn: the request, the reply, tools used, files
  touched, branch, outcome. Idempotent, so a re-fired hook never
  duplicates.
- **Secrets are redacted on write**, before anything is stored or indexed.
- Compaction markers anchored to the last assistant message, so the
  handoff survives the summarizer.
- A handoff file (`docs/SESSION-STATE.md`) re-injected into the agent's
  context at session start — and, with a hub, **shared over it** with
  compare-and-swap versioning, so every machine reads the same current
  handoff without a git push; diverged copies get a real three-way
  merge (`aimem docs merge`).
- **Structured collections** (with a hub) for the artifacts many agents edit at
  once (an API surface, a config matrix): live trees of small JSON
  records on the hub, compare-and-swap **per record** so disjoint
  edits never conflict, markdown strictly generated
  (`aimem col render`) — git gets release cuts, not merge pain.

### Knowledge base — the curated layer

*Every mode. Curation fills it; without curation, agents fill it with
`remember`.*

- Facts are **typed** (fact, decision, convention, solution, preference,
  reference), tagged, and cite the journal events they came from.
- **Bi-temporal**: rows carry transaction time and belief time; forgetting
  is expiry, never deletion, and every write is audited.
- Supersession is a lineage, not an overwrite — you can always see what a
  fact replaced and why.
- **Conflict policy**: identical text corroborates and raises confidence;
  text that differs but means the same thing is an update, so the newer
  wins — except for pinned facts, which are human-protected and report
  the clash instead.
- **Chapters** organize a knowledge base by topic. A fact may be filed in
  up to three, because knowledge genuinely spans topics; the first filing
  stays primary.
- **A staleness review loop** closes the lifecycle: facts that are old,
  thinly corroborated, and untouched queue for a verdict — still true,
  superseded, or expired — from the CLI, MCP, or the console. The queue
  is derived from the audit trail, so reviewing is what empties it.
- **Knowledge groups** let consenting projects share curated facts.
  Sharing is opt-in and physically scoped: each group is its own database,
  and raw journals never cross a project boundary.
- **Kept text stays readable.** One rule,
  [WRITING-PERSISTED-TEXT.md](docs/WRITING-PERSISTED-TEXT.md), covers what
  agents keep: task fields and comments, documents, memories, records and
  saved Markdown. Agents read it through the `writing_rule` MCP tool. Every
  write tool's description and the session start point to it.

### Recall

*Every mode. The semantic half needs an embedding model.*

- **Hybrid**: BM25 full text fused with cosine similarity over embeddings
  by Reciprocal Rank Fusion. Zero-keyword-overlap queries still find the
  right fact.
- Embeddings live as float32 blobs **in the same SQLite file** — no vector
  server, nothing else to run or back up.
- Fail-open by design: if the embedding endpoint is down, recall silently
  degrades to BM25 rather than breaking.
- Exposed over **MCP**, so the agent can query memory as a tool — and,
  opt-in, a budgeted slice of relevant facts is **injected at session
  start**, matched against the previous session's activity, so
  conventions arrive before the first mistake.

### Multi-machine

*With a hub (step 2 and up).*

- A **hub** is the same binary run as a server. Checkpoints push to it in
  real time; if it is unreachable they spool locally and flush later.
- Cursor-based incremental sync provides anti-entropy for anything
  missed — over the same authenticated HTTPS channel, on every platform.
- **Named tokens** with writer/admin roles (stored hashed, revoked per
  machine) make writes attributable; the full API ships with an OpenAPI
  spec pinned to the real routes by a CI parity test.
- Run **several hubs** and bind projects to them individually, so work and
  personal memory never touch the same server.
- Every machine works fully offline. The hub is a merge point, not a
  dependency.

### Operating it

*The console needs a hub; the dashboard works in every mode; budgets and
providers belong to curation.*

- **Web console** on the hub: browse and edit the knowledge base, manage
  chapters, review stale facts, edit shared documents with revision
  history, maintain wikis record-by-record (rendered tree view, per-entry
  history), search the journal, configure model providers with live test
  probes, watch token spend, read logs. Four color schemes.
- **Terminal dashboard** (`aimem tui`): projects with their hub bindings,
  pending merge conflicts, groups, AI usage, per-hub health and spool,
  memory and disk gauges.
- **Budgets** (`aimem budget`) cap curation spend per window, enforced
  *before* the call, so a cap cannot be overrun.
- **Multiprovider**: point curation and embeddings at different endpoints
  — a vendor API, a LiteLLM or vLLM proxy, Ollama, or headless Claude
  Code. API tokens stay host-local and never sync.

## How it differs

Most "AI memory" projects are SDKs for agents you are *building*; aimem
adds memory to a coding agent you already *use*, and keeps the capture
path mechanical. The axes that actually separate the popular options:

| | aimem | claude-mem | Mem0 | Zep | Letta |
|---|---|---|---|---|---|
| What it is | tool for your coding agent | tool for your coding agent | memory SDK + cloud | cloud service (OSS core: Graphiti) | agent runtime (replaces your agent) |
| Journals every turn, verbatim | **yes, mechanical** | yes (LLM in the path) | plugin: LLM summaries | no | n/a |
| Survives context compaction | **yes** | yes | yes (plugin) | no | n/a |
| Works with zero LLM calls | **yes** | no | no | no | no |
| Self-contained storage | **1 binary + SQLite** | local | embedded default | needs graph DB (Graphiti) | git repo of Markdown |
| Multi-machine sync | **partitioned hubs** | no | via cloud | cloud | via git |

Fair detail, sources, and where each of these is genuinely stronger:
**[docs/COMPARISON.md](docs/COMPARISON.md)**.

## Install

**New here? Start with the [Quickstart](docs/QUICKSTART.md)** — fifteen
minutes from nothing to a hub your agents share, in three stages that are
each useful on their own. Adding a project on a new machine to a hub you
already run, with tasks, the handoff and design documents, is
[QUICKSTART-NEW-PROJECT.md](docs/QUICKSTART-NEW-PROJECT.md).

**Workstation** — run inside a project directory:

```sh
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.17/boot.sh | bash
```

Windows, in a PowerShell window (from cmd or Win+R, type `powershell`
first and paste the block there):

```powershell
& {
  $ErrorActionPreference = 'Stop'
  $d = New-Item -ItemType Directory -Force (Join-Path $env:TEMP 'aimem-boot')
  $f = Join-Path $d 'boot.ps1'; $s = Join-Path $d 'SHA256SUMS'
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.17/boot.ps1 -OutFile $f -UseBasicParsing
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.17/SHA256SUMS -OutFile $s -UseBasicParsing
  $want = ((Select-String -LiteralPath $s -Pattern '\sboot\.ps1$').Line -split '\s+')[0]
  if ((Get-FileHash -LiteralPath $f).Hash -ne $want) { throw 'boot.ps1 does not match SHA256SUMS' }
  Unblock-File -LiteralPath $f
  powershell -NoProfile -ExecutionPolicy RemoteSigned -File $f
}
```

The block downloads `boot.ps1` and `SHA256SUMS` from the release,
refuses a `boot.ps1` whose hash the release does not list, and runs it
in a child PowerShell under the `RemoteSigned` policy for that process
only. There is no `irm | iex` and no execution-policy bypass. There is
no one-line cmd form: the check and the run in one line would be too
long to read before running it. Releases up to 0.10.13 publish no
`boot.ps1` asset; they were installed with `irm …/boot.ps1 | iex`, as
their own README shows.

Then restart any running Claude Code or OpenCode session. The first run
installs the binary, hooks, plugin and background service, and wires the
project you are standing in. The one-liner names a release, and the
prebuilt static binary comes from that release, checked against its
SHA256SUMS, so no Go toolchain is needed. To upgrade, run the one-liner of
the newer release from any directory: it backs up the state root first and
rolls back if the new release does not come up, and it wires only a
directory that already holds `.aimem.json`
([ADMIN-MANUAL](docs/ADMIN-MANUAL.md#upgrades)). The home directory is
never wired.

To add another project once aimem is installed, run `aimem wire` inside
it. It writes the same files the one-liner does, and its flags set the
project's keys in `.aimem.json`; `aimem wire --check` reports what is
wired without writing:

```sh
aimem wire --project my-project --hub team
```

The one-liner needs the script to arrive intact. If the Windows block
stops with `boot.ps1 does not match SHA256SUMS`, the download was cut or
changed on the way (often a transient CDN or network-filter hiccup):
retry, or run the installer from a checkout instead. That path skips the
download entirely. (With no aimem installed yet it builds the binary from
the checkout's source, which needs Go.)

```sh
# Linux / macOS, from the checkout
bash ./install.sh user                       # first install only
bash ./install.sh project /path/to/project   # wire one project
```

```powershell
# Windows, from the checkout
powershell -NoProfile -ExecutionPolicy Bypass -File .\install.ps1 -Target C:\path\to\project
```

**Hub** (optional) — on a fresh Debian or Ubuntu host, as root:

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.17/install-hub.sh | bash
```

Full detail: **[INSTALL-CLIENT.md](docs/INSTALL-CLIENT.md)** ·
**[INSTALL-HUB.md](docs/INSTALL-HUB.md)**

## Verify

```sh
aimem health          # service, state root, spool depth
aimem sessions -p "$(aimem project-id .)"                  # this project's sessions
aimem timeline -p "$(aimem project-id .)" -s <session-id>  # the turns just journaled
aimem recall -q "how does auth work"
aimem tui             # operator dashboard
```

## Documentation

Grouped by the steps in [Four ways to run it](#four-ways-to-run-it).

| Step | Document | For |
|---|---|---|
| Start | [QUICKSTART.md](docs/QUICKSTART.md) | **start here** - workstation, then hub, then curation |
| 1. Local only | [INSTALL-CLIENT.md](docs/INSTALL-CLIENT.md) | putting aimem on a workstation |
| 1. Local only | [USER-MANUAL.md](docs/USER-MANUAL.md) | day-to-day use: handoffs, recall, curation |
| 1. Local only | [AI-MANUAL.md](docs/AI-MANUAL.md) | for the agent: how to read and write memory well |
| 1. Local only | [RECOVERY-RUNBOOK.md](docs/RECOVERY-RUNBOOK.md) | recovering a lost or compacted session |
| 1. Local only | [STORAGE-GUIDE.md](docs/STORAGE-GUIDE.md) | what goes where: journal vs KB vs groups vs docs vs wiki, and full-featured project setup |
| 2. With a hub | [INSTALL-HUB.md](docs/INSTALL-HUB.md) | standing up a hub |
| 2. With a hub | [QUICKSTART-NEW-PROJECT.md](docs/QUICKSTART-NEW-PROJECT.md) | one new project on a fresh workstation against an existing hub: tokens, tasks, handoff, design docs, [saving tokens](docs/QUICKSTART-NEW-PROJECT.md#45-saving-tokens) |
| 2. With a hub | [DESIGN-hub-sync.md](docs/DESIGN-hub-sync.md) | anti-entropy sync over the hub API, named tokens, OpenAPI |
| 2. With a hub | [DESIGN-shared-docs.md](docs/DESIGN-shared-docs.md) | sharing the handoff and other documents over the hub |
| 2. With a hub | [DESIGN-doc-collab.md](docs/DESIGN-doc-collab.md) | git-like reconciliation for shared documents |
| 2. With a hub | [DESIGN-structured-docs.md](docs/DESIGN-structured-docs.md) | structured collections: record-granular CAS, generated markdown |
| 3. Hub with tasks | [KANBAN-QUICKSTART.md](docs/KANBAN-QUICKSTART.md) | enabling a project's task board, granting access, connecting an agent, selecting the process |
| 3. Hub with tasks | [TASK-CREDENTIALS.md](docs/TASK-CREDENTIALS.md) | which task credential an agent machine uses, and how to install it |
| 4. Hub in a team | [TEAM-QUICKSTART.md](docs/TEAM-QUICKSTART.md) | starting a coordinator and workers, step by step |
| 4. Hub in a team | [TEAM-AGENT-QUICKSTART.md](docs/TEAM-AGENT-QUICKSTART.md) | registration, messages, assignments and execution over MCP, CLI and HTTP |
| 4. Hub in a team | [DESIGN-AIFORGE.md](docs/DESIGN-AIFORGE.md) | the boundary between aimem (knowledge and backlog) and aicrew (agent coordination) |
| Curation | [DESIGN-multiprovider.md](docs/DESIGN-multiprovider.md) | the provider registry |
| Operations | [ADMIN-MANUAL.md](docs/ADMIN-MANUAL.md) | operating a deployment: timers, budgets, multi-hub, troubleshooting |
| Reference | [DESIGN.md](docs/DESIGN.md) | architecture, data model, and the reasoning behind both |
| Reference | [DESIGN-scale.md](docs/DESIGN-scale.md) | what breaks first as the knowledge base grows, with measurements |
| Reference | [COMPARISON.md](docs/COMPARISON.md) | how aimem relates to mem0, Zep, Letta, claude-mem and friends |
| Reference | [CHANGELOG.md](CHANGELOG.md) | what changed, with data-integrity fixes called out |

## Privacy and egress

Everything is local by default. Journals and memories never leave the
machine unless you configure a hub, and no LLM is contacted unless you
configure one.

- Redaction runs **on write**, before storage or indexing. Shared
  documents are the one exception: authored files publish as written
  (silently rewriting them would corrupt intended content) — instead
  they are scanned, unambiguous secret shapes refuse to publish, and
  softer matches warn.
- Curation and embeddings are opt-in per deployment. Unset means fully
  local, BM25-only operation with no egress at all.
- Provider tokens live in a host-local `providers.json` (mode 0600) that
  is deliberately excluded from sync.
- Hub traffic is bearer-token authenticated; TLS is yours to provide, and
  the design assumes certificates are delivered onto the hub rather than
  issued by it.

## Building from source

Needs Go 1.26+.

```sh
git clone https://github.com/BlackVS/aimem
cd aimem
CGO_ENABLED=0 go build -o aimem ./cmd/aimem
go test ./...
```

SQLite is `modernc.org/sqlite`, a pure-Go implementation, so the binary is
static and cross-compiles without a C toolchain.

## Status

Working and in daily use, but young — expect rough edges, and read the
**Data integrity** entries in the [changelog](CHANGELOG.md) before
upgrading a fleet. Windows support is best-effort: the service and the
periodic sync run as logon scheduled tasks (sync rides the hub API, so
no ssh is needed on any platform).

See the [roadmap](docs/ROADMAP.md) for delivered milestones and the next
delivery sequence. Issues and pull requests welcome.

## License

[PolyForm Noncommercial 1.0.0](LICENSE) — free for personal, research,
and any other noncommercial use, source always available. **Commercial
use requires a license from the author** — open a GitHub issue titled
"commercial license" to start that conversation.

Versions up to and including **v0.1.90** were published under MIT and
irrevocably remain so; **v0.2.0 and later** are PolyForm Noncommercial.
Contributions are accepted under the terms in
[CONTRIBUTING.md](CONTRIBUTING.md).
