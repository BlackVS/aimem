# What goes where: journal, knowledge, groups, docs, wiki

aimem holds five kinds of state. Each exists because a different thing
goes wrong without it, and each has a different writer, granularity,
and conflict story. Mixing them up is the most common way to use aimem
badly — a runbook stored as facts gets shredded by ranking; an API
surface stored as one document conflicts on every concurrent edit.

This guide is the map, plus the recipe for wiring a new project that
uses all of it. Day-to-day commands live in
[USER-MANUAL.md](USER-MANUAL.md); design rationale in the
DESIGN-*.md files each section names.

## The five kinds

| | Journal | Knowledge base | Knowledge group | Shared docs | Wiki (collections) |
|---|---|---|---|---|---|
| Holds | every turn, verbatim | distilled facts | facts shared across projects | whole authored files | trees of small JSON records |
| Written by | hooks, mechanically | the curator (LLM), async | member projects' curators | humans + agents, deliberately | humans + agents, deliberately |
| Unit | event | fact | fact | the whole file | one record |
| Conflicts | none (append-only, idempotent) | dedup / supersession / confidence | same | CAS + three-way merge | CAS per record; disjoint edits never conflict |
| Lifecycle | retention window | staleness review, expiry, audit | same | 20 retained revisions, tombstones | 20 revisions per record, tombstones |
| Scope | one project, never crosses | project (+ user scope) | its own group database | project or group | project or group |
| Read via | search, timeline | ranked hybrid recall | same | fetch whole by name | fetch by id; render to markdown |
| Typical | what happened | conventions, decisions, solutions | cross-project conventions | handoff, runbooks | API surface, config matrix, glossary |
| Design doc | DESIGN.md | DESIGN.md | DESIGN.md | DESIGN-shared-docs, -doc-collab | DESIGN-structured-docs |

Rules of thumb:

- **It happened** → journal. You never write this; hooks do.
- **It was learned** → knowledge base. Mostly the curator's job;
  `remember` (MCP) or `aimem remember` for the explicit cases.
- **Learned, and other projects need it too** → a knowledge group.
  Same lifecycle as the KB, physically separate database, opt-in.
  The NAME is the group's identity across every hub: members spanning
  hubs make one logical KB with per-hub replicas (bridged by machines
  that sync both), and two unrelated groups sharing a name will be
  silently merged the first time a machine bridges them — treat group
  names as globally unique, and keep a group's members on one hub if
  it must never leave that hub.
- **It reads as a narrative, whole** → shared doc. The handoff is one;
  runbooks are the other common case. One writer at a time per file
  is the natural cadence; diverged copies merge like git.
- **It's structured entries that many writers touch at once** → a
  collection. The record is the conflict unit, markdown is generated
  (`aimem col render`), and git receives release cuts of it — never
  hand-edits.

Two distinctions people ask about:

- **KB vs wiki**: the KB is *created by the hub's curator* — distilled,
  confidence-scored, ranked, reviewed for staleness. A collection is
  *authored* — deliberate reference material with stable ids and a
  deliberate tree shape. The curator never touches a collection.
- **Docs vs wiki**: granularity. A document is worth reading top to
  bottom and merges as prose; a collection is looked up by entry and
  never merges at all, because two writers editing different records
  simply don't collide.

## Setting up a big project that uses everything

Assumes a hub is already running ([INSTALL-HUB.md](INSTALL-HUB.md)) and
named consistently on every machine — a binding naming no local hub
syncs nowhere (sync and `aimem logs` warn).

**1. Wire the machine and project** (once per machine / per checkout):

```sh
aimem token add <machine>       # on the hub host: mint a writer token
aimem hub add work https://hub.example:8440 <token>   # on the machine
cd ~/src/megaproject && curl -fsSL .../boot.sh | bash # or boot.ps1
```

**2. Declare the project's shape in `.aimem.json`** (commit it if the
whole team should share it; keep it local if bindings are per-person):

```json
{
  "project": "megaproject",
  "hub": "work",
  "groups": ["platform"],
  "docs": ["docs/RUNBOOK.md"],
  "collections": [
    {"name": "api",      "scope": "group:platform", "render": "docs/api/"},
    {"name": "glossary", "render": "docs/GLOSSARY.md"}
  ],
  "session_facts": 600,
  "ctx_warn_fraction": 0.7,
  "auto_compact": 0.85
}
```

Field by field: `project` pins a stable id (renames and clones keep
one identity). `hub` names which hub this project's data belongs to —
work and personal memory never touch the same server. `groups` opts
into shared knowledge scopes; the group database appears on the hub on
first push. `docs` binds extra files as shared documents
(docs/SESSION-STATE.md is bound by default). `collections` declares
the wikis this project uses — `scope: "group:..."` shares one across
every project that declared the group; `render` is where generated
markdown lands. `session_facts` injects a token budget of relevant
facts at session start. The two `ctx_*`/`auto_compact` knobs are
OpenCode context management, set per project because they are really
model-behavior knobs (a model that degrades mid-context wants
auto-compact at 0.2–0.4).

The file must be plain UTF-8. A BOM is tolerated, but editors that
write one are the leading cause of "my config looks broken".

**3. Seed the authored surfaces:**

```sh
git add docs/RUNBOOK.md && git commit        # the file IS the doc; it
# publishes on the next checkpoint, or immediately:
aimem docs sync

aimem col import api openapi.json            # seed the wiki from a spec
aimem col put glossary terms/checkpoint entry.json   # or entry by entry
aimem col render api                         # first release cut
```

**4. Verify before trusting** (the order failures actually surface):

```sh
aimem health          # service up, right version
aimem session-start   # handoff + injected facts arrive?
aimem docs list       # bound docs at expected revs
aimem col list        # collections visible (group ones flagged)
aimem logs            # anything the hooks warned about
```

**5. Day-2 operations** — curation providers, budgets, timers, the
review loop, token revocation: [ADMIN-MANUAL.md](ADMIN-MANUAL.md).
Point agents at [AI-MANUAL.md](AI-MANUAL.md); it teaches the MCP
tools, including the wiki's `list_records`/`get_record`/`put_record`.

What you get from all of it together: sessions that survive crashes
and compaction (journal + handoff), a knowledge base that improves
while you sleep (curator + review loop), team-wide conventions that
arrive before the first mistake (groups + session facts), runbooks
that are current on every machine (docs), and one living reference
many agents maintain concurrently without merge pain (wiki) — with
git holding clean release cuts of the generated artifacts.
