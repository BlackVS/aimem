# Result storage foundation

This increment adds Go storage methods for result submission and coordinator
disposition on top of [assignment storage](TEAM-ASSIGNMENT-STORAGE.md). It does
not expose submit/review through HTTP, CLI or MCP. Existing responses continue
to advertise `workflow_ready: false`; a worker pilot still needs the remaining
lifecycle, recovery and client operations.

## Submission and disposition

`SubmitTeamResult` requires the current assigned worker's bound session handle,
a RUNNING attempt and the task's current expected revision. It stores one result
with a stable ID, attempt ID, worker session/generation, pre-submission task
revision, submission time and the worker's current profile/revision. The original
offer retains its separate task requirements, model/profile and selection rationale.
Model declarations are evidence for the coordinator to assess, never authority.

Result content requires full lowercase Git base and candidate object IDs (40 or
64 hexadecimal characters), a summary, a validation account and at least one typed
evidence reference. Content is limited to 32 KiB of canonical JSON; references
use the existing task limits and authored text/secret checks. Link longer reports.
The store validates shape; it neither fetches repositories nor proves that the
reported commits or checks exist. This first contract is for repository candidates.

Submission atomically sets attempt SUBMITTED and task REVIEW, increments the
task revision and writes task history, assignment/result, audit and retry receipt.
SUBMITTED still reserves the task and the worker's sole capacity slot. A second
submission cannot replace the result, even under a new retry key.

`ReviewTeamResult` requires the current coordinator's session and coordinator
generation, exact result ID, expected task revision, decision and a reason:

| Decision | Attempt | Task | Reservation |
| --- | --- | --- | --- |
| `accept` | ACCEPTED | REVIEW | Released |
| `rework` | RETURNED | READY | Released |

Both decisions increment task revision and record history and an immutable review
bound to the result ID and current coordinator. The result remains unchanged.
Acceptance is a candidate disposition, not proof that review or delivery gates
passed. It cannot mark DONE, merge or deploy. Returning for fixes requires a new
offer and acceptance; the old result and disposition remain readable on their
original attempt through `GetTeamAssignment`. A resumed coordinator may review
previously submitted work using its new current handle and coordinator generation.

## Authorization, retries and compatibility

The service must authenticate the caller's live ordinary write token and project
grant on every call, including receipt replay. Storage receives a trusted actor;
it checks task enablement, current enrollment and the session's persisted
user/token binding in the transaction. New effects additionally check the current
session generation and role, exact assignment ownership, attempt state and task
revision. A worker's availability declaration does not prevent it from submitting
work it already owns. Admin credentials cannot substitute for the assigned worker
or current coordinator.

Identical, authorized retries return the original snapshot without repeating
effects, including after a later transition or session resume. A reused key with
different content conflicts. A new command from a stale handle fails. Worker
resume rebinds a reserved attempt to the new generation in the same transaction
(see [session rebinding](TEAM-ASSIGNMENT-STORAGE.md#session-rebinding)); a
submitted result keeps the handle that submitted it. Missing heartbeat never
releases work.

First-offer management persists after acceptance or return. Generic task update
and archive remain refused, including for admins; standalone agents still skip
the managed task even when its active-attempt projection is empty. Task content
and references are preserved by these transitions. Dedicated edit/finalize and
operator unmanage are separate work.

The result and review are optional additive fields in schema 17 assignment JSON,
including assignment audit snapshots. No table or migration is needed. Existing
offer/accept receipts omit the new fields and replay unchanged. Submission and
review write correlated audit events; delivery into the shared message inbox is
still deferred. These methods do not advertise client support or a full workflow.

Tests cover independent SQLite connections racing to submit or decide, reservation
retention/release, task and history updates, immutable snapshots, failed-write
rollback at every write boundary, stale/wrong principals and generations, receipt
authorization, coordinator resume and reopen with old receipts. They use disposable
local stores, not production credentials or real model execution. Merge does not
request release or deployment; preserve full state and binary backups for rollout.
