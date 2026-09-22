# Management and admin HTTP routes

The hub exposes the coordinator's management operations and the operator's
recovery and release operations over HTTP, on top of the
[coordinator handoff](TEAM-COORDINATOR-HANDOFF-STORAGE.md),
[managed-task lifecycle](TEAM-MANAGED-LIFECYCLE-STORAGE.md) and
[operator recovery](TEAM-RECOVERY-STORAGE.md) storage contracts. Together with
the [assignment](TEAM-ASSIGNMENT-HTTP.md) and [execution](TEAM-EXECUTION-HTTP.md)
routes this is the complete lifecycle over HTTP, and the CLI and MCP tools
bridge the coordinator operations; recover and unmanage are CLI-only for the
operator. Handoffs and recoveries also deliver
[lifecycle messages](TEAM-MESSAGE-STORAGE.md#lifecycle-messages) to the affected
inboxes, and responses say `workflow_ready:true`. Readiness is not a pilot
approval: the rehearsal and a separately approved deployment come first.

All paths below begin with `/v1/projects/PROJECT/teams/TEAM_ID`. Every command
requires `Idempotency-Key` and one strict JSON body of at most 64 KiB; unknown
fields and trailing values are rejected, and identical retries return the
original receipt after the caller is reauthorized. Storage decides generation,
management, reservation, revision and transition, mapped through the same
refusals as the execution routes; a revision conflict carries the current task.

| Method and suffix | Authority and effect |
| --- | --- |
| `POST /handoff` | Current coordinator with its bound ordinary session, or an admin token for a lost coordinator with reconciliation: transfers the slot to an active designated session with no reserved attempt; responds with the successor's session view |
| `POST /tasks/TASK_ID/edit` | Current coordinator: replaces a managed task's content between attempts; state must stay the current one |
| `POST /tasks/TASK_ID/finalize` | Current coordinator: records DONE for a REVIEW task whose named attempt was accepted, with merge/delivery evidence |
| `POST /assignments/ATTEMPT_ID/recover` | Admin token only: closes an abandoned RUNNING/BLOCKED/STOP_REQUESTED/STOPPED attempt as RECOVERED with recorded reconciliation; task READY |
| `POST /tasks/TASK_ID/unmanage` | Admin token only: releases a task this team manages with no reserved attempt; generic writes return |

Handoff carries the outgoing `session_id`/`generation`, `coordinator_generation`
as observed, `target` `{session_id, generation}` and a nonempty `reason`. An
ordinary coordinator names its own live handle and must not send
`reconciliation`; an admin names the lost coordinator's exact handle and current
slot generation and must send `reconciliation` with `coordinator_stopped:true`,
a `liveness_check` and at least one typed `evidence_refs` entry. The route serves
both from the role the bearer token actually carries; the audit event records
that actor. Legacy writer tokens have no path. The successor's session generation
does not change and its liveness is untouched; the outgoing session is closed
and its handles are stale for every command and read.

Edit carries the coordinator handle, `coordinator_generation`, `expected_revision`,
the full `content` (with `state` equal to the current state and `archived` false)
and a `reason`. Finalize carries the handle, `coordinator_generation`,
`expected_revision`, `attempt_id` of the ACCEPTED attempt, a `reason` and
`evidence` (typed references, at least one). Both answer `protocol_version:1`,
`task` (the task view with links) and `workflow_ready:true`; an admin token is
refused with 403 because these are session-bound coordinator operations.

Recover and unmanage are admin routes: the bearer gate refuses ordinary tokens
before any handler runs, and storage refuses any non-admin actor. Recover
carries `expected_revision`, `expected_worker` exactly as assigned,
`expected_session_generation` and `expected_coordinator_generation` as observed,
and `reconciliation` with `execution_stopped:true`, `reason`, `runtime_check`,
`worktree_check` and `evidence_refs`. Unmanage carries `expected_revision` and a
`reason`; the team in the URL is checked inside the storage transaction and
bound into the retry receipt, so a task another team manages is refused with
409 under this team's prefix and a retry replays only through the same prefix.
Both are recorded operator assessments: nothing here stops a process,
verifies evidence or proves a coordinator or worker is gone. Merging this
increment does not request a release, deployment or pilot.
