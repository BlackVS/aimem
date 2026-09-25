# AIForge proposal: split aimem and aicrew

Status: **Architecture boundary for review**. No implementation or removal approved yet.
Updated: 2026-09-25. Source baseline: aimem v0.7.3 (`2c23ba0`).
The Q1-Q5 directions below are agreed with the operator; detailed protocols remain separate review gates.

Keep aimem as the knowledge service and project backlog. Build aicrew as a
separate service for coordinating agents that work on that backlog.

**AIForge** is the umbrella for three separate projects: **aimem** (knowledge
and project backlog), **aicrew** (agent coordination and execution), and
**ai-skills** (shared skills and processes). The umbrella is a name and shared
documentation structure, not another service or authentication layer.

This supersedes the earlier combined redesign draft. Existing behavior stays
supported until its replacement and migration work.

## Agreed direction (Q1-Q5)

1. **Service boundary:** aimem remains the standalone knowledge service and project backlog; aicrew owns team coordination. Agents use each service directly.
2. **Identity and access:** aimem owns stable named identities and resource grants; aicrew links members by verified IDs. A team is an aimem access profile, while each action retains its individual actor.
3. **Session context:** a session selects personal or team access. Team grants replace personal grants for that context, and the server verifies the binding. Onboarding manages an individual credential without a shared team bearer or manual credential switching.
4. **Work ownership:** aimem owns one atomic task reservation shared by standalone and aicrew claims. Aicrew owns attempts and scheduling; assigned workers cannot bypass their reservation through direct task tools. Denials explain the active context and the permitted next action.
5. **Delivery:** start with one coordinator, one worker and one aimem project after reviewed contracts and prerequisites. Aicrew has its own repository and aimem board; aimem changes use ordinary feature PRs. General aimem work and legacy-team cleanup remain paused.

These decisions fix ownership and behavior, not endpoint shapes, token issuance, storage transactions, migration, or release timing. Each detailed contract needs its own review and evidence before implementation.

## What each project does

| aimem | aicrew |
| --- | --- |
| Journals, memories, curation and recall | Agent onboarding and identities |
| Wiki, documents and structured records | Teams, roles and project access for members |
| Knowledge sharing and synchronization | Assignments, attempts and recovery |
| Project tasks, epics, priorities and dependencies | Messaging, wake-up and supervision |
| Task status, comments, history and process selection | Coordinating reviews and recording execution evidence |
| Atomic task reservation | Worker selection and execution scheduling |

Aimem works alone: a person or standalone agent can use knowledge and project
tasks without joining a team. Aicrew does not keep a second backlog or KB.

**Terms:**
* _KB_ means the durable knowledge as a whole.
* _Memories_ are small sourced facts;
* _journals_ are session events;
* _docs_ are versioned prose;
* _records_ are structured entries.
* _Wiki_ is a view of docs/records, not another store.

Knowledge groups share knowledge across projects; teams organize agents.
A project owns its knowledge and tasks. A review assesses a specific result;
it is not a permanent team role. The project process defines review gates.

## What changes from today

| Today | Proposed |
| --- | --- |
| aimem contains knowledge, tasks and team coordination | aimem keeps knowledge/tasks; aicrew owns coordination |
| Teams belong to one project | aicrew teams can work across connected projects |
| Operators assemble users, grants, tokens and enrollment | Invite an agent once; onboarding configures its identity |
| Team state is tied to checkout context | Agent identity is separate from model and workspace |
| Task and coordination state share a database | Two services cooperate through a tested reservation API |

The last change is the main risk: failures between services must not create
conflicting ownership or lose task updates.

## How they connect

```text
Agent -- aimem tools --> knowledge and project tasks
      -- aicrew tools --> team, assignments and messages
```

Aimem owns named identities and resource access profiles. Aicrew owns teams,
membership and roles, linking members to verified stable aimem hub/user IDs.
Readable names appear in assignments and comments; rotation or renaming must
not break identity. A team is an ordinary service access profile to aimem,
not a second team-membership system inside it.

Each session selects personal or team context. Team grants replace personal
grants rather than combining them; individual actor attribution is retained.
The context is session-local, not host-global. Leaving restores personal
context only after outstanding work is reconciled. The server verifies the
binding; a request field alone cannot authorize it.

Onboarding links the existing aimem identity or creates one through an
authorized flow. Team-session credentials are managed automatically, with no
shared team bearer or manual token switching. Aicrew does not proxy or duplicate
aimem's knowledge interfaces. Exact issuance and verification protocols remain
design work. Backend task integration may use a bounded service credential;
it cannot replace verified individual context with arbitrary actor labels.

**Current gap:** ordinary aimem task tokens cannot access the legacy knowledge
API. Define a small scoped extension before claiming full integration. Do not
hide a broad fallback token or ask each worker to configure a second credential.

Aimem owns the task and its reservation. Aicrew owns execution attempts and
messages. Standalone and aicrew claims must contend on the same atomic
reservation. Requests use revisions, retry keys and ownership fences. After a
lost reply, aicrew reconciles the result before proceeding. Disconnection must
not silently release running work. Review and human-merge gates still apply.

An assigned worker cannot bypass coordination by claiming through direct aimem
tools. Refusals return structured reasons, active context and permitted next
actions: check the aicrew inbox or reconcile an unexpected context, never retry
with personal credentials. Independent workers use the same reservations.

Dependencies reference task IDs across projects on the same hub. Ordinary
pickup checks their state; storing a dependency is not automatic scheduling.
Missing or inaccessible dependency evidence must not be treated as DONE.

## Aicrew roles and onboarding

- **Coordinator:** plans, assigns and reviews.
- **Worker:** waits for an assignment.
- **Independent worker:** claims eligible tasks directly, respecting reservations.

Every aicrew agent belongs to a team, including a one-member team. This rule
does not apply to standalone aimem users. Team roles do not grant administration.

The proposed hub CLI will create a single-use invitation for a team and role. The client
runs a bootstrap command, enters the invitation privately, and receives its
agent credential and setup. No hub-to-client SSH or manual token-file transfer.
Claude Code and OpenCode are the initial automation targets; their existing
provider login stays separate and unchanged. Web management comes later.

### How-to note: switching from personal to team work

Prefer a **fresh conversation with the team context active** when switching
from personal to team work. The main reason is context quality: unrelated
instructions, assumptions and task history can distract the agent or conflict
with its team role, even when it is authorized to access both sets of data.
Preserve the personal session separately so it can be resumed later.

Do not carry over the entire personal transcript or a broad summary by default.
Bring across only relevant facts and sources as a deliberate handoff. Switching
the access context affects subsequent retrieval; it does not clear the model's
existing conversation. A fresh conversation is a working practice for keeping
context focused, not a substitute for access controls or a security guarantee.
This guidance describes the proposed flow, not an automatic reset performed by
today's join commands.

## Steps before starting aicrew implementation

### Onboarding workspace and dependencies

An agent starts in its own home, not a project repository. Use one consistent
layout: `agent.json` for nonsecret configuration, `creds/` for credential
material only, `docs/` for agent guidance/handoff, `state/` for recovery data,
`logs/` for redacted logs, `work/` for temporary artifacts, `repos/<repo>/` for
repositories and `worktrees/<attempt>/` for isolated implementation. Repository
instructions remain authoritative for repository work; do not duplicate them
in agent-home docs. Names must distinguish repositories with equal short names.

Credential references use readable service/account/purpose names, independent
of the model. Never store secrets in configuration, repositories or worktrees.
Use owner-only storage with appropriate OS protection. Provider logins remain
in the client's supported storage. This layout does not isolate agents running
as the same OS user. Define rotation, expiry, backup and cleanup conventions.

The proposed onboarding one-liner will create this structure and initial managed
guidance, then check aimem, aicrew and ai-skills against a supported version
set. Show planned changes first; install missing components or safely upgrade
incompatible ones using supported, verified distributions. Do not upgrade to
latest unnecessarily. Where automatic changes are unavailable or unsafe,
provide exact manual steps. Preserve credentials, repositories, personal notes
and unrelated client/provider configuration on reruns; report managed-file
conflicts rather than overwrite them silently.

Verify actual versions, required capabilities and client integration afterward.
Report **ready**, **restart required**, or **blocked with instructions**;
installer success alone is insufficient. Workspace/credential conventions are
a prerequisite contract within onboarding, not a second installer system.

1. **Approve this boundary.** Agree what stays in aimem and the integration trust model.
2. **Inventory and align the backlog.** Classify existing team code, APIs, data,
   commands and tests as reuse, replace or retire. Hold the old combined redesign.
3. **Specify and prove task reservations.** Implement the smallest aimem API
   with a fake consumer; test competing claims, crashes, retries, revocation
   and completion. Preserve current managed-task protection meanwhile.
4. **Specify knowledge access.** Cover memories, docs, records, journals,
   shared groups and sync without another per-agent credential. Preserve
   standalone behavior and make permission differences explicit.
5. **Plan compatibility and migration.** Decide how old clients and team
   history remain usable, and how to recover a failed migration.
6. **Prepare the aicrew repository.** Confirm licensing for reused code,
   API compatibility and the first vertical-slice acceptance tests.

After these steps, start with one coordinator, one worker and one aimem
project: onboard, reserve, assign, execute, review and complete a task.
A task-only development slice can precede knowledge implementation, but full
onboarding is not ready until knowledge access also works.

## What we cut from aimem, and when

Eventually remove team enrollment/sessions, assignment execution, team inbox,
team runtime commands and supervisor integrations from aimem. Keep simple tasks,
project access management, process selection and all knowledge features.

General aimem development and cleanup are paused until aicrew is functional;
only features required by aicrew proceed. Legacy team code is useful as a Git
reference, not a reason to maintain two active coordination systems. Before
eventual removal, verify active work, preserve needed history and review data
and client compatibility. No blind module deletion or schema drop.
Reuse proven contracts and tests where useful; starting a new project does
not require rewriting everything.

Aimem prerequisites use normal feature PRs into master. Aicrew gets a separate
GitHub repository and a separate aimem project/board; the earlier long-lived
aimem redesign branch is no longer needed. Cross-project task dependencies
link prerequisites without duplicating the backlog. Production releases and
migration remain separate operator decisions.

## Shared ai-skills on GitHub (later)

Host ai-skills on GitHub so aimem, aicrew and other GitHub projects can link to
and install the same reviewed skills and process definitions. Keep one canonical
source; do not fork a separate skill collection into each project. Select the
canonical repository or an explicitly maintained mirror when publishing.

Pin consumed assets to an immutable commit/version. Check license and private
content before publication, and preserve or migrate existing references
explicitly. GitHub hosting is useful shared infrastructure, not a prerequisite
for designing or prototyping aicrew.

## Ready for production when

Aimem still works independently; both usage modes share one authoritative task
backlog; faults cannot produce two task owners; onboarding needs one agent
credential; knowledge access respects grants; real clients receive messages
without operator relay; and migration/restore have been demonstrated.

Source basis: `internal/server/tasks.go`, `internal/access/store.go`,
`internal/store/tasks.go`, `internal/store/team*`, `internal/teamstate`,
`internal/server/docs.go`, `internal/server/collections.go`, and `docs/DESIGN.md`.
The detailed earlier review remains supporting analysis, not a competing plan.
