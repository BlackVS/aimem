# Assignment storage foundation

Schema 17 adds durable offers and execution reservations. This increment exposes
Go storage methods and a read-only task projection. The subsequent
[HTTP increment](TEAM-ASSIGNMENT-HTTP.md) exposes these primitives; a usable
CLI/MCP execution workflow is still deferred. Do not start a worker
pilot from these primitives. [Result storage](TEAM-RESULT-STORAGE.md) adds internal
submission and disposition methods. [Cooperative work control](TEAM-WORK-CONTROL-STORAGE.md)
adds internal block/resume and acknowledged stopping. [Operator recovery storage](TEAM-RECOVERY-STORAGE.md)
adds reconciled forced closure; client exposure remains deferred.

`OfferTeamAssignment` requires the current coordinator's session and coordinator
generation, a READY task at the expected revision, same-project DONE dependencies,
and an available, enrolled worker at its current session generation. A required
service callback checks that worker's live token, user, scope and write grant.
The callback must not re-enter the project database. The service authenticates
the caller's live ordinary write token on every call, including receipt replay.
Storage does not authenticate tokens held in the separate access database.

An offer reserves both the task and one capacity slot for the worker session.
Partial unique indexes enforce one reserved attempt per task across teams and
one per worker session. Multiple sessions of the same access user are separate
workers. Transactional checks enforce permanent management by the first team.
There is no worker self-assignment operation.

Each offer captures the task content and revision, worker profile and its revision,
model/source declarations, coordinator generation, suitability rationale and cost
rationale. The coordinator records complexity, required capabilities and model fit
in the suitability rationale. Unknown suitability needs clarification before an
offer; using a stronger model needs a reason. These are process assessments, not
server attestations of competence. Later profile edits do not rewrite an offer.

The intended worker can accept an OFFERED attempt. Acceptance rechecks readiness,
dependencies, current session, availability, enrollment and live worker authority,
then records RUNNING and task IN_PROGRESS in one transaction with task history,
audit and retry receipt. It preserves the task's access-user/group assignee.
The worker may decline an OFFERED attempt with a reason; the current coordinator
may withdraw it with a reason. Either closes the reservation. Accept, decline and
withdraw races have one winner. Retrying an accepted command with the same key
and content returns its original result after current caller authorization; it
does not execute again. A different request under that key conflicts.

First offer permanently marks the task as managed, even between attempts.
Generic task PUT/archive returns `409` with `managed_task` in the error and a
pointer to coordination operations, including for admins and old update receipts.
Comments remain discussion under their existing authorization. Dedicated edit,
override and unmanage operations are deferred; there is no generic-write escape.
Unmanaged tasks retain their existing behavior.

Task GET and list responses derive an optional `coordination` object from current
coordination rows: `team_id`, plus `attempt_id` and `state` when reserved. These
fields are not editable task content and are not copied into task history.
Standalone clients must skip all tasks carrying this object, including READY
tasks with no current attempt. Dedicated board rendering is deferred.

Heartbeat age never releases a reservation. Resume or leave does not release
RUNNING work; closing or fencing a recovery attempt cannot stop a local
process. A resumed worker cannot accept an old-generation offer: the coordinator
must withdraw and issue a new offer. Result submission/disposition and cooperative
stopping and operator recovery have their own storage methods. Coordinator handoff
remains deferred; no storage method can stop a local process.

## Session rebinding

A worker `resume` carries the session's reserved RUNNING, BLOCKED,
STOP_REQUESTED, STOPPED or SUBMITTED attempt to the new session generation in
the same transaction as the session change: the assignment's worker handle
becomes the handle resume returned, a rebind record (previous generation, new
generation, time) is appended, `rebind_count` increases and a correlated
`team.assignment.rebind` audit event commits with the session event and retry
receipt. The worker identity, offer, profile and result snapshots, task content,
task revision and history do not change; the reservation is neither released nor
re-executed. An OFFERED attempt stays bound to the generation that received it.
Coordinator resume, heartbeat, profile and leave never rebind; a departed
worker's attempt keeps its last handle for operator recovery, which must name
that handle and the separately observed session generation.

After rebinding, the pre-resume handle is stale for work commands, submission
and reads; the returned handle commands the same attempt. Rebinding records
hub-side ownership only: the returning client must still reconcile any command
that survived locally before continuing. `ReservedTeamAssignment` returns the
reserved attempt bound to the calling current session in the same snapshot, or
not-found when none, so a resumed worker can read its outstanding work without
keeping the attempt ID. The assignment keeps the most recent 32 rebind records;
the audit stream keeps every one. Rebinding is an additive optional field in
existing schema 17 assignment JSON; old accept receipts replay in their original
shape. Public exposure of the outstanding-work read is a later transport increment.

Migration from schema 16 adds coordination tables without rewriting tasks or
receipts. Older binaries refuse schema 17. Before a later deployment, back up
full state and the previous binary; rollback requires restoring matching state.
Merging this foundation does not request a release or deployment.
