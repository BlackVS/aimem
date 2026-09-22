# Cooperative work-control storage

`ChangeTeamWork` extends the assignment/result storage foundation with five
internal commands, exposed over HTTP, CLI and MCP by the [execution routes](TEAM-EXECUTION-HTTP.md);
each command delivers a [lifecycle message](TEAM-MESSAGE-STORAGE.md#lifecycle-messages)
to the counterpart's inbox. Operator recovery has a
[separate storage method](TEAM-RECOVERY-STORAGE.md); token rebinding and
shared-inbox lifecycle delivery remain separate increments.

## State and authority

Every command carries a bound session handle, expected task revision and a
nonblank reason (at most 4096 bytes). Coordinator commands also require the
current coordinator generation; worker commands must omit it.

| Command | Caller | Prior attempt | Next attempt | Task state |
| --- | --- | --- | --- | --- |
| `block` | Assigned worker | RUNNING | BLOCKED | BLOCKED |
| `resume-work` | Assigned worker | BLOCKED | RUNNING | IN_PROGRESS |
| `cancel` | Current coordinator | RUNNING or BLOCKED | STOP_REQUESTED | BLOCKED |
| `stopped` | Assigned worker | STOP_REQUESTED | STOPPED | BLOCKED |
| `close-stop` | Current coordinator | STOPPED | CANCELLED | READY |

BLOCKED, STOP_REQUESTED and STOPPED retain both the task reservation and the
worker's sole capacity slot. Only `close-stop` releases them. It closes the
attempt, not the task: the task stays managed and returns to READY for a new
offer/accept cycle. Generic task update/archive remains refused, including for
admins. Submitted results use the separate result-review path and cannot be
cancelled through these commands.

Block and cancel set the task's blocker from the reason. Resume and closure
clear it; stop acknowledgement preserves the cancellation blocker. Other task
content remains intact. The resulting task is validated before writing, so a
near-limit task cannot silently exceed its content-size limit when blocked.
Every accepted command increments task revision and commits task history,
assignment, audit event and retry receipt in one transaction.

## What a stop means

Cancel is a request to stop, never evidence that the local process has stopped.
It immediately rejects further result submissions or work resumption for that
attempt. The cooperative worker must reconcile its local commands and worktree,
then use `stopped` with a reason recording what it checked. This acknowledgement
is a declaration, not a hub observation or a process-kill guarantee.

The coordinator assesses that acknowledgement and records its reason for
requeueing in `close-stop`. Until then, even STOPPED retains ownership. The
worker cannot close its own attempt, and the coordinator cannot impersonate the
worker's acknowledgement. The assignment's latest `reason` reflects the last
transition; prior cancellation, stop and closure reasons and authenticated
session/generation snapshots remain in their immutable correlated audit events.

Missing heartbeats, unavailable profiles, model IDLE/DEAD/STALLED observations,
transport reconnects and supervisor restarts do not perform these transitions.
Session resume/leave also retain reservations. A worker resume rebinds its
reserved attempt to the new generation, so the returned handle can block, resume
work or acknowledge a stop; the pre-resume handle is stale (see
[session rebinding](TEAM-ASSIGNMENT-STORAGE.md#session-rebinding)). Resume does
not prove the old local command stopped. If no current handle can acknowledge a
stop, explicit operator recovery with reconciliation is needed; see
[operator recovery storage](TEAM-RECOVERY-STORAGE.md). Do not bypass this with
generic task edits or reassign the same task outside aimem.

## Boundaries and retries

Storage receives a trusted actor. The service must authenticate its live ordinary
write token and project grant on every invocation, including receipt replay.
Storage checks task enablement, current enrollment and persisted user/token
binding in the transaction. New effects additionally check role, session and
coordinator generations, exact assignment ownership, legal transition and task
revision. Admin credentials cannot stand in for an ordinary participant.

Identical authorized retries return the original snapshot, even after later
transitions or session resume/leave. Different input under the same key conflicts.
New stale commands fail; the same key after a rolled-back failure may be retried.
A submit/cancel race has one winner. A resume/cancel race at the same task revision
also has one winner: re-read after conflict rather than assuming cancellation.

These are additional states in existing schema 17 rows, with no migration,
dependency change or new public route. Existing offer/result methods and receipts
retain their contracts. Tests use independent SQLite connections, failure injection
at every write boundary, restarts and stale-session scenarios. This validates the
storage handshake; it does not validate a real worker process stopping. Merge
does not request a release, deployment or live-agent pilot.
