# Managed-task lifecycle storage

Schema 18 adds the three managed-task operations the team design reserves for
the coordinator and the operator: `EditManagedTask`, `FinalizeManagedTask` and
`UnmanageTask`. The [management routes](TEAM-MANAGEMENT-HTTP.md) expose them
over HTTP, edit and finalize also over CLI and MCP, and unmanage over the
operator CLI. Edit, finalize and unmanage deliver no lifecycle message: they are
task-level events without a counterpart session.

## Coordinator edit

The current coordinator (bound session, current session and coordinator
generation) replaces the full content of a task its team manages, under the
task's expected revision and with a reason. Edits are allowed only while no
attempt is reserved; changing scope during execution requires a stop and a new
offer. The task state and archive flag belong to coordination operations, so
an edit must carry the current state and cannot archive. Ordinary content
validation, epic assignability and the existing size limits apply. Task row,
task history, one `team.task.edit` audit event with the change record and the
retry receipt commit together. Generic PUT/archive stays refused; a later
offer snapshots the edited content as its requirements.

## Coordinator finalize

Coordinator acceptance leaves a managed task in REVIEW for the ordinary
delivery gates. Finalize records DONE only after those gates: the current
coordinator names the ACCEPTED attempt of that task, a reason and at least one
merge/delivery evidence reference. The evidence is appended to the task's
evidence references and to the `team.task.finalize` audit event; the resulting
task must still fit its bounds. Submitted, returned, recovered or foreign
attempts cannot finalize, a task with a reserved attempt cannot finalize, and
the hub does not verify that the evidence proves a merge. The task remains
managed after DONE, so generic writes stay refused and no new offer is possible.

## Operator unmanage

A trusted admin actor releases a task from the named team's management with
the expected revision and a reason, only while no attempt is reserved. The team
is checked inside the transaction and bound into the receipt scope: a task
another team took over (offers and withdrawals do not advance the task
revision) is refused, and a retry replays only through the same team. The
management row
keeps its history reference (assignments point at it under enforced foreign
keys) and is flagged released: the task's coordination projection disappears,
generic update and archive work again, and coordinator operations refuse the
task. The revision advances with an identical-content history row so a
concurrent coordinator command at the old revision conflicts. The
`team.task.unmanage` event carries no session, only the operator actor and the
change record. Historical attempts remain readable through their IDs; a later
offer from any team manages the task afresh with a new attempt. The service
must authenticate live admin authority on every call, including receipt replay.

## Concurrency, retries and migration

Every operation runs in one transaction with the task, history, audit and
receipt writes; failure at any boundary rolls back everything and leaves the
key unused. Identical authorized retries return the original task snapshot,
including after reopen; changed input under the same key conflicts. An edit or
unmanage racing an offer at the same revision has one winner, as does finalize
racing unmanage. Tests cover both authorities, every fence, rollback per write
boundary, independent SQLite races, reserved-state refusals and the 17 to 18
migration with reopen.

Migration 17 to 18 adds one flag column to the management table and rewrites
no rows; existing managed tasks stay managed. Older binaries refuse schema 18.
Before a later deployment, back up full state and the previous binary;
rollback requires restoring matching state. Merging this increment does not
request a release, deployment or pilot.
