# Execution HTTP routes

The hub exposes the worker and coordinator execution primitives over HTTP:
reading the calling session's reserved attempt, blocking and resuming work,
requesting, acknowledging and closing a stop, submitting a result and
recording its disposition. They use the merged storage contracts for
[results](TEAM-RESULT-STORAGE.md), [cooperative work control](TEAM-WORK-CONTROL-STORAGE.md)
and [session rebinding](TEAM-ASSIGNMENT-STORAGE.md#session-rebinding), and
follow the [assignment HTTP primitives](TEAM-ASSIGNMENT-HTTP.md) in every
authorization and encoding rule. Handoff, managed edit/finalize, operator
recovery and unmanage are the [management routes](TEAM-MANAGEMENT-HTTP.md).
The CLI and MCP tools in the [quickstart](TEAM-AGENT-QUICKSTART.md) bridge to
these routes. Every transition also delivers a
[lifecycle message](TEAM-MESSAGE-STORAGE.md#lifecycle-messages) to the
counterpart's inbox, and responses say `workflow_ready:true`. Readiness is not a
pilot approval: the rehearsal and a separately approved deployment come first.

All paths below begin with `/v1/projects/PROJECT/teams/TEAM_ID`. Each request
requires the caller's ordinary write token for this project, current grant and
enrollment, and a session bound to that token; admin, local operator, checkpoint
and read-only credentials are refused with 403. Session handles are concurrency
keys, never authority. The storage layer decides role, generation, ownership,
transition and revision, so HTTP refuses exactly as the later CLI/MCP will.

| Method and suffix | Actor and effect |
| --- | --- |
| `GET /assignments/reserved?session_id=SESSION_ID&generation=GENERATION` | Calling current session reads the attempt reserved for it (open offer or RUNNING/BLOCKED/STOP_REQUESTED/STOPPED/SUBMITTED); 404 when none |
| `POST /assignments/ATTEMPT_ID/block` | Assigned worker: RUNNING to BLOCKED, task BLOCKED with the reason as blocker |
| `POST /assignments/ATTEMPT_ID/resume-work` | Assigned worker: BLOCKED to RUNNING, task IN_PROGRESS, blocker cleared |
| `POST /assignments/ATTEMPT_ID/cancel` | Current coordinator: RUNNING or BLOCKED to STOP_REQUESTED, task BLOCKED |
| `POST /assignments/ATTEMPT_ID/stopped` | Assigned worker acknowledges after reconciling local execution: STOPPED, reservation retained |
| `POST /assignments/ATTEMPT_ID/close-stop` | Current coordinator: STOPPED to CANCELLED, task READY |
| `POST /assignments/ATTEMPT_ID/submit` | Assigned worker submits one immutable result: SUBMITTED, task REVIEW |
| `POST /assignments/ATTEMPT_ID/review` | Current coordinator records accept (ACCEPTED, task stays REVIEW) or rework (RETURNED, task READY) |

Commands require `Idempotency-Key` and one JSON body of at most 64 KiB; unknown
fields and trailing values are rejected. Worker commands carry `session_id`,
`generation`, `expected_revision` and a nonempty `reason` (4096 UTF-8 bytes at
most); coordinator commands add `coordinator_generation`. Submission carries the
handle, `expected_revision`, `base_commit` and `commit` as full Git object IDs,
`summary`, `validation` and at least one typed `evidence_refs` entry. Review
carries the coordinator handle, `coordinator_generation`, `expected_revision`,
`result_id`, `decision` (`accept` or `rework`) and `reason`. Use revisions and
generations the hub actually returned. Successful operations respond 200 with
`protocol_version:1`, `assignment` and `workflow_ready:true`; responses expose
session handles and snapshots, never token or access-user bindings.

Refusals follow the assignment routes: 400 invalid input or missing retry key,
401 invalid caller credential, 403 authority/enrollment/binding denial, 404
missing scoped resource or no reserved attempt, 409 stale generation, revision
conflict (with the current task), invalid transition or retry mismatch. Re-read
before choosing a new command. A query string with a malformed percent-encoding
is refused with 400. After a session resume the previous handle is stale for its
attempt and the returned handle commands it; read the reserved attempt first and
reconcile any surviving local command, because no route here stops a process.
Acceptance never marks DONE: finalization, forced recovery and unmanage are the
management routes. Storage fixtures and these HTTP tests do
not demonstrate real client behavior; merging this increment does not request a
release, deployment or pilot.
