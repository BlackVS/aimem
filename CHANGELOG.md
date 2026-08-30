# Changelog

Notable changes to aimem. Versions are release tags on the repository;
because releases are cut per-deployment, entries are grouped by the change
that matters rather than one heading per tag. Fixes that could lose or
misplace data are called out under **Data integrity** — read those before
upgrading a fleet.

The format follows [Keep a Changelog](https://keepachangelog.com/1.1.0/);
this project does not yet promise semantic versioning. The on-disk schema
version is tracked separately (`currentSchema` in `internal/store/store.go`,
currently 8); a binary refuses a database newer than it understands.

## [Unreleased]

Nothing yet.

## [0.2.5] — 2026-08-30

### Added

- **Git-like reconciliation for shared documents**
  (`docs/DESIGN-doc-collab.md`): the periodic sync now fast-forwards a
  bound file the machine hasn't changed to the hub's newer revision,
  auto-applies and pushes back CLEAN three-way merges when both sides
  changed disjoint parts, and on real overlaps leaves the bound file
  untouched — dropping a `<file>.merge` preview beside it, warning in
  `aimem logs`, and flagging it at session start until
  `aimem docs merge` resolves it. Console saves that hit a 409 now
  auto-merge the draft in the editor via a new compute-only
  `POST .../docs/{name}/merge` endpoint (clean → save again;
  overlaps → markers to resolve in place). The hub still never merges
  on write; only unchanged files are ever overwritten.

## [0.2.4] — 2026-08-30

### Fixed

- Console doc-conflict hints now say how a conflict is *resolved*, not
  just that one will happen: the handoff editor's note explains the
  refuse-never-overwrite contract and points the divergent machine at
  `aimem docs merge`, and the save-conflict message names the same
  command for any bound doc.

## [0.2.3] — 2026-08-29

### Added

- **`aimem logs`** — local diagnostics in one command: client-side
  warnings and the service log ring. Adapter warnings (spooled
  checkpoints, orphaned hub bindings, shared-doc conflicts, replay
  drops) are now also persisted to `<state-root>/adapter.log`
  (timestamped, rotated at 512KB) — previously they lived only on the
  submit process's stderr, which OpenCode's detached spawn discards
  and Windows hook plumbing buries; that silence is how the RC binding
  incident hid for four hours. The orphaned-binding sync warning
  (0.2.2) persists there too.

## [0.2.2] — 2026-08-29

### Fixed

- **`aimem sync` now warns, loudly and by name, about projects bound
  to a hub name this machine has not configured.** Since the
  no-fallback partition guard such a project syncs NOWHERE and its
  checkpoints spool indefinitely — correct, but quiet enough to hide a
  project for four hours (found live: a `.aimem.json` said
  `"hub": "work"` on a machine whose hub is named `seclab`; facts
  stayed local and curation saw no events until the binding was
  fixed). Hub names are machine-local — use the same names everywhere,
  as ADMIN-MANUAL already said.

## [0.2.1] — 2026-08-29

### Fixed

- Console KB tree: an expanded scope (project, group, or user) no
  longer shows a count in its title — the "all" row directly beneath
  carries the same number, and the duplicate read as two different
  figures. Collapsed entries keep their counts.

## [0.2.0] — 2026-08-29

### Changed

- **License: PolyForm Noncommercial 1.0.0** (was MIT). Free for
  personal, research, and any other noncommercial use; commercial use
  requires a license from the author (open a GitHub issue titled
  "commercial license"). Versions ≤ v0.1.90 were published under MIT
  and irrevocably remain so. Contributions are accepted under
  CONTRIBUTING.md terms (author may sublicense commercially).

The milestone release closing the 2026-08-29 review-and-build day: a
full project review actioned end to end, all seven feature proposals
shipped (hub-API sync, named tokens + OpenAPI, docs merge, staleness
review loop, session-start knowledge injection, honest headless
metering, and document search below), plus the partition no-fallback
guard, console action menus, and the context/auto-compact toolkit
(v0.1.82–v0.1.90 rolled up — see their entries).

### Added

- **Search finds shared documents** (FEATURE-PROPOSALS #4): `aimem
  search` and MCP `search_journal` now also return matching shared
  documents — name, revision, and a snippet around the hit — so "which
  runbook mentions the proxy" has an answer for humans and agents
  alike. Retrieval stays fetch-by-name and whole; documents are never
  ranked alongside facts. Implemented as an exact case-insensitive
  scan (a project holds a handful of ≤256KB docs; FTS is the recorded
  upgrade path if that shape ever changes).

## [0.1.90] — 2026-08-29

### Added

- **Per-project context knobs**: `.aimem.json` may set `auto_compact`,
  `ctx_warn_fraction`, and `ctx_limit`, overriding the host for
  projects whose models degrade mid-context ("lost in the middle")
  long before the window fills — auto-compacting at 0.2–0.4 is sane
  there while the host default stays lax. Precedence: process env >
  project > `~/.config/aimem/env` > default; fraction values above 1
  read as percent (30 == 0.3).

## [0.1.89] — 2026-08-29

### Added

- **The OpenCode plugin reads `~/.config/aimem/env`** with the CLI's
  exact fold semantics (AIMEM_* lines, quotes stripped, process env
  wins), so `AIMEM_CTX_WARN_FRACTION`, `AIMEM_AUTO_COMPACT`, and
  `AIMEM_CTX_LIMIT` are set in the machine's one aimem config file
  instead of whatever shell launched OpenCode. Restart OpenCode to
  apply.

## [0.1.88] — 2026-08-29

### Added

- **OpenCode context warnings now escalate** — first at
  `AIMEM_CTX_WARN_FRACTION` (default 0.8), again every 5% — so one
  missed toast is no longer the last word before the hard context
  error (observed live: a session sailed past its single warning and
  stuck). **`AIMEM_AUTO_COMPACT=<fraction>`** (opt-in, e.g. 0.9)
  additionally triggers compaction at that fraction, while the
  summarizer still has room to run; the journal, compaction marker,
  and handoff instruction already make the "prepare" half automatic.
  Both re-arm after each compaction. REMINDER: custom-provider models
  report limit 0 and both features stay silent by design — declare
  `limit:{context}` in opencode.jsonc (ADMIN-MANUAL 3b) or set
  `AIMEM_CTX_LIMIT`.

### Fixed

- Console chapter rename refuses the reserved view names `all`,
  `unfiled`, and `~` — a chapter carrying one would be
  indistinguishable from the built-in row it collides with.

## [0.1.87] — 2026-08-29

### Data integrity

- **A project bound to an unconfigured hub silently routed to the
  default hub.** `ResolveHub` fell back to the default for a NAMED hub
  missing from this machine's `hub.json` — so pinning
  `{"hub": "work"}` before running `aimem hub add work` sent that
  project's checkpoints across the work/home partition. Found live
  during a project binding. Push now spools under the named hub
  (delivered the moment it is configured) and warns; nothing else
  falls back.

### Added

- **Console KB tree actions moved into a `⋯` menu on the titles** (user
  feedback: the old "rename…" pseudo-row between chapters read as a
  filter). Selected projects get *rename* and *drop* (drop confirms
  with event/fact counts and says plainly that pushing machines
  re-create the id); groups link to their Groups-tab charter; selected
  chapters get **rename chapter** — the add-then-remove relabeling from
  the taxonomy runbook, automated: chapters meta updates, every filed
  fact swaps labels without ever going chapterless (cap-filled facts
  swap remove-first), and failures are counted and reported, never
  hidden.

### Fixed

- MCP `list_docs` now matches its design and description: the default
  listing covers the project AND its member groups (group docs labeled)
  — a group runbook has no bound file in most member checkouts, so the
  tool is often its only access. An explicit `scope` still narrows to
  one scope.

## [0.1.86] — 2026-08-29

### Added

- **Console Review tab**: the staleness queue in the browser — pick a
  project and an age window, see each stale fact with its confidence,
  corroboration, and last-seen age, and record a verdict in place:
  *still true* (confirm), *update…* (supersede with lineage), or
  *expire*. Acting on a row removes it; the queue stays derived from
  the audit trail.

### Fixed

- **`install-hub.sh` no longer echoes the bearer token on re-runs.**
  The secret is printed once, at creation — the same rule named tokens
  follow; an upgrade must not re-echo a live credential into logs. The
  closing hint now suggests per-machine tokens (`aimem token add`) and
  drops the obsolete `--sync` suggestion.

## [0.1.85] — 2026-08-29

### Added

- **Opt-in session-start knowledge injection** (`.aimem.json`
  `{"session_facts": <tokenBudget>}`): a budgeted slice of recalled
  facts rides into context with the handoff, matched against the
  previous session's requests — conventions reach the agent before the
  first mistake instead of waiting for it to think of `recall_memory`.
  Mechanical and local end to end (BM25-only without embeddings, zero
  egress), fail-open at every step, and framed with "verify before
  relying". Covers project, declared groups, and user scopes.

### Fixed

- **The headless `claude` curate backend now reports real token usage.**
  The CLI's top-level `input_tokens` is only the uncached slice of the
  final call; the bulk of an extraction rides prompt caching, so runs
  recorded ~40-80 input tokens and cross-backend comparison was
  meaningless. Input now counts uncached + cache-creation + cache-read
  tokens. Cache reads are cheaper per token, so counts slightly
  overstate the backend's relative cost — the safe direction for the
  budget brake.

## [0.1.84] — 2026-08-29

### Added

- **Staleness review loop** (`aimem review`, MCP `review_memories` /
  `confirm_memory`, `GET .../memories/review`, `POST .../confirm`):
  surfaces active, unpinned facts that are old (default 30 days),
  thinly corroborated (default <= 2 sources), and untouched since.
  Verdicts are the existing audited writes — confirm (new: audited
  touch + modest confidence reinforcement), supersede, forget — so the
  queue is derived from the audit trail and empties itself as you
  review; nothing new is stored, nothing can rot. Facts that arrived by
  sync with no local history queue by their creation time. Console
  review tab not built yet — next UI iteration.

- **`aimem docs merge <name>`** — three-way merge for a diverged shared
  document: base is the revision this machine last synced (fetched from
  the hub's retained history), non-overlapping edits from both sides
  apply automatically, overlaps land as `<<<<<<<` conflict markers in
  the bound file. After a clean merge the next checkpoint publishes the
  result; after a conflicted one, auto-publish stays quiet until the
  markers are resolved by an edit — conflict markers can never reach
  the hub on their own. The storage contract is unchanged: the hub
  still refuses and hands both sides back; this is client tooling
  around it. MCP's `update_doc` conflict message now points bound docs
  at it.

## [0.1.83] — 2026-08-29

### Fixed

- **The Windows installer's new `aimem-sync` task failed to register**:
  `[TimeSpan]::MaxValue` as the repetition duration renders as
  `P99999999DT...`, which Task Scheduler rejects. An omitted duration
  repeats indefinitely. Found on the first live run of v0.1.82.
- **Windows upgrades left the OLD service running.** Stopping the
  `aimem-serve` task kills its conhost wrapper but orphans the aimem
  child, which keeps serving the parked binary — one machine was found
  serving v0.1.73 after three "successful" upgrades. The installer now
  kills stray serve processes before restarting. If you upgraded on
  Windows before this, run the installer again (or kill `aimem.exe
  serve` yourself) to actually switch binaries.

## [0.1.82] — 2026-08-29

### Added

- **Anti-entropy sync over the hub API** (`docs/DESIGN-hub-sync.md`):
  `aimem sync` now exchanges events, memories, and group config through
  `/v1/sync/*` on the hub's existing bearer+TLS channel — no ssh
  account, keys, or remote binary path. Windows machines finally PULL
  curated knowledge (previously they could only push events); the
  installer registers an `aimem-sync` scheduled task, and Linux
  `enable-sync` without an argument uses the API for every configured
  hub. Both directions carry the machine's project filter (bound
  projects + user + declared groups). Hubs that predate the routes fall
  back to their ssh destination with a warning.
- **Named hub tokens** (`aimem token add|list|rm`): per-machine bearer
  secrets stored as sha256 digests in `tokens.json` (0600, host-local),
  with writer/admin roles — writers get events, sync, recall, and
  shared documents; admin adds config, providers, rename, drop,
  retention, chapter tools, and logs. A named token's writes stamp
  `updated_by` on shared documents, so attribution comes from
  authentication. Revocation is deleting one entry, read per request —
  no restart, no fleet re-key. `AIMEM_HTTP_TOKEN` remains an implicit
  admin.
- **OpenAPI**: the full `/v1` surface is described in
  `internal/server/openapi.json`, served bearer-gated at
  `GET /v1/openapi.json`, and pinned to the real route table by a
  two-way CI parity test (paths, methods, and per-route roles).

### Changed

- The hub's HTTP body timeouts widened to 15 minutes (30s header
  timeout guards the connection): first syncs legitimately stream for
  minutes.

### Data integrity

- **A crash mid-replay could silently lose spooled checkpoints.** Both
  spool replayers claim the file by renaming it to `.replay-<pid>` and
  delete the claim when done; a hard kill in between orphaned the claim
  and nothing ever re-scanned it. Replays now sweep orphaned claims back
  into the spool first (events are idempotent, so the rare sweep of a
  still-live claim is deduplicated, not duplicated).
- **Sync dropped multi-chapter filings.** `ImportMemory` attached tags
  through the merge path, which keeps only the first chapter — so a fact
  deliberately filed in three chapters arrived on peers with one, and
  machines diverged permanently on labels. Import now replicates the
  source's filings (the 3-chapter cap still applies).

### Fixed

- **A malformed `.aimem.json` blocked checkpoints** despite the v0.1.77
  changelog saying it is treated as absent. An unparseable file now
  really is treated as absent — with a stderr warning naming what was
  lost (pin, hub binding, groups) — while an invalid value in a
  parseable file remains a hard error.
- **MCP `update_doc` on a bound doc now rewrites the local file and the
  sync bookkeeping** (push+pull equivalent), as DESIGN-shared-docs §4b
  always specified — previously the next checkpoint's auto-publish
  fought the agent's own write with a spurious conflict.
- **A record the service rejected (4xx) was spooled and reported as
  "service unreachable, checkpoint spooled".** Replay would drop it
  later with a warning, but the submitter saw success and a misleading
  message. A rejection now surfaces immediately and is never queued.
- **Two bound files with the same base name** (`docs/A.md`,
  `notes/A.md`) published as the same document and fought each other on
  every checkpoint. The first binding (the default handoff, then
  declared order) now wins and the later one refuses loudly until
  renamed or unbound.
- **Schema migrations are now atomic per step**: the DDL and the version
  bump commit in one transaction, so a crash mid-migration leaves the
  old schema cleanly instead of a partial one the next start would trip
  over.

### Added

- **Secret scanning for shared documents.** Documents publish as written
  (never silently redacted), so publishers now scan: private key blocks
  and recognised vendor token formats refuse publication on both the
  client and the hub; softer secret-shaped matches warn on stderr.

### Changed

- **Knowledge mutations now surface storage errors.** Audit, tag, source
  and link writes used to be silently best-effort; the design promises an
  audited trail for every mutation, so a failed side-effect write now
  fails the mutation instead of losing the record quietly.
- **Hub spool flushes are bounded** (100 records per checkpoint, early
  give-up after 3 consecutive transport failures) so a large backlog or a
  hub dying mid-flush cannot stall the coding client's Stop hook; the
  remainder drains on later checkpoints. A spool past 8 MB adds its size
  to the per-checkpoint warning so long outages are hard to miss.
- Transactions open with SQLite's immediate lock (`_txlock=immediate`),
  so a concurrent CLI-beside-daemon doc write waits its turn instead of
  failing with a non-retryable snapshot error; conflict-payload
  truncation no longer splits UTF-8 runes; `aimem docs list` compares
  the local file against the sidecar hash instead of fetching every doc
  body from the hub.
- Test coverage extended to the previously untested edges: the real TCP
  auth wrapper (the old test exercised a drifted copy), the embedding
  width guard, UUIDv7 monotonicity and cursor math, MCP's SESSION-STATE
  refusal and CAS-conflict payload, and `aimem docs pull`'s
  refuse-to-clobber guard.

## [0.1.81] — 2026-08-29

### Added

- **Console Docs tab**: browse a project's shared documents, read and
  edit them, walk their revision history, load an old revision back into
  the editor (saving restores it as a new revision, never a rewrite of
  history), and retire one. Saving is a compare-and-swap write, so a
  conflict refuses, names the other writer, refreshes the listing, and
  keeps your unsaved text in the editor.

## [0.1.80] — 2026-08-29

### Added

- **Shared documents** (`docs/DESIGN-shared-docs.md`): whole authored
  files — the handoff, runbooks — versioned on the project's hub with
  compare-and-swap, never newest-wins. `docs/SESSION-STATE.md` is bound
  by default and publishes automatically on every checkpoint whose hash
  changed; a conflict refuses and names the other writer. New CLI
  `aimem docs list|push|pull|diff|log|rm` (tombstoned deletes; `pull`
  refuses to clobber local changes), MCP tools `list_docs` / `read_doc` /
  `update_doc` (CAS conflicts return both sides so the agent can merge;
  SESSION-STATE is file-only by design), and a session-start notice when
  the hub holds a newer handoff than this machine last saw. Schema v8.
  Opt out per checkout with `"docs": []` in `.aimem.json`.

## [0.1.79] — 2026-08-29

### Fixed

- **The admin console was unresponsive on 0.1.78.** Raw newlines inside
  JavaScript string literals made the page's script a SyntaxError, which
  discards the entire script block rather than one function — the token
  gate accepted a paste and Connect did nothing, so a valid hub token
  looked like a dead hub. `TestAdminPageScriptParses` now scans the
  embedded script for string literals left open at end of line.

## [0.1.78] — 2026-08-29

### Added

- **Rename a project from the console.** `POST /v1/projects/{p}/rename`
  moves the journal, its facts, its embeddings and its curate cursor, and
  relabels the `project:<id>` citations that shared knowledge bases keep
  about their contributors. Refuses reserved ids (`user`, `group-*`) and a
  target that already exists. The rename does not reach client machines: a
  client still deriving the old id re-creates it, so pin
  `{"project": "<id>"}` in that project's `.aimem.json`.

### Fixed

- Releases are cut by pushing a tag; `.github/workflows/release.yml`
  tests, cross-builds five platforms with `SHA256SUMS`, and publishes
  them as release assets.

## [0.1.77] — 2026-08-29

### Data integrity

- **The Windows installer wrote UTF-8 BOMs into every JSON config it
  touched.** Go's `encoding/json` rejects a BOM, and a malformed
  `.aimem.json` is deliberately treated as absent so it cannot block
  checkpoints — so a project's hub binding and group membership silently
  vanished and its data went to the machine's default hub. `install.ps1`
  now writes BOM-free, and `ident.readConfig` tolerates a leading BOM
  because editors will keep producing them.

## [0.1.76] — 2026-08-29

### Added

- **Public status page at `/`** plus `GET /v1/status`: liveness, build,
  hostname, uptime, and nothing else. With `/admin` these are the complete
  unauthenticated surface; every other route stays bearer-gated.
- `docs/ADMIN-MANUAL.md` documents that surface, and why the listener is
  8440 rather than 443 (a systemd `--user` service cannot bind below 1024).

## [0.1.75] — 2026-08-29

### Changed

- Admin header is no longer a full-width slab of the accent color: panel
  ground, hairline rule, one line tall, accent kept as the title's ink.

## [0.1.74] — 2026-08-28

### Data integrity

- **Sync re-broadcast defeated hub partitioning.** `aimem import-events` —
  the pull half of every sync — went through `adapter.Submit`, which always
  ends in a hub push. Exported events carry no hub binding, so the push
  went to the importing machine's *default* hub: a machine subscribed to
  two hubs replicated one hub's projects onto the other, no matter how the
  projects were bound. Imports now use `SubmitLocal` (store, never deliver).

## [0.1.72] — 2026-08-28

### Data integrity

- **Superseding a fact with unchanged text destroyed it.** `Remember`
  folded the new assertion into the very row being retired, so the fact was
  marked superseded by itself: expired, and pointing at itself.

## [0.1.63] — 2026-08-28

### Data integrity

- **Curation reinforced stale facts instead of superseding them.** A new
  assertion that contradicted an existing fact raised the old fact's
  confidence. Conflicts now resolve newest-wins, with pinned facts exempt,
  and land in the audit log so they are visible in the Log tab.

## [0.1.49] – [0.1.73] — 2026-08-28

### Added

- **Multiprovider registry.** Per-model provider entries (URL, token, kind)
  in a host-local `providers.json`, managed from the console with a live
  per-model test probe. Model aliases key vector spaces as `<model>@<dim>`.
  The binding's kind selects the curation backend.
- **Console rebuilt around tabs**: Knowledge Base, Groups, AI Setup, Usage,
  Health, Log. Color schemes for both the console and the TUI. The hub's
  name appears in the header and the browser tab. The Log tab carries the
  service ring buffer and the knowledge audit, so curation conflicts and
  zero-yield runs are visible.
- **Knowledge Base browsing**: group, project and user scopes in one tree,
  paged 50 facts at a time, with `/` focusing a quick search.
- **Chapters became labels**: a fact may be filed in up to three, the first
  staying primary. Unfiled facts get a propose → approve → refile pass.
- **Partitioning enforces itself**: hub bindings travel with config sync,
  and `curate --all` skips projects bound to another hub.
- **Configurable embedding dimension** (`AIMEM_EMBED_DIM`), with a width
  guard that refuses a provider which ignores the request. Scaling
  thresholds and the vector-index decision — in-file pure-Go ANN, not
  sqlite-vec (the driver is pure Go) and not a vector server — are recorded
  in `docs/DESIGN-scale.md`.

## [0.1.26] – [0.1.48] — 2026-08-27

### Added

- **Multi-hub topology**: named hubs, per-project binding via `.aimem.json`,
  partitioned sync, and a hub installer (`install-hub.sh`) with self-signed
  bootstrap and a cert-pull enrollment path.
- **Admin web console** at `/admin`, token-gated in the browser.
- **Knowledge-base chapters** and charter-driven group routing.
- **Semantic dedup** at curation time, plus a retroactive `dedup` command.
- `drop-project` across service, API and CLI.
- Design-document synthesis from a knowledge base.

### Fixed

- Read endpoints no longer auto-create projects, which used to resurrect
  dropped ids as empty husks.
- MCP tool schemas never emit `"required": null`, which broke OpenCode.
- `submit-claude` tolerates the UTF-8 BOM PowerShell 5.1 adds to pipes.

## [0.1.0] – [0.1.25] — 2026-08-26

The system's first day: proposal to running fleet.

### Added

- **Session journals**: every turn checkpointed to a local per-project
  SQLite through a unix-socket service, with spool fallback when it is down,
  and compaction assistance for both Claude Code and OpenCode.
- **Hub and spoke**: an authenticated TCP hub that aggregates journals, with
  push-on-checkpoint and cursor-based incremental sync for anti-entropy.
- **Curated memory**: an asynchronous curator extracts durable, typed,
  tagged, confidence-scored facts through an audited write path; knowledge
  groups share curated facts between consenting projects while raw journals
  never cross a project boundary.
- **Hybrid recall**: FTS5 BM25 fused with cosine over in-SQLite embeddings.
- **MCP facade** for recall, journal search and design-doc access.
- **`aimem tui`**: an operator dashboard (projects, groups, AI, hub) with
  token-usage history, budget caps enforced before spend, and hub resource
  monitoring.
- One-liner bootstrap installers for Linux and Windows.
