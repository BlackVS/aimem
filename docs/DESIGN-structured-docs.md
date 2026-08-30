# Structured collections: live hub data, generated markdown

Status: **PROPOSED** 2026-08-30, awaiting verdict. Companion to
DESIGN-shared-docs.md and DESIGN-doc-collab.md, which it deliberately
does not change.

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

A **collection** is a named, per-project set of **records** on the hub.
A record is a small JSON object with a caller-chosen id (e.g.
`GET /v1/users`). The hub is the single source of truth; markdown is a
build artifact.

- **CAS moves from the file to the record.** Each record carries its
  own rev; writes are compare-and-swap exactly like docs. Two agents
  editing different endpoints never see each other. A genuine same-
  record collision is refused with the other writer's name and is small
  enough to re-apply by hand or by agent in seconds.
- **No merge machinery at all.** No diff3, no sidecars, no reconcile.
  Record granularity makes merging unnecessary — that is the entire
  point of the design.
- **Markdown is generated, one way.** `aimem col render <name>`
  produces a deterministic file (stable record ordering by group key +
  id) with a header: `GENERATED from hub collection <name> — do not
  edit; regenerate with aimem col render`. Commit it to git when a
  snapshot matters (a release); otherwise regenerate on demand. A
  generated file is never pulled, pushed, or merged.

## Mechanics

Storage (hub DB, mirroring the docs tables): `collections(project,
name, created_by)` and `col_records(project, collection, id, body
JSON, rev, updated_by, updated_at, deleted)` with per-record audited
history (retain last N revs, like doc_revisions).

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
- Not the KB: records are *authored* data with ids and schema-shaped
  bodies; facts are *distilled* knowledge. No curator involvement.
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
4. Group docs equivalent (shared collections across projects)? Defer
   until a real cross-project dataset appears.

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
