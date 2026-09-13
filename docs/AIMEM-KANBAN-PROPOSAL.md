# Aimem Kanban Proposal

Status: **proposal, not implemented**, revised 2026-09-13 against v0.3.31,
source `207997a81ef3f4cf017b3ffcb573a770dcac3d4b`.

## Purpose

Add current work tracking to aimem so agents can discover active tasks, their
status, blockers, and next action without reading a long session chronology.

Kanban is a separate subsystem with its own task storage, MCP tools, HTTP API,
and later dashboard. Tasks belong to **existing aimem projects**. There is no
new project registry, and generic collections are not the task implementation.
Keep the initial design small and reuse aimem's existing infrastructure.

Proposed release target: **v0.4.0**, the next substantial pre-1.0 feature milestone.
Kanban and project-based access control belong to one development cycle, delivered
in small serial increments. This is a planning target, not a version bump or a
release authorization. Reserve v1.0.0 for an explicit stable-contract commitment
after the task APIs, permissions and upgrade behavior have proved reliable.

## Review Against Current Aimem

| Current behavior | Implication |
| --- | --- |
| Projects already have identities and separate SQLite databases | Reuse project ownership and partitioning |
| Collections are arbitrary JSON records, capped at 32 KB with 20 retained revisions | Do not use them as a task workflow/history database |
| MCP collection lists expose metadata, without typed task filters | Add task-specific list/get/update tools |
| Journal events capture turns, failures and compaction; retention can delete them | Task changes need their own history |
| SESSION-STATE is an authored local file, published and reconciled as a document | Link to tasks; do not render over the handoff |
| Named tokens currently have global writer/admin roles | Project-scoped task writes need the access-control extension |

Sources: [storage guide](STORAGE-GUIDE.md), [main design](DESIGN.md),
[collections](DESIGN-structured-docs.md),
[collection storage](../internal/store/collections.go),
[MCP](../internal/mcp/mcp.go), [event schema](../internal/schema/schema.go),
[tokens](../internal/server/tokens.go).

The current project's `tasks` collection returned no records in the 2026-09-13
MCP check. An empty generic collection is not an existing Kanban feature.

## Storage And Task Content

Use dedicated task tables in the existing per-project SQLite database. Separate
storage does not require a new server or a central task database. Start with:

- `tasks`: current task state and revision.
- `task_history`: accepted changes, actor, time, and resulting revision.

A task contains an immutable UUID, title, objective, acceptance criteria,
non-goals, state, optional assignee and blocker, dependency task IDs, current
candidate/evidence references, and next action. Server metadata supplies project,
revision, and creation/update times. Keep long reports in documents or CI/review
systems and link to them. Set explicit body/history limits before implementation;
never silently prune task history using the collection revision policy.

Writes use expected-revision CAS. Update task state and append its history entry
in the same transaction. On conflict, return current state for the writer to
re-read and reapply intent. Creation/retries need an idempotency key so a timeout
does not produce duplicate tasks or changes. Task history is independent of
session checkpointing and must be included in backup/restore.

One task has one owning project. Cross-project work links separate tasks; no
copies of a task on several boards. Dependency links are advisory initially;
strict cycle enforcement and atomic changes across projects are deferred.
Task assignment may reference a user or access group, but grants no permissions.

## States

Start with a small set:

| State | Meaning |
| --- | --- |
| `BACKLOG` | Accepted work not yet ready to start |
| `READY` | Scoped and ready to start |
| `IN_PROGRESS` | Implementation or verification underway |
| `REVIEW` | Awaiting or addressing review |
| `BLOCKED` | Cannot proceed; record reason and next action |
| `DONE` | Acceptance criteria completed with evidence |
| `CANCELLED` | Deliberately abandoned; retain the reason |

CI, human merge, release and deployment details belong in the next action,
blocker, and evidence fields initially; they do not each need a separate state.
No forced linear progression. State changes are explicit and audited. Reopening
is explicit too. Terminal tasks can be archived from active views while remaining
readable through their original links.

## Who Can Change Tasks

See [users, access groups and tokens](DESIGN-access-control.md).

- Admin has full access using the existing admin token obtained only directly
  from the aimem host console.
- A user/agent may change tasks in its working project when both current project
  assignment and its token allow it.
- Agents from other projects can read tasks and statuses, but cannot change them.
- Read access spans all projects on the authenticated hub, not other hubs.

Access groups simplify assigning users to existing projects. They are separate
from knowledge groups. Only admin manages users, access groups, assignments, and
ordinary tokens; admin tokens cannot be issued or revealed remotely.
Use aimem's existing HTTPS listener and token validation for both MCP and direct
HTTP clients; extend the authenticated identity with user/project permissions.

The hub enforces the same rule for every mutation through MCP, HTTP, CLI and UI,
including evidence, assignment and archive changes. A project argument or editable
configuration cannot grant access. Task assignees do not act as locks; authorized
same-project agents may update using CAS. No automatic curator-driven transitions.

## Direct References: MCP Or HTTP JSON

Agents with aimem MCP use task tools to get tasks and statuses. Any third-party
program or external agent without MCP can follow a normal web task reference and
receive JSON. Both paths expose the same task ID, revision and current state;
neither requires loading a dashboard or scraping a web page.

Proposed resources, **not existing routes**:

| Resource | Purpose |
| --- | --- |
| `GET /v1/projects/{project}/tasks` | Filtered, paginated task summaries |
| `GET /v1/tasks/{id}` | Current task JSON, including status and next action |
| `GET /v1/tasks/{id}/history` | Paginated change history |
| `/admin?task={id}` | Small human-readable task-only view |

A document can contain a direct link such as:

```markdown
[Backup task](https://hub.example.com/v1/tasks/01994700-0000-7000-8000-000000000001)
```

Task JSON includes objective, acceptance criteria, non-goals, assignment, blocker,
dependencies, candidate/evidence references, next action and storage metadata.
Return bounded current data with links to history and longer evidence. HTTP reads
use normal bearer authentication; errors must not masquerade as task JSON or
return a login page in place of data. Links contain no credentials and do not
grant access. An external client needs an authorized token, but no MCP or SDK.

Task URLs use immutable IDs so title changes, state transitions, and archiving
cannot break references. Resolve IDs from existing project partitions; any lookup
index is derived and rebuildable. Preserve reachability after project rename and
restore. Cross-project moves are deferred. Explicit project destruction can make
links unavailable and must not silently discard tasks. Host-address changes still
require an operator redirect for old absolute URLs.

An MCP agent uses `get_task(id)` against its configured hub. Never forward its
credentials to an arbitrary URL pasted into a document. Public documentation uses
placeholder origins; internal task URLs stay out of public PR text where required.

## Task Tools And Dashboard

Initial task MCP operations: `list_tasks`, `get_task`, `create_task`, `update_task`,
and `get_task_history`. `update_task` handles permitted field/state/archive changes
through one validated service with expected revision and idempotency key. Add more
specialized tools only when needed. Lists support state and assignee filters,
exclude archived tasks by default, and return small paginated summaries.

HTTP and CLI use the same task service and authorization rules. New routes and
schemas go into OpenAPI with route-parity tests. Older hubs report unsupported
capability; never silently fall back to generic collections.

The first UI can be a task list and task-only page with copy-link actions. Later,
add a Kanban board using the existing project selector and the same list/update
API. Dragging a card is an ordinary CAS state change; show conflicts instead of
silently overwriting. There is no separate dashboard database.

## Evidence And Repository Rules

Record the current source candidate with full head/base SHAs and PR reference
where applicable. Evidence references identify the command/result, CI run, review,
or deployed artifact and the source it applies to. Task entries record observed
claims; Git, CI, reviews and runtime checks remain authoritative.

Changing the candidate or agreed acceptance criteria requires reassessing evidence
and readiness. A matching head SHA alone may not suffice after base or policy
changes. Keep old evidence identifiable as historical. Start with explicit agent
checks; automated freshness/gate integrations can follow real use.

Findings may be blockers, follow-ups, rejected, or explicitly accepted risks.
A follow-up links an independently scoped task. Labeling it a follow-up does not
waive a repository review finding: preserve required user waivers and review gates.
A task state never authorizes a merge or deployment. Keep scope changes deliberate.

## Handoff And Delivery

SESSION-STATE remains the operational handoff during adoption and links active
tasks rather than duplicating the board. Keep journal chronology and durable
knowledge in their existing stores. Do not replace the handoff with generated
output or introduce a second document-publishing path.

Initial task authority is the owning hub. Existing journal/memory sync does not
replicate tasks. When offline, record pending intent and last-observed revision in
the local handoff, then re-read and reconcile after reconnecting. No offline task
queue or cross-hub task protocol in v1.

Deliver serial increments:

1. Minimal users/groups/project assignments and tokens on the existing HTTPS/auth
   foundation, with admin-only management and permission tests.
2. Task storage/service with migrations and CAS history, followed by MCP and HTTP
   task operations and stable JSON references; trial real project tasks.
3. Task list/detail page, followed by the board on the same API.

Verify project write boundaries and admin access, concurrent writes/retries,
history atomicity, HTTP/MCP parity, direct links after archive/rename/restore,
and resumption from a linked task. Specify safe project removal and backup behavior
before shipping storage changes. Use the repository's sensitive-surface review
gate for auth/schema changes.

Defer a general workflow engine, custom role engine, task-specific service-account
hierarchy, group-owned boards, cross-hub replication, historical metrics, automatic
assignment, and mass migration. No implementation or live account/task/token
creation is part of this proposal edit.
