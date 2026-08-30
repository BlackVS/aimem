# Structured collections: live hub data, generated markdown

Status: **implemented** 2026-08-30 (v0.3.0), same day as proposed.
Corrections against the proposal, all in the simplifying direction:

- No `collections` registry table: a collection exists iff it has
  records (`ListCollections` derives the listing), so nothing can fall
  out of sync with reality. Two tables total (`col_records`,
  `col_revisions`, schema v9), mirroring the docs pair with a
  composite (collection, id) key.
- Record bodies must be JSON *objects* (not arrays/scalars); the
  secret-shape refusal guards records exactly as it guards documents.
- `aimem col put` enforces read-before-write for humans: updating an
  existing record requires `--base-rev` (creation is free) — the CLI
  never silently fetches a base it didn't read.
- The console conflict flow shows the CURRENT record to re-apply onto
  — no merge machinery at record granularity, per the design's point.
- The importer never overwrites: an existing record belongs to its
  writers, so `aimem col import` counts it as skipped.

Companion to DESIGN-shared-docs.md and DESIGN-doc-collab.md, which it
deliberately does not change.

## The problem

Shared docs solve prose: one file, one revision, whole-file CAS, merged
like git. That model breaks down for a different artifact class — big
*structured* documents under constant concurrent edit. The motivating
case: the API surface of the product being developed. Dozens of
endpoints, request/response shapes, status codes — touched daily by
several developers and agents at once. As a markdown file (in git or as
a shared doc) every pair of simultaneous edits to *different endpoints*
still collides at the file level and demands a merge. The pain is
constant push/pull/merge on a file whose conflicts are almost never
real conflicts.

The user's framing, verbatim intent: the live structured doc should
live on the hub as a database; local markdown files are *generated*
from it when needed — never hand-edited, never merged.

aimem already has three nearby mechanisms, none of which is this:
shared docs (whole-file blobs — the granularity is the problem),
KB facts (curated prose atoms, LLM-distilled, not authored records),
and `aimem doc` (synthesizes a design doc *from the KB*, not from an
authored dataset).

## The model

A **collection** is a named set of **records** on the hub — owned by a
project, or (the primary case, per user 2026-08-30) shared by a
**knowledge group**, because the artifact that motivates this is a
framework's live wiki that many projects and parallel sessions each
update in their own part. A record is a small JSON object with a
caller-chosen id. Ids are slash-separated paths, so a collection is a
**tree**: `api/messages/create`, `api/messages/list`,
`api/models/get` — the shape of a real reference wiki (think
platform.claude.com/docs/en/api: sections → pages → entries), not a
flat list. One hub hosts **any number of such wikis** for different
projects and groups side by side — collections partition exactly like
docs and KBs do, so a work hub can carry one framework's API wiki, an
unrelated product's config matrix, and a group glossary without any of
them seeing the others. The hub is the single source of truth;
markdown is a build artifact.

- **CAS moves from the file to the record.** Each record carries its
  own rev; writes are compare-and-swap exactly like docs. Two agents
  editing different endpoints never see each other. A genuine same-
  record collision is refused with the other writer's name and is small
  enough to re-apply by hand or by agent in seconds.
- **No merge machinery at all.** No diff3, no sidecars, no reconcile.
  Record granularity makes merging unnecessary — that is the entire
  point of the design.
- **Markdown is generated, one way.** `aimem col render <name>`
  produces deterministic output (stable ordering: optional per-record
  `order` field, then id) with a header: `GENERATED from hub
  collection <name> — do not edit; regenerate with aimem col render`.
  Rendering to a file flattens the tree into one document; rendering
  to a directory (`--out docs/api/`) emits one file per branch,
  mirroring the id paths. A generated file is never pulled, pushed,
  or merged.
- **Releases are cuts.** "When needed we just cut them as release to
  git": render into the consuming repo, commit, tag — that commit IS
  the frozen snapshot of the wiki at release time, reviewable in the
  PR like any artifact. Between cuts, git holds the last cut and the
  hub holds the truth; nobody maintains both by hand.

## Mechanics

Storage (hub DB, mirroring the docs tables): `collections(project,
name, created_by)` and `col_records(project, collection, id, body
JSON, rev, updated_by, updated_at, deleted)` with per-record audited
history (retain last N revs, like doc_revisions).

Binding (user request, 2026-08-30): a project declares the collections
it uses in `.aimem.json`, mirroring the existing `"docs"` list —

```json
{"collections": [
  {"name": "api-surface", "scope": "group:framework", "render": "docs/api/"},
  {"name": "glossary"}
]}
```

The binding scopes the MCP tools to the declared collections (an agent
in the project sees exactly its collections, nothing else), and
`render` names where `aimem col render` writes the generated output
(file or directory) — optional, because a collection is useful with no
markdown at all. Scope rides the machinery that already exists: a
project-scoped collection lives in the project partition like its
docs; a `group:` collection lives in the knowledge group's database,
shared by exactly the projects that declared the group — same consent
model, same physical isolation. Concurrency across sessions needs
nothing new either: two sessions on different parts of the framework
touch different records; two sessions on the SAME part collide only on
the specific record both edited, and the loser re-reads one record and
re-applies.

API (writer role, OpenAPI + parity test as usual):

```
GET    /v1/projects/{p}/collections                     list
GET    /v1/projects/{p}/collections/{c}                 all records (one call, for render)
GET    /v1/projects/{p}/collections/{c}/records/{id}    one record
PUT    /v1/projects/{p}/collections/{c}/records/{id}    CAS write {body, base_rev}
DELETE /v1/projects/{p}/collections/{c}/records/{id}    tombstone (CAS)
```

CLI: `aimem col list|get|put|rm|render`. MCP tools for agents:
`list_records`, `get_record`, `put_record` (the agent-facing surface —
an agent documenting an endpoint writes one record, not one file).
Console: a table view per collection; edit a record as JSON in place.

The MCP surface is the point ("shared db / structured wiki via MCP"):
to an agent this *is* a shared database — it reads the one record it
needs (a single endpoint's contract costs ~200 tokens, where today it
would slurp a 2,000-line markdown file), writes back the one record it
changed, and never touches a file. Wiki-style cross-references come
free as a convention, not machinery: a record body may name other
records (`"see": ["GET /v1/users"]`), the renderer turns those into
anchors, and `get_record` follows them one hop on request.

Rendering, deliberately simple for the MVP: a built-in deterministic
renderer — records grouped by an optional `group` field, one `##` per
group, each record as a heading + definition list + fenced JSON for
nested shapes. An optional Go text/template at
`.aimem/templates/<collection>.tmpl` overrides it when a project wants
its own layout. No template language beyond text/template, no
per-record markdown bodies in v1 (a record MAY have a `notes` string).

## What this is not

- Not a replacement for shared docs: prose with narrative order (the
  handoff, runbooks) stays whole-file + doc-collab.
- Not the KB, though it rhymes with it (the user's own observation):
  the KB is *created by the hub's curator* — distilled, confidence-
  scored, retrieval-ranked, with its own lifecycle (staleness review,
  supersession). Collections are *authored by the writers themselves*
  — deliberate reference material with stable ids and a deliberate
  tree shape. No curator ever touches a collection.
- Not a general database: no queries beyond get/list in v1, no
  cross-collection joins, no schema enforcement in v1 (a JSON Schema
  per collection is the natural v2 if garbage records become a
  problem).
- Not offline-first: like `docs push`, writing a record needs the hub.
  Reads for render can add a local cache later if it hurts.

## Open questions

1. Record size cap (proposal: 32KB — records are entries, not
   documents; anything bigger belongs in a shared doc or the repo).
2. Does `render` also bind into a project file list so checkpoints
   auto-regenerate? Proposal: no — regeneration is a deliberate act;
   auto-writing generated files into working trees invites confusion
   with doc-collab's reconcile.
3. Import bootstrap: `aimem col import <file.json|.md>` to seed a
   collection from an existing document? Useful, but v2.
4. ~~Group docs equivalent?~~ RESOLVED by the user (2026-08-30): group
   scope is the primary use case (a framework's wiki shared by many
   projects), in scope for v1 via the existing knowledge-group
   machinery.

## Dogfood first: the aimem wiki

The first live collection is aimem's own (user, 2026-08-30). Natural
seed: the hub API reference — `openapi.json` already holds every
route, method, role, and description, so a small importer turns each
route into a record (`api/projects/docs/put`, ...), giving the import
bootstrap (open question 3) a concrete v1 shape instead of a deferred
one. Render lands in `docs/API.md` (or `docs/api/` as a tree) with the
GENERATED header; the parity test keeps openapi.json honest against
the router, and the collection stays honest against openapi.json by
being re-importable. From then on aimem's own sessions maintain their
reference the way the design intends every framework to: edit the
record, regenerate, cut to git at release.

## Prior art (what the world does today, 2026)

Four families, none of which covers this exact shape:

1. **Spec-driven development**: an executable spec (OpenAPI, or tools
   like GitHub Spec Kit / Kiro / Tessl) is the source of truth and code
   is generated/verified against it. Right instinct — spec as anchor,
   not prose — but it lives in git as files, so concurrent multi-writer
   editing has exactly the merge problem this proposal removes.
2. **SaaS wikis over MCP**: Notion, Confluence, Airtable et al. expose
   MCP servers; agents read/write pages or rows. Proves the "shared db
   via MCP" interaction works, but it is a cloud dependency with
   page-level granularity and no deterministic markdown export into
   the repo.
3. **Markdown knowledge bases for agents** (Obsidian-over-MCP, mdflow,
   AGENTS.md conventions): agent-native and simple, but whole-file
   again — the same conflict granularity as our shared docs.
4. **Memory SDKs** (mem0, Zep, Letta): distilled conversational
   memory, not authored structured artifacts; complements, does not
   overlap.

The gap this fills: record-granular CAS + agent-native MCP CRUD +
deterministic repo-committable markdown, self-hosted on the hub the
team already runs.

## Why this is worth building

It completes a clean three-way split of "project memory" by artifact
class: journals capture what happened, the KB holds what was learned,
shared docs carry the narrative — and collections hold the *authored
structured state* of the product itself. The concurrency pain the
whole-file model can never fix (many writers, disjoint entries, one
file) disappears by construction, with the same hub, tokens, audit,
and console the rest of aimem already has.
