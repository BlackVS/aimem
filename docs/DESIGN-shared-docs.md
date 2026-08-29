# Shared documents over the hub — proposal

Status: **implemented** in v0.1.80 (2026-08-29), as revised after review.
Corrections found during implementation, recorded per protocol:

- The CLI family is `aimem docs` (plural): `aimem doc` was already the
  design-document synthesizer and hub timers call it by name.
- Local bookkeeping lives in a sidecar (`<state-root>/docsync/<project>.json`),
  not the project DB's meta: the submit path is a short-lived process
  that talks to the service over a socket, and granting it (or the
  token-gated meta API) DB write access for two fields would have been a
  bigger surface than a file.
- `.aimem.json` `"docs": []` (explicitly empty) opts out of ALL bindings,
  default included — discovered immediately: an archived checkout still
  carrying a handoff file must not publish it as the live shared one.
- The handoff is file-only end to end (user ruling): `update_doc`
  refuses SESSION-STATE and points at the file flow.
- §4b's "a successful `update_doc` on a bound doc also rewrites the
  local file and the bookkeeping" shipped in v0.1.80 WITHOUT that
  rewrite — found in review 2026-08-29 and fixed the same day (atomic
  file write + sidecar record; group scope untouched since group docs
  have no bound file).

Companion to DESIGN.md.

## The problem

aimem's premise is that agents on several machines share one memory. Its
own most important collaboration artifact does not travel that way. The
handoff (`docs/SESSION-STATE.md`) reaches another machine only if someone
commits and pushes it, and the receiving session pulls before starting.
That works for a repository whose collaborators all have push access and
all remember; it fails for everyone else, and it fails completely for a
public repository whose handoff must not be committed at all.

The gap is general. A project accumulates a small number of documents
that are **authored, whole, and current** — the handoff, a runbook, an
architecture note, working agreements. They are not facts, and forcing
them through the knowledge base would be wrong (below). They are exactly
the kind of thing several agents on several machines should see the same
version of.

## Why not the knowledge base

Facts and documents have different lifecycles, and conflating them
damages both:

| | curated fact | shared document |
|---|---|---|
| Authored by | the curator, from journal events | a person or an agent, deliberately |
| Unit | one assertion | a whole file |
| On conflict | supersede, newest wins, keep the lineage | refuse; the other writer must merge |
| Retrieval | semantic recall, ranked, budget-trimmed | fetched by name, entire |
| Wrong answer costs | one stale fact among hundreds | the handoff is gone |

A fact is disposable and replaceable; a document is not. Dedup,
supersession and RRF ranking are all the wrong operations on a runbook.

## What already exists

- **`meta(key, value)`** per project, with an HTTP surface
  (`GET|PUT /v1/projects/{p}/meta/{key}`) and an allow-list of exposed
  keys (`internal/server/admin.go`).
- **Config sync** ships selected meta keys as JSONL between peers
  (`export-group-config` / `import-group-config`, `cmd/aimem/main.go`).
  Ordinary config is **fill-only**: a peer's value adopts into an empty
  local key, divergence warns and keeps local, because meta rows carry no
  timestamps and a blind overwrite could undo a newer edit.
- **`design_doc` is the exception and the precedent**: it travels with a
  companion `design_doc_ts` so import can apply **newest-wins**. It is
  safe there only because the document is *generated* — losing an edit
  costs a re-synthesis.

So the transport exists. What is missing is a document model with an
honest conflict rule, and a binding to the file on disk.

## Proposal

### 1. A `docs` table, not a meta key

```sql
CREATE TABLE docs(
  name       TEXT PRIMARY KEY,   -- "SESSION-STATE", "RUNBOOK"
  body       TEXT NOT NULL,
  rev        INTEGER NOT NULL,   -- monotonic per doc; the CAS token
  updated_at TEXT NOT NULL,      -- RFC3339 UTC
  updated_by TEXT NOT NULL       -- host + client, e.g. "minis/claude-code"
);
CREATE TABLE doc_revisions(      -- bounded history: the last N bodies
  name TEXT NOT NULL, rev INTEGER NOT NULL,
  body TEXT NOT NULL, updated_at TEXT NOT NULL, updated_by TEXT NOT NULL,
  PRIMARY KEY(name, rev)
);
```

`rev` and `updated_by` are the point. "Who overwrote my handoff, and what
did it say before" is the first question anyone asks when this goes
wrong, and a meta key cannot answer it.

Keep history bounded (say 20 revisions, or 30 days) — this is a
convenience, not an archive. Git remains the archive for anything
committed.

### 2. Compare-and-swap, never newest-wins

A write sends the `rev` it was based on. A stale write is **refused**,
and the response carries the current body so the caller can merge:

```
PUT /v1/projects/{p}/docs/{name}   {"body": "...", "base_rev": 7}
  -> 200 {"rev": 8}
  -> 409 {"error": "...", "rev": 9, "body": "<current>", "updated_by": "minis/..."}
```

Two refinements make CAS safe under retries:

- **An identical body always succeeds**, returning the current rev
  regardless of `base_rev`. A retried publish (crash between write and
  ack, a re-fired hook) is then a no-op instead of a spurious conflict.
- The 409 response body is capped (the size limit below); past the cap it
  carries `rev`, `updated_by` and a truncated head, and the caller runs
  `aimem doc pull` to get the whole thing.

This is deliberately unlike `design_doc`. Newest-wins is correct for a
generated document and destructive for an authored one. It also matches
the handoff protocol already written into `templates/AGENTS.md`
("single-writer: the handoff header names the driving session; do not
overwrite another session's handoff without taking over explicitly") —
the protocol says do not clobber, so the storage should not let you do it
by accident.

It matches the codebase's habit, too: rename refuses an existing target
rather than half-merging, supersede refuses to retire a fact into itself,
budgets refuse rather than overrun.

### 3. Binding to files on disk

`.aimem.json` gains an optional list; `docs/SESSION-STATE.md` is bound by
default when present, because that is the file the whole protocol already
names.

```json
{"hub": "home", "docs": ["docs/SESSION-STATE.md", "docs/RUNBOOK.md"]}
```

Document name = the file's base name without extension. Paths stay
inside the project (no `..`, no absolute paths).

**Local bookkeeping.** CAS needs a base, and it must not come from a
network round trip — capture never depends on the network. The local
project DB's `meta` records, per doc, the last rev and content hash this
machine pushed or pulled (`doc_rev:<name>`, `doc_hash:<name>`). The
publish trigger compares the file's hash against `doc_hash`; the CAS
`base_rev` is `doc_rev`; both update on a successful push or pull. A
turn where nothing changed costs one hash of a small file and zero
network calls beyond the checkpoint itself.

Because docs live in the project's own `journal.db`, `Registry.Rename`
moves them with everything else — no special case.

### 4. Commands

```
aimem doc list                 names, rev, updated_at, updated_by, local/hub state
aimem doc push [name]          publish the local file (CAS; refuses on conflict)
aimem doc pull [name]          write the hub copy to the local file
aimem doc diff [name]          local vs hub
aimem doc log  [name]          recent revisions
aimem doc rm   <name>          retire a doc (tombstone, CAS like any write)
```

`rm` is a tombstone — a rev bump with an empty body and a `deleted`
mark, never row deletion. A machine that still has the file and pushes
gets a conflict naming the deletion, instead of silently resurrecting
the doc; republishing past a tombstone is an explicit `--force`.

`pull` refuses to overwrite a locally-modified file without `--force`,
and says which revision it would have written. Losing an uncommitted
local handoff to a background pull would be the worst possible bug in
this feature.

### 4b. MCP surface

Three tools join the facade, alongside the existing `get_design_doc`:

```
list_docs()                       names + rev + updated_at + updated_by,
                                  for this project and its member groups
read_doc(name, scope?)            body + rev (scope: project | group:<name>)
update_doc(name, body, base_rev)  CAS write; returns the new rev, or the
                                  conflict: current rev, writer, and body
```

This is where the CAS design pays for itself. The conflict response
hands the model both sides, and the tool description instructs it:
re-read, merge deliberately, retry with the new `base_rev` — never
resubmit your own version unchanged. An agent is the ideal executor of
exactly that loop; without an MCP surface, "a human or an agent merges"
is only half true.

Group docs make MCP the primary interface, not a convenience: a runbook
shared by a knowledge group has no bound file in most member checkouts,
so for most of the agents it serves, the tool is the only access. Group
scope follows the recall rule — a project reads and writes only the
groups it is a member of.

Two rules keep MCP from becoming a second, divergent write path:

- **A successful `update_doc` on a bound doc also rewrites the local
  file and the `doc_rev`/`doc_hash` bookkeeping** — equivalent to a push
  followed by a pull. Otherwise the next checkpoint's hash-publish would
  fight the agent's own write with a spurious conflict.
- **Offline, `update_doc` refuses** (CAS needs the hub) with a message
  saying to edit the bound file instead — the checkpoint publisher will
  deliver it when the hub returns. Consistent with docs never spooling.

Session-start injection of the handoff stays hook-based and mechanical;
MCP is the deliberate mid-session layer. Same split as everywhere else
in the system: capture never depends on the model, judgment always
belongs to it.

### 5. Publishing without a new hook

The checkpoint path already runs on every turn and already talks to the
hub (`adapter.Client.submit` → `pushHub`). Reuse the **trigger**, not the
message: after the event push, hash each bound file, and when the hash
differs from `doc_hash` fire a **separate PUT** to the doc endpoint. The
doc body is never embedded in the event payload — `POST /v1/events` is
idempotent by contract and a CAS write is not, so conflating them would
make checkpoint retries produce spurious conflicts and complicate the
append schema for nothing.

**Doc publishes never enter the event spool.** Spooled bodies replay
later in arbitrary order relative to newer edits; CAS would reject the
stale ones, but only noisily. No queue is needed at all: a failed or
conflicted publish leaves `doc_hash` stale, so the very next checkpoint
retries it for free.

On conflict the publish is skipped and a warning goes to stderr, where
the agent sees it — never a silent overwrite, never a blocked
checkpoint. Capture must not fail because a document diverged.

### 6. Session start

`aimem session-start` currently prints the local file. It should:

1. print the local file as today (never block on the network);
2. if the hub holds a newer revision, say so, name the writer, and
   include a bounded excerpt plus the instruction to run
   `aimem doc pull` — the agent then reconciles deliberately, which is
   exactly what the handoff protocol asks for. Never dump a whole large
   document into session context.

The check runs under a hard short timeout (~1s) and fails open: an
unreachable or slow hub degrades to today's behavior silently. Session
start gains no new failure mode.

### 7. Scope: projects and groups

Groups are projects (`group-<name>`), so a group document comes free and
is the collaboration case — a runbook shared by every project in a
knowledge group, with the same CAS discipline.

### 8. Console

A Docs tab: list, view, edit, browse history, restore an old revision as
a new one, and retire. Editing in the console is a normal CAS write and
conflicts like any other. Two rules learned by driving it in a browser:
on a conflict the listing must refresh (a stale "rev 4 by console" beside
a conflict about rev 5 is precisely the confusion the message is trying
to resolve) while the EDITOR must survive, because it holds the unsaved
text the conflict message is telling the user to preserve.

## Non-goals

- **Not a wiki, not a file sync.** A small number of named text documents
  per project. No directories, no binaries, no rename tracking.
- **Not a replacement for git.** Anything that belongs in version control
  still belongs there. This carries the documents that must be current on
  every machine *now*, including the ones a public repository cannot
  commit.
- **Not merge resolution — in storage.** The hub detects the conflict
  and hands both sides back; it never merges. (Since 2026-08-29 the
  CLIENT offers `aimem docs merge <name>`: a line-based three-way merge
  against the last-synced revision, fetched from the hub's retained
  history. Non-overlapping edits apply, overlaps become conflict
  markers in the local file, and the sidecar is rebased so the resolved
  file pushes with a valid CAS base — while conflict markers can never
  auto-publish, because the sidecar hash matches the marker file until
  a human edits it. That is tooling around the CAS contract, not a
  change to it: the storage rule stands.)
- **Not a second transport.** Docs move over the hub HTTP API only.
  They do not ride the ssh `aimem sync` JSONL path: that path is
  fill-only (or newest-wins for generated docs), and CAS semantics do
  not survive it. The hub is the rendezvous; machines that share a doc
  share a hub.
- **Not attribution you can trust adversarially.** `updated_by` is
  advisory (host + client for the "who overwrote this" question), not
  authentication — every writer holds the same bearer token.
- **Not semantic recall.** Documents are fetched by name — but since
  2026-08-29 `aimem search` (and MCP `search_journal`) also FINDS them:
  a case-insensitive multi-term scan returns doc names with snippets,
  and retrieval stays fetch-by-name and whole. Deliberately an exact
  scan rather than an FTS table: a project holds a handful of ≤256KB
  documents, and at that scale scanning beats trigger-maintained index
  machinery; FTS is the recorded upgrade path if the shape changes.
  Ranking documents alongside facts remains a non-goal.

## Open questions (all resolved as built)

1. **Size cap.** RESOLVED as proposed: refuse over 256 KB
   (`store.MaxDocBytes`), warn past 64 KB.
2. **Redaction.** RESOLVED as proposed (2026-08-29): documents publish
   as written, never silently redacted. `redact.ScanAuthored` classifies
   secret shapes; high-confidence matches (private key blocks,
   recognised vendor token formats) refuse publication at BOTH the
   publisher (`adapter.PublishDocs`, naming the file) and the hub
   (`store.PutDoc`, covering console/MCP/REST writes); softer shapes
   warn on stderr and publish.
3. **Do bound files publish by default?** RESOLVED as proposed:
   `docs/SESSION-STATE.md` yes, everything else opt-in; `"docs": []`
   opts out entirely.
4. **Retention of `doc_revisions`** — RESOLVED: 20 revisions
   (`docHistoryKeep`), no time bound; git remains the archive.

## Why this is worth building

It closes a hole in the product's own premise: aimem tells you your
agents share memory across machines, and today its most important
handoff artifact does not. Any team running aimem on more than one
machine hits this on day one. The transport, the hub, the sync path and
the per-turn hook all already exist — this is a table, a CAS check, and
a file binding.
