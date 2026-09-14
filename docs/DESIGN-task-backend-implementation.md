# Task Backend Implementation Plan

Status: implementation handoff, 2026-09-14. No task implementation is merged or
exposed. Baseline: `4e7021533a1aefb2635e28a071953f2061f1df5b` on `master`.
Read this alongside [the Kanban proposal](AIMEM-KANBAN-PROPOSAL.md) and
[access control](DESIGN-access-control.md). Those documents contain the approved
product contract; this document gives the next agent an implementation sequence
and explicit choices. Record corrections in these documents rather than silently
changing the contract.

## Resume State

Stage 1 (storage and integrity) is implemented on branch `feat/task-storage`
as `internal/store/tasks.go` with schema 11 (`store.go`), lifecycle refusals
in `Registry.Drop`/`MergeProject`, and `internal/store/tasks_test.go`. The
earlier untested draft (`docs/drafts/task-storage.go.txt`) is superseded and
removed. Nothing is exposed yet: no HTTP route, MCP tool, CLI command or UI
reads or writes tasks; the service layer is stage 2.

Decisions taken while implementing stage 1 (corrections to the draft):

- No task mutex. A project database already serializes writers (one
  connection, immediate transactions), so a task mutation owns the database
  for its transaction. The draft's `DB.taskMu` was unnecessary.
- Lifecycle coordination lives at the registry lock. `Drop` and the final
  step of `MergeProject` evict and close the cached handle, then check for
  tasks through a fresh connection **under the same lock** before removing
  the directory. `database/sql.Close` does NOT end a transaction already in
  flight on the closed handle, so the check takes an IMMEDIATE transaction
  of its own: SQLite makes it wait for that writer to commit or roll back,
  and a committed task is seen. New writers cannot start (closed handle;
  reopen needs the lock). Proven by deterministic tests that hold a write
  transaction open across the drop and across the merge's final step.
  `MergeProject` also refuses early, before copying, like docs/collections.
  Known limitation (stage-2 follow-up): a task created during the copy is
  caught only by the late check, after the history was folded into the
  target and group citations were relabelled; the source is kept intact
  and a re-run is refused until the task is gone. Stage 2 should add a
  registry-level "merging" guard so the source refuses task writes for the
  duration, then the late check becomes a pure safety net.
- The warning tier of the authored-secret scan is not computed here.
  Storage refuses the high-confidence tier only; the service computes and
  transports warnings from the same scan, so signatures stay `(T, error)`.
- Receipts are keyed by `(actor, operation, scope, key)` where scope is the
  task ID (empty for create); the canonical JSON of the input is hashed;
  replay returns the original result even after the task advanced; a
  changed input is `ErrTaskRetryConflict`; a failed attempt writes nothing.
- Updates replace all editable fields and require an explicit state (only
  creation defaults to BACKLOG); optional fields omitted are cleared.
- Reserved stores (user, `group-*`) receive the tables (uniform schema) but
  every task write there is refused (`ErrTaskReservedScope`).

## Scope And Delivery Boundaries

Keep three serial delivery stages, each branched from the previous merged result:

1. **Storage and integrity.** Dedicated task/history/comment tables, task CAS,
   idempotent writes, bounded reads, immutable comments, migration and safe project
   lifecycle behavior. Internal Go APIs only; no credential reaches a new route.
2. **Authorized HTTP and MCP.** One service boundary for project resolution,
   current authorization and actor stamping; task and comment JSON routes,
   explicit MCP tools and ordinary-token routing. Exercise a compiled service
   end to end with ordinary and admin credentials.
3. **Task UI.** Task list/detail/discussion first, Kanban board afterwards, all
   using the same service. UI implementation is outside the first two stages.

The user has authorized implementation, not release, deployment, automatic merge,
or an unlimited redesign. v0.4.0 remains a proposed milestone. Follow the current
AGENTS.md review gates and serial-PR rule. External review preference is
`review-this` or `review-this:codex-astra`; resolve available labels at request time.

## Existing Code To Reuse

| Boundary | Existing implementation | Constraint |
| --- | --- | --- |
| Project databases | `internal/store/store.go`, `Registry`, `DB` | One `journal.db` per existing project; schema version 11 (tasks added in stage 1) |
| Project identity | `internal/store/access.go` | Host-local `access-id` survives rename; never replace it from writable meta |
| Access decisions | `internal/access/store.go`, `Authorize` | Match token project instance and current direct/group grants |
| Server auth/store ownership | `internal/server/tokens.go`, `access.go` | Shared lazy access handle; never cache identity/grant decisions |
| Routes | `internal/server/server.go`, `openapi.json` | Route parity and explicit ordinary-token allow-list |
| MCP | `internal/mcp/mcp.go`, `cmd/aimem/main.go` | HTTP MCP currently forwards legacy operations through a trusted local client |
| Authored secret handling | `internal/redact/redact.go`, `store/docs.go` | Refuse high-confidence secret shapes; never silently alter authored text |
| Ordering/IDs | `internal/uuidv7` | Generate UUIDs server-side; use a DB append sequence for comment pagination |

Avoid generic collections, checkpoint events as the task authority, a second
project registry, nested access groups or a new authentication proxy.

## Storage Contract

Use dedicated tables in each existing ordinary project's SQLite database. Do not
permit tasks in reserved user/knowledge-group stores. Keep task data out of
journal/memory sync and retention. Proposed schema 11 is additive and transactional:

| Table | Contents and constraints |
| --- | --- |
| `tasks` | Immutable UUID primary key, current content/revision/timestamps, indexed state/archive/assignee filters |
| `task_history` | Task ID, monotonically increasing task revision, complete resulting snapshot and authenticated actor; unique `(task_id, revision)` |
| `task_comments` | Immutable UUID, parent task FK, Markdown body, actor, creation time, monotonically increasing database append sequence |
| `task_requests` | Retry receipt keyed by actor/operation/scope/key, canonical request hash, original response; committed with its mutation |

Stage 1 stores typed snapshots as JSON plus a few indexed filter columns
(state, archived, assignee). All copies are updated in one transaction, with a
test proving the filter columns, the stored snapshot and the returned task agree. No schema change is needed in `access.db`.

A task has title, objective, acceptance criteria, non-goals, state, optional
assignee, blocker, dependency task IDs, candidate/evidence references, next action,
archive flag, revision and creation/update times. Resolve owning project from its
partition at read time; do not bake the mutable project name into stored snapshots
or replay receipts. Task and comment UUIDs remain stable through project rename.

Use the seven approved states: `BACKLOG`, `READY`, `IN_PROGRESS`, `REVIEW`,
`BLOCKED`, `DONE`, `CANCELLED`. Default creation to BACKLOG. No forced transition
graph or automatic transitions. Only DONE/CANCELLED may be archived. Reopening
requires an explicit state update; unarchive before adding discussion to an
archived task. An unarchived terminal task may still receive comments.

An assignee is `{kind: user|group, id: <access identity UUID>}`. It conveys no
authority. Storage validates its shape; the service should validate that the
referenced user/access group exists. Disabled historical assignees remain readable.
Dependencies are advisory task UUIDs; no automatic scheduling or cross-project
transaction. Cycle enforcement is deferred.

### Input Bounds

- Title: nonblank valid UTF-8, at most 256 bytes.
- Editable task content: at most 32 KiB of canonical JSON in this first storage
  design. Each text field must also be valid UTF-8. Long reports belong in linked
  documents, not task fields.
- Comment: nonblank valid UTF-8 Markdown, at most 32 KiB of decoded body bytes.
- Dependencies and each reference list: at most 32 entries; reference strings at
  most 2,048 bytes each. Never fetch links as part of a write.
- Idempotency key: nonblank, at most 128 bytes. Do not allow secret-bearing keys.
- List/history/comment pages: default 20, maximum 100. Reject invalid cursors and
  limits. Do not put whole discussions/history into task summaries.

Validate decoded text before storage; JSON escaping must not bypass secret checks.
Reject unsupported control characters. Preserve content exactly rather than
redacting it silently. Use the existing authored-content warning/refusal tiers:
storage refuses the high-confidence tier; the service computes the warning tier
and includes warnings in transport results without leaking matched secret values.
Actor display names are metadata too: bound and validate them before persistence.

## Task Writes And Retry Semantics

Creation generates the task UUID and revision 1 and writes its first history
entry. Initially use full replacement of editable fields for updates: required
fields stay required; omitted optional fields clear. Document that contract in
HTTP/MCP schemas, rather than accidentally implementing ambiguous partial updates.

For an update, require `expected_revision`. Within the existing immediate SQLite
transaction, read current state, compare revision, write revision+1 and append
history. Return a typed conflict containing current state if stale. Never copy the
document store's relaxed "identical body succeeds despite stale revision" behavior
into tasks: retries are handled explicitly by receipts.

Require a key on create, update and comment append. Scope it to authenticated
principal, operation, and task ID (empty scope for create; the receipt table
already lives in the project's own database, so the project is implicit). For ordinary
agents, identify the principal with stable user/token IDs; display-name changes
must not change that key. Admin actor identity must come from the trusted auth
layer, never a body field. Different ordinary tokens are distinct retry scopes;
retrying through a newly issued token does not promise deduplication.

Hash the normalized request, including expected revision for task updates. An
identical retry returns the original result even if the task has since advanced.
Reuse with different input returns a conflict. Write receipt, task and history
atomically: injected failure in any one must roll back all of them. Failed writes
do not consume keys. Keep receipts with task data so restart/restore does not
silently permit duplicates. Do not prune history/comments/receipts automatically.

For a committed comment retry, return the original comment after fresh
authorization, even if the task was subsequently archived. A new append to an
archived task fails. Check existence/archive state and insertion in one project
transaction so an append cannot race archival. Comments do not change the task
revision or produce duplicated automatic-history bodies. Concurrent independent
appends must both succeed and receive stable sequence ordering.

## Actors And Authorization

Storage does not authenticate a supplied Go struct. Its internal entry points
must explicitly require a trusted actor; only the service constructs that actor
from validated credentials. Capture stable user/token IDs plus a display-name
snapshot and server time. Do not accept author, creation time or revision metadata
from HTTP/MCP callers.

| Caller | Read task/history/comments | Create/update/archive/comment |
| --- | --- | --- |
| Admin | All ordinary projects | All ordinary projects, subject to CAS/integrity |
| Ordinary valid token | All ordinary projects on its hub | Only if token's project instance matches and user has a current grant |
| Read-only/cross-project ordinary token | Yes | No |
| Disabled/revoked/expired/invalid credential | No | No |
| Legacy writer token | All ordinary projects, read only | No automatic task-write authority; require an ordinary project token or admin |

Re-run authorization for every attempt, including receipt replay; a receipt never
grants permission. Task assignment, client project parameters and local working
directory do not authorize writes. Resolve the task's actual owning project
before checking the project grant. Preserve the host-console-only admin token
mechanism and keep ordinary tokens out of unrelated legacy APIs.

## Project Lifecycle And Backup: Required Before Storage Ships

Rename must retain task IDs, history, comments, receipts and access-id. Fetching an
ID after rename must identify the new project name. A task lookup should inspect
only existing ordinary project databases; do not use a read path that creates a
missing `journal.db` in an orphan directory. The current `OpenExisting` checks a
directory, so inspect that assumption before reusing it for global task lookup.

For the first storage increment, **refuse project Drop and source-project Merge
when the source contains any tasks, including archived tasks**. No task delete or
cross-project move exists yet. Do not silently discard discussion/history, and do
not imply the existing event export backs them up. A target containing tasks may
receive legacy non-task data only if the merge leaves its task identity/data intact.
Later explicit task-preserving export/removal can relax the refusal in a separate
change. Manual host filesystem destruction remains an operator action outside the
task API and necessarily invalidates links.

The refusal must be race-safe against task creation. Plan locking before coding:
one registry/project lifecycle boundary must cover resolution, task operations,
rename/drop/merge, and closing cached handles. Document one lock order and avoid
reentering `Registry.Open` while holding its non-reentrant mutex. An in-process
task mutex is not a solution on its own. Test concurrent create versus
drop/merge (stage 1 does: see Resume State). An in-process mutex does not coordinate independent registries or
processes; task mutations should enter the owning service, and offline maintenance
must stop it. Do not claim cross-process safety from a Go mutex.

Back up the owning project database consistently, including WAL state using an
appropriate SQLite backup/checkpoint procedure, plus project `access-id` and hub
`access.db`/existing credential configuration as required by the backup design.
The simplest documented operator procedure is stop service, copy the complete
state tree, restore it to a stopped instance, then restart. Do not copy only a live
`journal.db` or use event JSON export as a task backup. Test close/copy/reopen
restore for tasks, comments, history, receipts and stable IDs. Never add task
replication to existing journal/knowledge sync as part of this work.

## HTTP Contract For The Second Increment

These routes are proposed and must not be advertised as implemented yet:

| Method and path | Behavior |
| --- | --- |
| `GET /v1/projects/{project}/tasks` | Paginated task summaries; state/assignee filters, archived excluded by default |
| `POST /v1/projects/{project}/tasks` | Create in an existing ordinary project |
| `GET /v1/tasks/{id}` | Current task plus project and reference links |
| `PUT /v1/tasks/{id}` | Full editable-field replacement with expected revision |
| `GET /v1/tasks/{id}/history` | Paginated snapshots/actors in revision order |
| `GET /v1/tasks/{id}/comments` | Paginated comments in append order |
| `POST /v1/tasks/{id}/comments` | Append body; author/time supplied by service |
| `GET /v1/tasks/{id}/comments/{comment_id}` | One immutable comment; wrong parent returns not found |

Use one explicit idempotency-key transport contract: HTTP `Idempotency-Key` header,
MCP `idempotency_key` argument translated by the client/service. Strictly decode
bodies, reject unknown fields and extra JSON values, and bound raw requests above
decoded field limits so legal escaping is supported without unbounded input.
Specify 400 validation, 401 invalid auth, 403 denied writes, 404 missing resource,
409 revision/key/archive conflicts and 500 storage faults. Do not return internal
paths, credentials or HTML login pages as API errors. Revision conflicts include
current task data only after read authorization.

Use stable task/comment IDs in links. Prefer a configured canonical origin; if it
is unavailable, return an origin-relative resource path that a configured client
can resolve. Do not invent URLs from untrusted Host/forwarded headers. Links never
contain tokens or grant access. A derived ID-to-project index may be added later;
first implement a correct existing-project scan with explicit unavailable/error
behavior and tests for rename. Never create a second authoritative task registry.

Add actual routes and schemas together with OpenAPI parity coverage. Ordinary
tokens currently have an exact identity-route allow-list in `authWrapper`;
extend only the new task paths and recheck authorization inside their service.
Do not broadly admit `/v1/projects/*` or arbitrary `/mcp` tool calls.

## MCP Integration And The Critical Trust Boundary

Expose `list_tasks`, `get_task`, `create_task`, `update_task`, `get_task_history`,
`list_task_comments`, `get_task_comment`, `add_task_comment`. Use typed tool
schemas and structured JSON results with bounded pages, current revisions and
reference links. The model supplies IDs/content/keys, never credentials or actors.

Remote MCP currently uses `NewHTTPHandler(api)` with a shared client to the
trusted local HTTP socket. That path does **not** propagate the remote caller's
authority today. Simply allowing ordinary tokens through `/mcp` would expose a
privilege bypass through legacy tools. Fix the boundary explicitly: dispatch new
task tools into the same task service with the request's authenticated context,
or use an equivalent transport that carries and revalidates the caller's bearer
credential. Do not trust client-set identity headers on a local socket.

For an ordinary remote caller, list and dispatch only permitted task tools;
direct JSON-RPC calls to hidden legacy tools must still be rejected. Preserve
admin/legacy behavior for existing tools. Do not cache auth context across MCP
requests or rely on authentication of an earlier initialize call after revocation.

Local stdio MCP must use the project's configured ordinary hub credential for
task operations, through the configured hub. It cannot inherit unrestricted
task authority from the local socket or a fallback shared admin credential.
Determine and document the concrete credential setting before implementing this
transport; there is no approved secret-in-tool-arguments shortcut. Missing task
credentials should return an actionable configuration error. Never forward a
configured bearer to a URL supplied in task content, and do not silently downgrade
to generic collections on an older hub or when offline.

## Verification And Acceptance

### Storage Stage

Prove migration from schema 10 preserves old journal/docs/collections; new/opened
databases acquire task tables atomically; newer schemas are rejected. Test:

- Creation/default state, required/optional bounds, valid states and assignees.
- Two competing CAS updates: exactly one succeeds, loser receives current task;
  each accepted revision has exactly one matching actor-stamped history row.
- Failure injection rolls back current task, history and receipt together.
- Same-key create/update/comment retries after reopen return original IDs/results;
  changed input conflicts; same key in different actor/operation/task scopes works.
- Concurrent comment appends, stable pagination while later comments arrive,
  no task revision bump, wrong-parent lookup, archive/new-append/refusal/replay.
- Secret refusal, UTF-8, control characters, exact size boundaries and limits.
- State/assignee/archive filtering, summary bounds, history retained beyond 20
  revisions, no pruning by checkpoint retention or collection cleanup.
- Rename/restore preserve task/comment references and retry receipts; project
  drop/source merge refuse task-bearing projects, including concurrent creation.

### Service And Transport Stage

Exercise real middleware/routes with real temporary stores: ordinary own-project
writes, cross-project reads and refused writes/comments, read-only tokens, admin,
disabled users, grant removal, expiry, revocation and retry-after-revocation.
Client actor spoofing, project-name reuse, arbitrary MCP legacy-tool invocation,
and local-socket credential bypass must fail. Run the same task/comment lifecycle
via HTTP and MCP and compare results. Include direct comment links and rename.

Run a compiled isolated service as evidence: create task, update with CAS, append
comment, retry each operation, follow JSON references, observe conflict and denied
writes, remove a grant/revoke a credential and verify immediate enforcement. Use
temporary state and synthetic secrets; no live accounts/tasks or deployment.

For each code PR run the repository build/full tests/gofmt/staticcheck and review
gates. Storage/authentication changes require the ultra pre-merge gate, actual CI
and a posted exact-head review; medium review precedes every push. Use the current
delivery-first dispositions: only blockers reopen frozen implementation scope;
follow-ups become separate work. The user authorizes merge separately.

## Draft Review Checklist (resolved by stage 1)

The checklist that gated the removed draft, kept for the record; items 1, 2, 4
and 7 are done in `tasks.go`/`tasks_test.go`, the refusal half of 6 too; the
warning tier of 6 and items 3 and 5 belong to the stage-2 service:

1. Wire schema 11 and lifecycle protection, or revise the approach; the draft
   cannot compile because `DB.taskMu` does not exist.
2. Decide lifecycle coordination across Registry/DB entry points before adding a
   mutex. A check-then-close guard without task-write coordination is insufficient.
3. Validate assignee filters and actor-name/control rules as well as write content.
   The draft validates assignee content shape but not its existence.
4. Confirm retry scope/normalization, receipt growth and restart/restore tests.
   Receipt replay returns original task snapshots, intentionally not latest state.
5. Ensure task APIs return owning project/link metadata dynamically after rename.
   The draft has only per-DB lookup, no global resolver or service authorization.
6. Check every input against raw and decoded byte limits and authored-secret tiers;
   the draft does not transport warning-tier results.
7. Add meaningful migration, transaction-failure, concurrency and lifecycle tests
   before considering any draft code an implementation milestone.

Stage 1 is on `feat/task-storage`. Do not publish new MCP definitions without
enforcement or change the public version. Resume with stage 2 (the authorized
service) from the merged storage stage, and preserve the approved simple scope.
