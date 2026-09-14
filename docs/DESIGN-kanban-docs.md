# The documents an AI-driven Kanban needs

Status: **proposal, draft for review** (2026-09-14). Nothing here is
implemented. Written after the task backend (storage, service, page and
board; v0.4.0) shipped and the first agent-side fixes landed, when it
became clear that the shared-documents feature predates the Kanban idea
and was never asked what a process run by agents needs from it.

Companion to [the Kanban proposal](AIMEM-KANBAN-PROPOSAL.md) (the product
contract for tasks, which this does not change), [the implementation
plan](DESIGN-task-backend-implementation.md), [shared documents]
(DESIGN-shared-docs.md) and [structured collections]
(DESIGN-structured-docs.md). Record corrections here, not silently.

## The problem

A team of agents and one person running a Kanban process needs more than
tasks. It needs the rules of the process (what a state means, who moves a
task, what counts as evidence, which review gates apply), the checklists
that make a state claimable (ready, done), the instructions each agent
loads (skills, agent definitions, review guides), and the product-level
context a task is cut from (brief, roadmap, epics). Today these live in
four places with no stated rule for which goes where: the skills
repository and its installer, the repository's own `AGENTS.md` and design
docs, the machine-wide agent configuration, and — for the handoff only —
the hub's shared documents.

The task subsystem also assumes every project wants it. Most projects on
a hub do not: they are journals and memories with no board. Task tools
that appear for every project, and process rules that reach every
session, are noise for those and a source of accidental writes.

Two facts constrain any answer. Skills, agent definitions and hooks must
be files on disk to be loaded, so they cannot live only on a hub. And a
second copy of any process text — one in git, one on the hub — drifts,
so each document type gets exactly one home.

## The model

**Tasks are opt-in per project.** `.aimem.json` gains `"tasks": true`.
Without it the project has no task tools in the MCP facade (they are not
listed, not merely refused), the task page shows the project as one that
does not run tasks, and the service refuses task writes into it with a
message naming the flag. The service mirrors the flag into the project's
metadata on the hub so the console, the page and a peer hub read it
without the file. Turning it off later archives nothing and deletes
nothing; the tasks stay readable through their links and the flag can go
back on.

**One home per document kind.** The rule that decides placement:

| Home | What belongs there | Why |
| --- | --- | --- |
| Git, next to the code | Anything a pull request should review: design docs, `AGENTS.md`, the review guide, skills, agent definitions, prompts | Versioned with the code it governs; the merge gate is the review |
| Hub, live | Coordination state that changes during work and crosses machines: tasks, the handoff, iteration records, the project's own process notes | Concurrent writers, compare-and-swap, no merge ritual |
| Local disk, distributed from git | Tool-loaded assets: installed skills, hooks, agent definitions | The agents can only load files; the installer keeps them current |

The hub may carry a **read-only mirror** of a git-homed document when
every machine and the console need to see the current version (the
process handbook is the case). A mirror is published by the repository's
CI or by the skills installer, never edited on the hub; the hub's
compare-and-swap refuses a write that is not the publisher's, so a
mirror cannot drift by hand.

**Injection follows the flag.** aimem's session-start hook already puts
the handoff and the session facts into a new session's context. When the
project's flag is on, the same hook adds: a one-line signal ("this
project runs the task process; the tools are `list_tasks` … and the
rules are the handbook"), and the process handbook itself — the short
document, not the skills. Skills and agent definitions arrive by the
installer as today, and the handbook names them by their installed
names. For OpenCode the plugin does what the hook does. Nothing is
written into the repository by injection.

**Global and project document sets.** Global means "shared by every
project that runs the process" and lives in one knowledge group, named
for the dev process, that flagged projects join. Project means the
project's own scope.

Global, in the dev-process group:

- *Process handbook* (mirror of the git-homed text): states and their
  meaning, who moves a task and when, the evidence rules, the review
  gates, the release rules, the retry-key convention per transition.
- *Definition of Ready* and *Definition of Done* as collection records,
  one per checklist item, keyed by the state they gate. READY is
  claimable when the DoR items hold; DONE when the DoD items hold. The
  handbook says so; the service does not enforce it (no forced
  transition graph, by the product contract).
- *Agent roster* as collection records: each agent identity the hub
  knows, its machine, its model, what it may do. The task actor already
  records who wrote; the roster says who that is.
- *Task template* as a collection record: the field set a well-formed
  task carries and what each field is for, so every agent creates the
  same shape.

Project, in the project's scope:

- *The backlog is the tasks in BACKLOG.* No backlog document: it would
  be a second copy that rots.
- *Epics* (see the features below): the grouping above tasks that a
  release or a milestone maps to.
- *Product brief and roadmap*: rarely changed, reviewed like design
  docs, git-homed with an optional mirror.
- *Iteration records* as collection records, only if the project runs a
  cadence: goal, dates, the task ids committed, what landed. Kanban
  alone needs none.
- *The handoff* stays what it is — bound, reconciled, published — and
  links the active task instead of restating it.
- *Evidence* stays in the task's fields, typed (below).

## Mechanics

Two features the Kanban needs from aimem; everything else in this
proposal is convention and lives in the handbook and the skills.

**Task grouping.** A task gains an optional `epic` reference; epics are a
project-scoped collection (`epics/<id>`: title, objective, state, the
release or milestone it targets). The board filters by epic; a release
maps to the set of tasks under its epics; a task without an epic is
allowed and shows as such. Storage: one nullable indexed column and one
filter; the epic record is an ordinary collection record with no new
storage kind. Boundary: `TaskContent`, the list filter, the page's filter
control, the MCP `list_tasks` argument. Not in scope: nesting, roll-up
state, automatic epic closure.

**Typed references.** `candidate_refs` and `evidence_refs` are free
strings today. Each becomes `{kind, ref, note}` with `kind` one of
`task`, `doc`, `record`, `commit`, `pr`, `ci`, `url`, and `ref` validated
per kind (a task id pattern, a document path, a collection record path,
a commit hash, a PR number, a run id, a URL). The page renders each as a
link where the kind allows; the tools accept and return the typed form;
the existing string form is accepted on write as `kind: url` or `kind:
text` for one release and re-served typed. Boundary: `TaskContent`
validation, the page's renderer, the tool schemas, the OpenAPI document.
Not in scope: dereferencing or freshness checks (the proposal's "start
with explicit agent checks" stands).

**The flag.** `.aimem.json` `"tasks": true`; `ident` reads it beside the
hub binding; the MCP facade lists the task tools only when it is on; the
service stores it as project metadata (`tasks=on`) on the first write
from a flagged project and on `aimem sync`, and refuses task writes into
a project whose metadata lacks it with a message naming the flag; the
page reads the metadata to label the project. Boundary: `ident`, the
facade's tool listing, the task routes' project resolution, the page.

**The hook.** The session-start hook's additional context gains, when the
flag is on: the signal line, and the handbook body read from the
dev-process group's mirror (bounded, with the same budget mechanism the
session facts use). Boundary: the hook's context builder; the plugin's
equivalent.

## What this is not

- Not a change to the product contract for tasks: states, the no-forced-
  graph rule, the authorization model and the retry semantics stand.
- Not a workflow engine: no automatic transitions, no enforcement of
  DoR/DoD by the service, no timers.
- Not a second distribution channel for skills: the installer and the
  skills repository remain the only way a skill reaches a machine.
- Not project creation or deletion from the page or the tools: those
  stay admin actions on the console and its routes.

## Open questions

- Should the mirror be published by the skills installer (one machine
  publishes on every install) or by the skills repository's CI (one
  publisher, needs a hub credential in CI)? The CI route is cleaner; the
  installer route needs no new secret.
- Does the flag belong in `.aimem.json` only, or should the hub be able
  to turn it on for a project that has no file (a project created from
  the console)? The metadata mirror suggests both: the file is the
  developer's switch, the console the admin's.
- Epic state: derived from its tasks, or set by hand? The proposal says
  by hand and not in scope to derive; real use may reverse that.
- The typed references' `text` kind for legacy strings: keep it forever
  or refuse it after one release?

## Dogfood first: aimem itself

The first flagged project is this repository. The dev-process group
holds the mirror of the handbook (today's `AGENTS.md` review-gate text
and the Kanban proposal's state table), a DoR and a DoD of five items
each, a roster with the two agent identities that work here, and the
task template. The follow-up lists in the implementation plan become
tasks under three epics: agent enablement, the task page, the access
section in the console. The release that ships the flag is the first
one whose own work was run through the board.

## Why this is worth building

Without the flag, tasks are a hub-wide feature that most projects do not
want, and every agent on the hub sees tools it must not use. Without the
placement rule, the process text will exist in three copies within a
month. Without grouping and typed references, the board is a list of
cards with prose pointers, and the "evidence rules" stay a convention no
tool can check. Each piece is small; together they are what turns a task
service into a process that agents can be trusted to run.
