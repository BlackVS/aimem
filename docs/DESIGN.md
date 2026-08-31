# aimem — Design

Session-resilience and shared knowledge memory for AI coding agents
(Claude Code and OpenCode). This document describes what is actually
built; the `docs/DESIGN-*.md` proposals carry the per-feature rationale
and record corrections found during implementation.

## Goals

1. **Crash/compaction resilience**: no work context is lost when a session
   dies, hangs, or compacts. Recovery is deterministic and LLM-free.
2. **Shared knowledge**: durable, curated facts about each project,
   available to every agent on every machine, with semantic recall.
3. **Local-first**: every machine works offline; the hub is a merge point,
   not a dependency.
4. **Egress discipline**: nothing leaves a machine unredacted; LLM egress
   (curation, embeddings) is opt-in per deployment.

## Components

```
Claude Code hooks ─┐                        ┌─ hourly curator (LLM)
OpenCode plugin  ──┼→ aimem serve (local) ──┼─ embeddings backfill
CLI / MCP tools  ──┘   SQLite per project   └─ sync ⇆ hub (TLS 8440)
                                                hub = same binary, merge point
```

- **`aimem` binary** (Go, pure modernc SQLite, CGO-free static build).
  One binary is the CLI, the local daemon (`serve`), the hub server, the
  MCP facade (`mcp`), the curator (`curate`), and the embedder (`embed`).
- **Claude Code adapter**: user-level hooks `Stop`/`StopFailure`/`PreCompact`
  run `aimem submit-claude`; it parses the session transcript and journals
  one event per turn. `SessionStart` runs `aimem session-start`, which
  re-injects `docs/SESSION-STATE.md` (the canonical handoff) into context.
- **OpenCode plugin** (`.opencode/plugin/aimem.ts`, installed globally):
  journals turns/errors, writes a compaction marker on summarize, and
  instructs the summarizer to end with a verbatim `AIMEM HANDOFF:` line.
- **Hub**: `aimem serve` on `aimem@hub.example.com`, TLS
  `https://hub.example.com:8440`, bearer-token auth. Receives real-time
  event pushes, serves search/recall/MCP, merges memories from all machines.

## Data model

Per-project SQLite databases (WAL) under `~/.local/state/aimem/`, one DB
per project ID (`aimem project-id .` — derived from the git remote or path).

### Journal (append-only)

Events: `kind` (turn | failure | compaction-marker), `outcome`,
`user_request`, `assistant_reply`, `tool_summary`, `session_id`, `turn_id`,
UUIDv7 ids, idempotency key `source:session:turn_id`. FTS5 index for search.
Checkpointing is **mechanical** — no LLM call, ~ms latency, runs on every
turn. Compaction markers are anchored to the last assistant message uuid so
re-fired hooks stay idempotent.

### Memories (schema v9)

- Bi-temporal rows: `created/expired` (transaction time) +
  `valid/invalid` (belief time); forget = expiry, never delete.
- `superseded_by` lineage; `memory_sources` provenance with
  corroboration-on-reassert; confidence reinforced on reassert.
- Chapters are LABELS (v0.1.64): a fact may be filed in up to
  `store.MaxChaptersPerFact` (3) chapters, because knowledge genuinely
  spans topics. Two paths, deliberately different: the MERGE path
  (`addTags`, used when dedup/reassert folds a twin's tags in) still
  keeps the first filing only — a twin filed elsewhere must never
  silently cross-file a fact; the EXPLICIT path (`Tag`: human, admin
  GUI, or a reviewed refile plan) may add labels up to the cap. The
  FIRST chapter filed is primary: tag order is pinned by rowid, and the
  design-document generator files each fact under its primary chapter
  only, so a multi-labeled fact never duplicates in the document. KB
  chapter counts are therefore appearances and may exceed the group's
  fact count (shown separately as "all"). Label-relation graphs are
  deliberately NOT stored: co-occurrence over `memory_tags` derives
  relatedness on demand, with nothing to maintain or let rot.
- Conflict policy (v0.1.63): identical text corroborates; text that
  DIFFERS at cosine >= DedupSim is a rephrasing or an update — cosine
  cannot tell them apart, so **newest wins**, the same survivor rule
  the retroactive `dedup` sweep already applied. Write-time curation
  used to keep the OLDER text, silently dropping updates and
  reinforcing stale facts; it now supersedes (bi-temporal, linked,
  audited) and reports every rewrite in the run report + stderr. A
  **pinned** fact is exempt: it is human-protected, so the incoming
  text folds onto it and the clash is reported instead.
- Typed facts (`kind`: fact | decision | convention | solution | preference
  | reference), entity `tags`, inter-fact `links`.
- Append-only audit table; redact-on-write (secrets stripped before
  storage); `pin` protects from forgetting and ranks first in recall.
- **Scopes**: project | user (reserved `user` DB) | knowledge groups
  (shared DBs `group-<name>`, projects opt in via `.aimem.json`
  `{"groups":[...]}`). The group NAME is its whole identity — no
  hub-scoped namespace, no hidden id. Members spanning hubs make one
  logical KB with per-hub replicas (machines syncing both hubs carry
  facts and config between them); two unrelated groups sharing a name
  on different hubs merge silently the first time a machine bridges
  them. Group names must be treated as globally unique across every
  hub a fleet's machines touch.
- `memory_embeddings(memory_id, model, dim, vec BLOB)` — float32
  little-endian vectors, **in the same SQLite file**. No external vector
  DB: at 10²–10³ facts per project, exact brute-force cosine beats any
  ANN index in simplicity and correctness (sqlite-vec is impossible with
  the pure-Go driver anyway). Embeddings are machine-local derived data —
  **not synced**; each machine/hub backfills its own via `aimem embed`.
- **Vector width is part of the space's identity** (v0.1.73):
  `AIMEM_EMBED_DIM` sends OpenAI's `dimensions` (768 cuts a 3072-dim
  vector from 12KB to 3KB — 4x less scanned by recall, write-time dedup
  and the nightly sweep alike). Vectors of different widths are not
  comparable, so embeddings are keyed `<model>@<dim>`: changing the
  dimension re-embeds cleanly instead of mixing two spaces under one
  name, and `Embed` refuses a response whose width differs from the
  request (a provider that ignores `dimensions` cannot corrupt the
  space — some proxies do exactly that).
- **If an index is ever needed** (decided 2026-08-28; see
  `DESIGN-scale.md`): it lives inside the per-project file, and is
  implemented as a **pure-Go ANN over the existing blobs** — NOT
  sqlite-vec, which cannot load into `modernc.org/sqlite`, and NOT a
  vector server, which would put the network back on the read path and
  turn physical project isolation into a WHERE clause. Recall is
  local-first on every machine; that property outranks index speed.
- **Staleness review loop** (v0.1.84): newest-wins closes conflicts at
  write time, but a fact nobody contradicts can still quietly rot. The
  review queue is **derived, never stored** — a query over the audit
  trail for active, unpinned facts that are old, thinly corroborated,
  and untouched since — and every verdict is an ordinary audited write
  (confirm = audited touch + modest confidence bump; supersede;
  forget), so reviewing is what empties it. Surfaced as `aimem review`,
  writer API routes, MCP `review_memories`/`confirm_memory` (whose
  descriptions forbid mechanical confirmation), and a console Review
  tab.

## Recall pipeline (hybrid RAG)

1. **BM25 leg**: FTS5, query terms literal-quoted and OR-combined, with
   optional `tag`/`kind` filters.
2. **Vector leg** (only when the caller supplies a query vector): SQL loads
   candidate `(id, vec)` under the same filters, cosine-scored in Go,
   top-50.
3. **Fusion**: Reciprocal Rank Fusion (k=60) merges both lists; pinned
   facts always first; token-budget trim last.
4. Query embedding happens **server-side, best-effort**: if
   `AIMEM_EMBED_MODEL` + `AIMEM_OPENAI_API_KEY` are set the server embeds
   the query; any failure falls back silently to BM25-only (fail-open —
   recall never breaks because an embedding endpoint is down).

Verified: zero-keyword-overlap queries rank the semantically-matching fact
first, both locally and over the hub TLS API.

**Session-start injection** (v0.1.85, opt-in): `.aimem.json`
`{"session_facts": <tokenBudget>}` appends a budgeted slice of recalled
facts to the SessionStart hook output, so conventions reach the agent
before its first mistake. The query is built mechanically from the
previous session's requests (no LLM), recall runs through this same
pipeline (BM25-only with no embeddings — zero egress), scope covers
project + declared groups + user, and every step fails open to silence:
session start gains no failure mode.

## Sync and multi-machine

- Real-time: every checkpoint pushes to the hub (`aimem hub <url> <token>`
  config). Hub outage degrades to a local spool file, flushed on next
  contact.
- Anti-entropy: `aimem sync --all-hubs` (systemd timer / Windows
  scheduled task) exchanges events, memories, and group config
  incrementally over the hub's own HTTPS API (`/v1/sync/*`,
  DESIGN-hub-sync) — per-hub cursors with a 1-hour clock-skew overlap
  window. Memory merge is union with **staleness-wins** (a forgotten
  fact cannot resurrect); import is idempotent. The pull legs carry the
  machine's project filter (bound projects + `user` + declared groups) —
  the recall rule applied to sync. Legacy `aimem sync <ssh-dest>`
  remains for hubs that predate the routes.
- Hub is authoritative for nothing; it is the rendezvous. Every machine
  keeps a full local copy.
- **Multiple hubs**: hubs are named in `hub.json` (legacy flat config =
  a hub named `default`); a project binds to one via `.aimem.json`
  `{"hub":"<name>"}`, unbound projects use the default. The binding
  rides event pushes into project meta (like groups), so real-time push
  routes per project (per-hub spools) and `aimem sync --hub <name>` /
  `--all-hubs` ships each hub only its bound projects + the user DB +
  their declared groups. Different hubs' data stays physically separate
  end to end — the point of running a second hub (e.g. work vs home).

## Curation (knowledge distillation)

`aimem curate` reads un-cursored journal events per project, prompts an LLM
to extract durable facts, and lands proposals via the audited Remember path
(actor=curator). Two backends, shared prompt:

- **openai**: any OpenAI-compatible chat endpoint (we use a LiteLLM proxy,
  model `gpt-4o-mini`) — cheap/fast models, Mem0-style economics. Note:
  some newer model families reject a `temperature` field; the client
  omits it.
- **claude**: headless `claude -p` (subscription-covered), run from a
  scratch workdir so curator turns don't pollute project journals.

`--all` loops every registry project, skipping `user`, `group-*`, and
`curator-workdir-*`. Every report includes token usage (input/output,
cost when reported); the LiteLLM per-key spend page is the ledger.
Runs hourly on the hub (`*:15` timer + boot trigger + persistent
catch-up), followed by `embed --all`; projects with no new events cost
zero LLM calls.

**Group curation**: each event push carries the project's `.aimem.json`
membership (adapter → `groups` field → project meta), so the hub knows it
without repo access. The curator offers declared groups to the extractor
(`scope: "group:<name>"`, conservative prompt); landing enforces the
membership gate (undeclared group → skipped) and records origin
provenance (`project:<id>` source) on group facts. `aimem meta -p <id>
groups` shows what a node believes.

## Cost accounting, budgets, and the dashboard

Every curation and embedding run is recorded in `curate_runs` (schema
v5/v6: timestamp, host, model, tokens in/out, cost) and synced between
machines id-idempotently, so any node can account for hub-side spend.
`aimem budget` sets pre-spend caps per calendar window (daily/weekly/
monthly; combined, per-direction in/out tokens, or USD) — a run is
refused before calling the model when usage plus a worst-case projection
would cross a cap, so budgets can never be overrun. `aimem tui` is the
read-only operator dashboard: four tabs (Projects with live tail,
Groups, AI with per-project/group/model token buckets and budget state,
Hub with real CPU/memory/disk gauges and load), htop-style bars,
responsive down to ~50 columns.

## Compaction resilience

- **Claude Code**: PreCompact journals a compaction marker (idempotent,
  one per compaction point); SessionStart re-injects the handoff after
  compaction. Both validated live, including the marker reaching the hub
  over TLS within ~1s.
- **OpenCode**: the plugin's compacting hook journals a marker and forces
  the summary to end with a verbatim `AIMEM HANDOFF: ...` line
  (observable in the compacted summary).
- Recovery beyond the summary: recent turns from the journal
  (`aimem timeline`/`latest`), durable facts from hybrid recall, canonical
  state from `docs/SESSION-STATE.md`.

## Shared documents (schema v8, v0.1.80)

Whole authored files — the handoff, runbooks — versioned on the
project's hub (`docs` + bounded `doc_revisions` tables, last 20
revisions). Writes are **compare-and-swap, never newest-wins**: a stale
write is refused and the response carries the current body, revision and
writer so the caller merges deliberately; an identical body always
succeeds, so retried publishes are no-ops. Deletes are tombstones.
`docs/SESSION-STATE.md` is bound by default and publishes automatically
after any checkpoint whose file hash changed (a separate PUT — doc
bodies never ride the event payload or the spool); `.aimem.json`
`{"docs":[...]}` binds more files, `{"docs":[]}` opts out. Surfaces:
`aimem docs list|push|pull|diff|log|rm` CLI, MCP `list_docs` /
`read_doc` / `update_doc` (SESSION-STATE itself is file-only over MCP),
a console Docs tab, and a session-start notice when the hub holds a
newer handoff. A diverged bound file is reconciled with `aimem docs
merge` (v0.1.84): a dependency-free three-way merge against the
last-synced revision from the hub's retained history — clean edits
apply and arm auto-publish, overlaps become conflict markers that can
never auto-publish on their own. The hub itself never merges; it
refuses and hands both sides back. Since v0.2.5 the periodic sync
reconciles git-style (`docs/DESIGN-doc-collab.md`): unchanged files
fast-forward to newer hub revisions, clean merges auto-apply and push
back, conflicts drop a self-describing `<file>.merge` preview and warn
loudly — `aimem docs sync` runs the same reconcile on demand. Full
rationale and corrections: `docs/DESIGN-shared-docs.md`.

## Structured collections (schema v9, v0.3.0)

Live trees of small JSON records (`col_records` + bounded
`col_revisions`, mirroring the docs pair with a composite
(collection, id) key) for authored structured state under concurrent
multi-agent edit — an API surface, a config matrix, a glossary. The
compare-and-swap unit is the **record**, so writers touching different
entries never conflict and no merge machinery exists at this layer at
all; ids are slash paths forming the tree. Group-scoped collections
live in the knowledge group's database like group docs. Bodies must be
JSON objects (32KB cap, secret shapes refused). Markdown is strictly a
GENERATED artifact (`aimem col render`, deterministic, file or
directory tree); git receives release cuts, never hand edits.
Surfaces: five hub routes (in OpenAPI + the parity test),
`aimem col list|get|put|rm|log|render|import`, MCP `list_records` /
`get_record` / `put_record` (scope resolves from the `.aimem.json`
`{"collections":[...]}` binding), a console Wiki tab (rendered tree
first, table mode second), and search hits alongside journal events
and doc matches. Distinct from the KB by authorship: the curator never
touches a collection. Full rationale and corrections:
`docs/DESIGN-structured-docs.md`; the storage-kind map lives in
`docs/STORAGE-GUIDE.md`.

## Security / egress

- Redaction happens on write, before anything leaves the session.
- Hub auth: bearer tokens (0600 files, never in the repo). **Named
  tokens** (`aimem token`, hashed in `tokens.json`) carry writer/admin
  roles, stamp doc writes with an authenticated name, and revoke
  per-machine; the env token remains an implicit admin. TLS is the
  operator's to provide; the design assumes certificates are delivered
  onto the hub rather than issued by it, so no ACME or DNS-provider
  credentials sit next to the memory. The full surface is described by
  `/v1/openapi.json`, pinned to the real routes by a CI parity test.
- Embeddings and curation are opt-in via environment; unset means fully
  local, BM25-only operation with no egress at all. When they are on,
  the endpoint is whatever the operator points them at — a vendor API or
  a self-hosted proxy.

## Key decisions (with rationale)

| Decision | Why |
|---|---|
| Mechanical checkpoints, not LLM summaries | zero cost/latency, deterministic, can run on every turn |
| SQLite per project, no server DB | zero ops, natural isolation, syncs as rows |
| Embeddings as blobs + brute-force cosine | exact, dependency-free, correct at our scale |
| RRF fusion (k=60) | rank-based, no score calibration between BM25 and cosine |
| Embeddings not synced | derived data; each node re-derives against its own model config |
| Hub-side query embedding, fail-open | clients stay dumb; recall never breaks on egress failure |
| Curation on small models via LiteLLM | Mem0 economics; usage accounted per run |
| One binary for everything | one release artifact, one installer, no version skew |
