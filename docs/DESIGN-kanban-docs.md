# The documents an AI-driven Kanban needs

Status: **accepted 2026-09-14; increments 1 (per-project enablement and
tool listing) and 2 (process reference selection and the hook bootstrap)
implemented** — epics and typed references are not. Written after the task backend (storage, service, page and
board; v0.4.0) shipped and the first agent-side fixes landed, when it
became clear that the shared-documents feature predates the Kanban idea
and was never asked what a process run by agents needs from it.

Companion to [the Kanban proposal](AIMEM-KANBAN-PROPOSAL.md), [the
implementation plan](DESIGN-task-backend-implementation.md), [shared
documents](DESIGN-shared-docs.md) and [structured collections]
(DESIGN-structured-docs.md). This document **amends the product contract**
in two places: a project whose tasks are disabled refuses task, comment
and epic mutations; and the ordinary-token surface gains the scoped
process-reference, directory and epic routes. When it is accepted, the
Kanban proposal's status line points here. Record corrections here, not
silently.

Reviewed twice on 2026-09-14: revised by a second agent (Git references
instead of hub-stored bodies; server-side enablement; explicit
availability states), then a fresh-eyes pass whose findings are folded in
below (increments and their order, epics as task-schema storage, the
compatibility policy for typed references, the unknown-availability rule,
the directory's fields).

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

**Tasks are enabled per project by an admin on the owning hub.** The hub's
project metadata is the sole authority; new projects default to disabled.
An admin enables or disables Kanban through the console or an admin CLI
command. There is no task-enable flag in `.aimem.json`: clients read the
setting from the project's bound hub. Ordinary agents, task writes and
sync cannot change it. Enabling activates task writes, the board and
automatic process-context injection. Disabling refuses all task mutations,
including comments, but preserves tasks and history with authorized reads
through existing links. The project can be enabled again later.

**One home per document kind.** The rule that decides placement:

| Home | What belongs there | Why |
| --- | --- | --- |
| Git, next to the code | Anything a pull request should review: design docs, `AGENTS.md`, the review guide, skills, agent definitions, prompts | Versioned with the code it governs; the merge gate is the review |
| Hub, live | Coordination state that changes during work and crosses machines: tasks, the handoff, iteration records, the project's own process notes | Concurrent writers, compare-and-swap, no merge ritual |
| Local disk, distributed from git | Tool-loaded assets: installed skills, hooks, agent definitions | The agents can only load files; the installer keeps them current |

**The hub stores Git references, not process-document bodies.** An admin
selects a repository URL, immutable commit hash and manifest path per
project. The manifest identifies the handbook, checklists and templates at
that commit. These may live in the existing skills/process repository;
a separate repository is not required. Git review controls their contents.
Changing or rolling back the selection requires admin authority and an
expected metadata revision. Ordinary agents and sync cannot change it.

Clients fetch files from Git and keep a disposable local cache. There is
no hub content publisher, duplicate process store or publisher credential.
Private sources require the consuming machine's own Git access. The page
links to the pinned source; serving process bodies from the hub is outside
this design.

**Injection follows server enablement.** aimem's session-start hook already puts
the handoff and the session facts into a new session's context. When the
project is enabled, the same hook adds: a one-line signal ("this
project runs the task process; the tools are `list_tasks` … and the
rules are the handbook"), and the process handbook itself — the short
document, not the skills. Skills and agent definitions arrive by the
installer as today, and the handbook names them by their installed
names. For OpenCode the plugin does what the hook does. Nothing is
written into the repository by injection.

**Global and project document sets.** Global means shared definitions in
Git that multiple projects select. Project means the project's own scope.
The hub stores the selected reference per project.

Global process definitions in Git:

- *Process handbook*: states, actors, evidence rules, review and release
  gates, and retry-key conventions.
- *Definition of Ready* and *Definition of Done*: structured checklists
  alongside the handbook, with stable item IDs and the states they gate.
  READY is claimable when DoR holds; DONE when DoD holds. The service does
  not enforce these rules. Actual checklist results belong with task
  evidence and identify the source commit and item assessed.
- *Task templates*: the field set a well-formed task carries and what each
  field is for. These are reviewed with the handbook, not duplicated as
  hub collection records.

The live *agent roster* remains on the hub, referencing access identities
through a limited identity directory. The directory is a read-only route
on the ordinary surface that returns, for every user and group the access
store holds: id, kind, name and whether it is enabled — the fields the
board needs to label an assignee and a comment author, and nothing else
(no tokens, no grants, no project lists). It is a discovery widening of
the same kind as the project listing and is pinned the same way: in the
ordinary-token gate matrix and by a handler test. Effective permissions
remain authoritative in the access system; roster entries grant no
authority.

The manifest and required files form a consistent version at the pinned
commit. Validate the complete set before atomically promoting it into the
local cache. Interrupted fetches must not replace a complete cache entry
with partial content or mix files from different commits.

Project-specific rules remain in reviewed project Git files, explicitly
identifying additions or overrides to global defaults. Global rules must
not silently replace them. Conflicts without declared precedence require
resolution. Live notes record coordination and observations; normative
changes go through Git review.

Retain historical repository/commit/manifest references associated with
project work on the hub, not copies of their contents. Selection changes
do not erase these associations. Historical content retrieval depends on
the Git commit remaining accessible or an exact local cache being present;
otherwise report it unavailable. Source maintainers must preserve referenced
commits for durable historical access.

Project, in the project's scope:

- *The backlog is the tasks in BACKLOG.* No backlog document: it would
  be a second copy that rots.
- *Epics* (see the features below): the grouping above tasks that a
  release or a milestone maps to.
- *Product brief and roadmap*: rarely changed, reviewed like design
  docs, Git-homed with pinned references where needed.
- *Iteration records* as collection records, only if the project runs a
  cadence: goal, dates, the task ids committed, what landed. Kanban
  alone needs none.
- *The handoff* stays what it is — bound, reconciled, published — and
  links the active task instead of restating it.
- *Evidence* stays in the task's fields, typed (below).

## Mechanics

The implementation includes project enablement, scoped process-reference
access, Git retrieval and local caching, context injection, task grouping
and typed references. Process conventions live in Git and installed skills.

**Process access.** Ordinary task credentials with read access to an enabled
project may read its selected Git reference and limited identity directory.
Only admins change selection; process content changes use Git review.
Git fetches use the client's own Git access, never elevated hub credentials.
Agents with task-write permission may create and update that enabled
project's epics; epic reads follow task-read permission. Disabling refuses
epic mutations and preserves existing records for authorized reads.

Historical reference reads are independent of enablement. Current task
readers may retrieve Git references associated with the project's recorded
work after disablement or selection changes. Resolve them through retained
project/version associations, not arbitrary caller-selected group paths.
Losing project access denies further hub reads; it does not revoke separate
Git permissions or erase content already downloaded by the client.

Expose scoped process-reference, directory and epic APIs/MCP tools.
Task credentials gain no general document or collection access. The hook
reads the selection from the hub and fetches the pinned files from Git;
the board reads epic names and links to process sources. Every hub request
checks target-project authority. Boundary: route authorization, admin
selection, process-reference/directory/epic handlers, HTTP/MCP schemas,
OpenAPI, client Git retrieval/cache, hooks and page consumers.

**Task grouping.** A task gains an optional `epic` reference; epics live
in the task schema (an `epics` table beside `tasks`, one schema bump:
id, title, objective, state, the release or milestone it targets,
revision, timestamps), written through the same receipt-backed mutation
as tasks and read through routes and MCP tools of the same shape
(`list_epics`, `get_epic`, `create_epic`, `update_epic`). An epic is a
task-shaped entity — project scope, an id never reused, an
expected-revision write, a retirement lifecycle, the enablement gate —
so it gets task-shaped storage rather than a carve-out in the generic
collection routes. The board filters by epic; a release maps to the set
of tasks under its epics; a task without an epic is allowed and shows as
such. Storage: the table, one nullable indexed column on `tasks` and one
filter. Boundary: the task schema and migration, `TaskContent`, the list
filter, the epic routes and tools, the page's filter control, the MCP
`list_tasks` argument.

Epic IDs are stable within the owning project. Creates require an unused
ID; updates and retirement require an expected revision, with stale writes
returning a conflict rather than overwriting concurrent work. Setting a
task's epic validates that it exists in the same project. Retire epics
instead of deleting them: existing task links and history continue to
resolve, and IDs are never reused. Retired epics cannot receive new task
assignments, but an update retaining an existing assignment remains valid.
Retirement is an explicit operation, not a roll-up of task states.

Epic mutations go through the task service, which enforces project
enablement, permission, revision and lifecycle checks exactly as for
tasks; the collections store is not involved. The indexed task reference
and the task snapshot must agree. Not in scope: nesting, roll-up state,
automatic epic closure or hard deletion of epics.

**Typed references.** `candidate_refs` and `evidence_refs` are free
strings today. Each becomes a typed reference: `kind`, `ref`, optional
`note`, and explicit scope where the kind needs it. Kinds are `task`,
`doc`, `record`, `commit`, `pr`, `ci`, `url` and `text`. External targets
use canonical URLs that identify the repository or service as well as the
target; a bare PR number, commit hash or run id is insufficient. Internal
targets identify the owning hub and the project or knowledge-group scope
as applicable, plus the task id, document name or collection and record
id. A Git-homed document uses a repository-qualified reference rather
than an ambiguous local path. References grant no access to their
targets.

Validation checks the shape appropriate to each kind. Link rendering
allows only HTTP(S) URLs or application links constructed from validated
internal identities; unsupported schemes are never clickable. `text`
preserves an unstructured reference as escaped, non-clickable text.

**Compatibility policy: a pre-1.0 break with a migration, not a dual
contract.** The installed base at the time of writing is one hub and one
agent identity on a release hours old, so the typed form replaces the
string form in the next release rather than living beside it. The
schema migration rewrites every stored string reference — in current
snapshots, history and saved retry results — to `url` when it is a valid
HTTP(S) URL and to `text` otherwise, preserving the exact text and never
inferring a target; nothing is silently rewritten to a guessed identity.
Old clients that send string arrays receive a 400 naming the new shape;
old clients that only read receive the typed form. Retry receipts: a
receipt's digest was computed over the request as it was sent, so a
request committed before the upgrade with non-empty string references
would not match its typed retry — the digest's zero-value pruning and
format prefix do not equate a string with an object (the external review
of this document found the gap). The migration therefore recomputes
every stored receipt's digest from its saved result, which is possible
because the result carries the full content and the revision: a create's
input is the created task's content; an update's input is the updated
task's content with `expected` = its revision minus one; a comment's
input is the body and is unaffected. A retry with the same key and the
typed form of the same content then replays; changed content still
conflicts. A legacy string-array request replayed after the upgrade is
refused by validation before any digest is computed, so no receipt is
consulted or written. The alternative — a
permanent v1/v2 contract with an edit adapter and ambiguity rules — was
written out in an earlier revision and rejected as cost without a
beneficiary; it is the right design the day a client outside this
project depends on the string form, and that day has not come.

Boundary: `TaskContent`, storage decoding and migration, the validation
of each kind, the tool schemas, the page's renderer and the OpenAPI
document. Not in scope: dereferencing, freshness checks or certifying
evidence correctness (the proposal's "start with explicit agent checks"
stands).

**Server enablement.** Store the setting in project metadata on the owning
hub, changed only through admin operations. The page reads it to label
the project. Local stdio MCP lists task tools only when the current
project is enabled at session startup; the list stays fixed until the next
session restart. The hub MCP endpoint serves multiple projects and may
expose task tools, enforcing each target project's setting on every call.
Hidden local tools must not bypass the setting when called by name.
When the startup lookup fails (the hub offline or unreachable) the
setting is unknown, not disabled: the facade lists the task tools and
the session context carries the availability notice, because the hub
refuses writes to a disabled project regardless and hiding the tools
would make an offline start look like a disabled project. All service
mutations check current enablement as well as existing task
authorization; a cached setting never authorizes a write.

Upgrade migration enables projects that already contain tasks, including
archived tasks. It must not undo a later admin disablement; the
implementation runs it at every service start and only ever writes where
the key is unset, which is the same as running it once and needs no
"already ran" marker (correction recorded at increment 1).
New projects default to disabled. Migrated projects without a selected
process reference report context unavailable until an admin selects one;
their existing task work remains enabled.

Normal hub interactions detect changes from session-start enablement and
show one notice: "Kanban availability changed for this project. Restart the
session to refresh its tools and process context." No background polling
is required; detection waits for the next normal interaction. Server-side
write restrictions still apply immediately.

A failed or offline lookup means availability is unknown, not disabled.
Clients report that state; a cached setting and handbook may support
session context with their stale/offline status made explicit. Boundary:
admin console and CLI, project metadata and its read interface, bound-hub
lookup, MCP discovery and dispatch, task mutation handlers and the page.

**Session bootstrap.** When the project is enabled, the session-start hook
reads the selected Git reference through the scoped hub interface, then
fetches a complete short process bootstrap from that pinned Git commit. It includes the enablement signal, owning project,
process-set version and source commit, the short handbook, applicable
DoR/DoD and how to obtain the matching templates and other process assets.
It identifies project-specific rules and the installed skill/agent names
the process requires. Hooks and the OpenCode plugin follow the same
contract; injection does not install skills or write repository files.

Process context has its own bounded budget, independent of `session_facts`
and previous-session activity. A first session with no recalled facts
must still receive it. The Git manifest defines a complete bounded bootstrap
unit: do not silently truncate away a review gate or checklist item. If
the required unit exceeds the budget, report it as unavailable with the
reason and a retrieval path rather than presenting a partial policy as
complete. Exact budget and network deadline are implementation choices
that must be explicit and tested.

Unavailable, stale and disabled are distinct states. Bounded hub and Git
fetches must not prevent session startup. Missing files, denied access,
absent credentials, commit mismatch or an offline service produce explicit
availability notices. If Git is unavailable, use only a complete local cache
matching the selected repository, commit and manifest, and warn: "Git is
unavailable; using cached process documents at commit …". If that exact
version is absent, report process context unavailable; do not substitute an
older commit. A confirmed Git access denial is reported as denial, not
disguised as an offline fallback.

If the hub is offline, identify the selection as last observed, including
its last-verification time; the cache must match that selection and the same
hub/project/credential context. Do not mix versions or another project's
assets. Confirmed hub disablement or access denial must not be masked by
cached enablement. Cached context never authorizes task writes.

Report missing required skills when detectable; otherwise identify their
availability as unverified and direct the agent to check before the
dependent step. Missing or incomplete process assets must not be treated
as satisfied gates. Once context is available, agents retrieve referenced
assets from Git at the same pinned commit using the manifest.
Boundary: the hook's context builder, credential handling, process-set
retrieval and cache, availability diagnostics and the plugin's equivalent.

## What this is not

- Task states, the no-forced-graph rule, existing task permission checks
  and retry semantics stand. Admin enablement adds a mutation gate;
  scoped process/epic access extends the ordinary-token API surface.
- Not a workflow engine: no automatic transitions, no enforcement of
  DoR/DoD by the service, no timers.
- Not a second distribution channel for skills: the installer and the
  skills repository remain the only way a skill reaches a machine.
- Not project creation or deletion from the page or the tools: those
  stay admin actions on the console and its routes.

## Remaining implementation choices

- Set concrete bootstrap size budgets and bounded hub/Git fetch deadlines.
- Manifest and checklist format: fixed at increment 2 as JSON — a
  manifest `{version:1, handbook, checklists:{STATE: path}, templates:
  {kind: path}, skills:[…], budget_bytes}` and a checklist `{state,
  items:[{id, text}]}`, parsed by one package (`internal/process`) that
  the hook and the CLI share. The "expected metadata revision" for a
  selection change is a compare-and-swap on the previously selected
  commit, which needs no schema change. The OpenCode plugin has no
  session-start hook today; `aimem process show` prints the same unit for
  any client that can run a command, and the plugin integration is a
  follow-up.
- Specify the typed-reference schema per kind, the epic routes and tools,
  and the migration's exact rewrite rules and its test corpus.

## Increments

Serial PRs, in this order (agreed with the user, 2026-09-14), each with
its own boundary and the acceptance checks below that apply to it:

1. **Enablement and tool listing.** Project metadata setting, admin
   console and CLI operations, the migration that enables projects already
   holding tasks, mutation refusals on disabled projects, the stdio
   facade's startup listing with the unknown-availability rule, the
   restart notice, the page label. Storage touched only by the metadata
   key; ultra review (authorization gate).
2. **Reference selection and the hook bootstrap.** Admin selection of
   repository, commit and manifest with an expected metadata revision;
   the scoped process-reference route; the manifest and checklist format;
   client fetch with atomic cache promotion, the offline and denial
   states, the bounded budget; the hook's and the plugin's context. No
   task-schema change.
3. **Epics.** The `epics` table (schema bump), the receipt-backed
   mutations, retirement, the routes and tools, the task's `epic` column
   and filter, the board's filter control. Ultra review (schema).
4. **Typed references.** The typed shape per kind, validation, the
   migration of stored strings, the renderer, the tool schemas and the
   OpenAPI document; the pre-1.0 break. Ultra review (schema and the
   retry receipts).

The identity directory ships with the first increment that needs it —
the board's assignee labels — which is 3 at the latest.

## Acceptance checks for implementation

- Upgrade storage holding tasks, historical snapshots and retry receipts
  with string references: every reference decodes typed afterwards, a
  valid HTTP(S) string became `url` and any other string became `text`
  with its exact text, nothing was rewritten to an inferred target. A
  legacy string-array write is refused with a message naming the new
  shape. Upgrade fixture: a create and an update committed before the
  upgrade with non-empty string references and a lost response — after
  the migration, the retry with the same key and the typed form of the
  same content replays the original result, and changed typed input
  conflicts. Ambiguous identities and unsafe clickable schemes are refused.
- Start a first session with `session_facts` absent or zero and verify that
  an enabled project gets its complete process bootstrap. Exercise disabled,
  offline, denied, missing, oversized and mixed-version process assets, plus
  missing skills; each yields the specified complete context or explicit
  availability notice without blocking session startup.
- Verify that ordinary credentials cannot enable or disable projects, that
  sync cannot overwrite enablement, and that disabling existing projects
  refuses task/comment/epic mutations while preserving authorized reads.
  Cross-project process reads must follow current task-read permissions;
  arbitrary group assets remain inaccessible.
- Reject non-admin selection changes and stale metadata revisions. Exercise
  interrupted Git fetches, missing commits, mixed-version files, private
  source access denial and exact-cache offline fallback with its warning.
  No process bodies are stored on the hub. Historical references survive
  selection changes and disablement; unavailable Git content is reported,
  never replaced by a different version.
- Verify migration enables existing task projects, including archived tasks,
  leaves new projects disabled and does not undo later admin choices.
  Sessions keep their tool list until restart, detect enablement changes
  during normal hub interactions and emit one restart notice; disabled
  writes fail immediately even before that notice.

- Race epic updates and retirement; stale revisions conflict. Refuse
  cross-project or missing epic assignments, new assignments to retired
  epics and ID reuse. Existing assignments and historical links still
  resolve after retirement; task snapshots and indexed epic filters agree.
- The identity directory returns exactly id, kind, name and enabled for
  every user and group, to any ordinary token, and nothing else; the gate
  matrix and a handler test pin it.

## Dogfood first: aimem itself

This repository is the first dogfood project, enabled by migration if it
already contains tasks. An admin selects the Git repository, commit and
manifest for its handbook, five-item DoR/DoD and task templates. The
repository is the private skills repository the installer already
fetches from, so every agent machine holds read access to it today; the
selection step confirms that access on each machine before the first
bootstrap is trusted. These
definitions are reviewed together in Git; the hub stores their reference
only. The roster exposes the two agent identities that work here. The
follow-up lists in the implementation plan become tasks under three epics:
agent enablement, the task page and the console access section. The release
that ships server enablement is the first whose own work ran through the board.

## Why this is worth building

Without per-project enablement, tasks are a hub-wide feature that most projects do not
want, and every agent on the hub sees tools it must not use. Without the
placement rule, the process text will exist in three copies within a
month. Without grouping and typed references, the board is a list of
cards with prose pointers, and the "evidence rules" stay a convention no
tool can check. None of the four increments is small any more, which is
why they are ordered and serial; together they are what turns a task
service into a process that agents can be trusted to run.
