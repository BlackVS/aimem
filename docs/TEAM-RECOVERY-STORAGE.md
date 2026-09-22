# Operator recovery storage

`RecoverTeamAssignment` provides an internal forced-close operation after an
operator reconciles abandoned execution. The admin-only recover route in the
[management routes](TEAM-MANAGEMENT-HTTP.md) exposes it over HTTP and the operator CLI (`aimem teams recover`); there is no
MCP recovery tool, and token rebinding remains a separate increment; a lost
coordinator is handled by [coordinator handoff](TEAM-COORDINATOR-HANDOFF-STORAGE.md).
The complete team workflow still advertises `workflow_ready: false`.

## Reconciliation before closure

Recovery accepts RUNNING, BLOCKED, STOP_REQUESTED or STOPPED attempts. It records
terminal RECOVERED, returns the managed task to READY, clears its blocker and
releases both task and worker reservations. A new offer/accept cycle is required.
OFFERED uses withdrawal; submitted results use result review. Recovery cannot
discard a submitted result, reopen a terminal attempt or mark a task DONE.

The operator must affirm `execution_stopped: true` and supply a reason, runtime
check, worktree check and at least one evidence reference. The runtime check must
account for the backend session/turn and surviving processes or commands; the
worktree check must account for remaining changes and their preservation. Resolve
unknown external effects before recovery. Each text field is bounded to 4096
bytes; existing task-reference validation and a 32 KiB total evidence limit apply.

This is a recorded operator assessment, not proof observed by the hub. Recovery
does not kill processes, undo commands or advance session generations. Missing
heartbeats, model observations and transport reconnects never invoke it
automatically. Closing the attempt rejects new late submissions and work commands;
it cannot prevent a surviving external process from writing to its worktree.

## Authority and concurrency

Storage accepts only a valid trusted admin actor. The service must authenticate
live admin/local-operator authority on every invocation, including receipt replay.
An ordinary coordinator or worker cannot use this operation. Project task
enablement and team existence are checked inside the transaction before receipts.
A departed or unenrolled worker remains recoverable; its former token does not
need authority to let an operator close abandoned execution.

Every new effect checks the exact assignment worker handle, expected task
revision, observed current worker-session generation and latest coordinator-slot
generation. A worker resume rebinds the assignment handle to the new generation;
leave advances the session without rebinding, so the assignment handle and the
separately observed session generation may differ after a departure. A changed
observation requires re-reading and reconciling before another command. The coordinator-slot generation
is checked even when the slot is vacant. Task management, current reservation and
task state must still match the attempt.

Task/history, assignment, audit event and retry receipt commit atomically. The
immutable recovery snapshot contains the checked generations, prior task revision,
timestamp and reconciliation evidence. Original offer, worker and profile snapshots
remain intact. The audit actor is the operator; its session snapshot describes the
affected worker, which may have departed. That snapshot grants no authority.

Identical authorized retries return the original snapshot after later transitions
or database restart. Historical accept receipts may still say RUNNING: receipts
report a past command, not current ownership. Read the current assignment before
acting. Changed input under the same key conflicts; a rolled-back attempt does not
consume its key. Concurrent submission, cancellation or recovery has one winner.
Session resume may occur after recovery, but cannot reopen the closed attempt.

The task stays managed, so generic edits/archive remain denied even for admins
until an operator [unmanages](TEAM-MANAGED-LIFECYCLE-STORAGE.md) it. This adds
optional assignment JSON in existing schema 17, without a migration or public
route. Tests cover stale observations, departure, receipts,
independent SQLite races and rollback at every write boundary. Storage fixtures
do not demonstrate real process reconciliation or authorize a release or pilot.
