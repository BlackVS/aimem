# aimem

**Session resilience and shared memory for AI coding agents.**

Your agent forgets everything when a session ends, crashes, or compacts.
aimem journals every turn to local SQLite as it happens, distils the
durable facts into a knowledge base, and serves both back on the next
session — on any machine.

Works with **Claude Code** and **OpenCode**. One static Go binary, no
runtime dependencies, no external database, no cloud service.

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh | bash
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

## What you get

### Journals — the mechanical layer

- One append-only SQLite database per project, under
  `~/.local/state/aimem/`, with an FTS5 index for search.
- A checkpoint per turn: the request, the reply, tools used, files
  touched, branch, outcome. Idempotent, so a re-fired hook never
  duplicates.
- **Secrets are redacted on write**, before anything is stored or indexed.
- Compaction markers anchored to the last assistant message, so the
  handoff survives the summarizer.
- A handoff file (`docs/SESSION-STATE.md`) re-injected into the agent's
  context at session start — and **shared over the hub** with
  compare-and-swap versioning, so every machine reads the same current
  handoff without a git push; diverged copies get a real three-way
  merge (`aimem docs merge`).

### Knowledge base — the curated layer

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

### Recall

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

- **Web console** on the hub: browse and edit the knowledge base, manage
  chapters, review stale facts, edit shared documents with revision
  history, configure model providers with live test probes, watch token
  spend, read logs. Four color schemes.
- **Terminal dashboard** (`aimem tui`): projects, groups, AI usage, hub
  health, memory and disk gauges.
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
each useful on their own.

**Workstation** — run inside a project directory:

```sh
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh | bash
```

```powershell
# Windows
irm https://raw.githubusercontent.com/BlackVS/aimem/master/boot.ps1 | iex
```

Then restart any running Claude Code or OpenCode session. The first run
installs the binary, hooks, plugin and background service; every run wires
the project you are standing in. Prebuilt static binaries come from the
latest release, so no Go toolchain is needed.

**Hub** (optional) — on a fresh Debian or Ubuntu host, as root:

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/install-hub.sh | bash
```

Full detail: **[INSTALL-CLIENT.md](docs/INSTALL-CLIENT.md)** ·
**[INSTALL-HUB.md](docs/INSTALL-HUB.md)**

## Verify

```sh
aimem health          # service, state root, spool depth
aimem timeline        # the turns just journaled
aimem recall "how does auth work"
aimem tui             # operator dashboard
```

## Documentation

| Document | For |
|---|---|
| [QUICKSTART.md](docs/QUICKSTART.md) | **start here** - workstation, then hub, then curation |
| [INSTALL-CLIENT.md](docs/INSTALL-CLIENT.md) | putting aimem on a workstation |
| [INSTALL-HUB.md](docs/INSTALL-HUB.md) | standing up a hub |
| [USER-MANUAL.md](docs/USER-MANUAL.md) | day-to-day use: handoffs, recall, curation |
| [ADMIN-MANUAL.md](docs/ADMIN-MANUAL.md) | operating a deployment: timers, budgets, multi-hub, troubleshooting |
| [AI-MANUAL.md](docs/AI-MANUAL.md) | for the agent: how to read and write memory well |
| [RECOVERY-RUNBOOK.md](docs/RECOVERY-RUNBOOK.md) | recovering a lost or compacted session |
| [DESIGN.md](docs/DESIGN.md) | architecture, data model, and the reasoning behind both |
| [DESIGN-scale.md](docs/DESIGN-scale.md) | what breaks first as the knowledge base grows, with measurements |
| [DESIGN-multiprovider.md](docs/DESIGN-multiprovider.md) | the provider registry |
| [DESIGN-shared-docs.md](docs/DESIGN-shared-docs.md) | sharing the handoff and other documents over the hub |
| [DESIGN-hub-sync.md](docs/DESIGN-hub-sync.md) | anti-entropy sync over the hub API, named tokens, OpenAPI |
| [DESIGN-doc-collab.md](docs/DESIGN-doc-collab.md) | git-like reconciliation for shared documents |
| [COMPARISON.md](docs/COMPARISON.md) | how aimem relates to mem0, Zep, Letta, claude-mem and friends |
| [CHANGELOG.md](CHANGELOG.md) | what changed, with data-integrity fixes called out |

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

Needs Go 1.25+.

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

Issues and pull requests welcome.

## License

[PolyForm Noncommercial 1.0.0](LICENSE) — free for personal, research,
and any other noncommercial use, source always available. **Commercial
use requires a license from the author** — open a GitHub issue titled
"commercial license" to start that conversation.

Versions up to and including **v0.1.90** were published under MIT and
irrevocably remain so; **v0.2.0 and later** are PolyForm Noncommercial.
Contributions are accepted under the terms in
[CONTRIBUTING.md](CONTRIBUTING.md).
