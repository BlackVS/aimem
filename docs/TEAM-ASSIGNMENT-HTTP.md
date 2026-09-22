# Assignment HTTP primitives

The hub supports offering, reading, accepting, declining and withdrawing team
assignments over HTTP. These use the [schema 17 storage contract](TEAM-ASSIGNMENT-STORAGE.md).
They are not a complete execution workflow: responses include `workflow_ready:false`.
The [execution routes](TEAM-EXECUTION-HTTP.md) add the reserved-attempt read,
block/resume, stop request/acknowledgement/closure, submission and review;
handoff, managed edit/finalize, recovery, unmanage and the MCP/CLI execution
tools are still deferred. Do not start a worker pilot yet.

All paths below begin with `/v1/projects/PROJECT/teams/TEAM_ID`. Use the team ID
returned by join. Each request requires `Authorization: Bearer TOKEN` with the
caller's ordinary write token for this project and current grant and enrollment.
Admin tokens, the local operator socket, checkpoint and read-only credentials
cannot act as agent sessions. The session must be bound to the authenticated
token; a session ID, model declaration or supervisor/backend ID is not authority.

| Method and suffix | Actor and effect |
| --- | --- |
| `POST /assignments` | Current coordinator offers a READY task; reserves task and worker |
| `GET /assignments/ATTEMPT_ID?session_id=SESSION_ID&generation=GENERATION` | Current team member reads the current assignment |
| `POST /assignments/ATTEMPT_ID/accept` | Intended worker accepts; task becomes IN_PROGRESS and attempt RUNNING |
| `POST /assignments/ATTEMPT_ID/decline` | Intended worker declines an OFFERED attempt with a reason |
| `POST /assignments/ATTEMPT_ID/withdraw` | Current coordinator withdraws an OFFERED attempt with a reason |

Commands require `Idempotency-Key: UNIQUE_COMMAND_KEY` and one JSON body of at
most 64 KiB. Reuse the key only for the identical command. Unknown fields and
trailing JSON values are rejected. Reads take only the two shown query parameters,
each once. Offer responds 201; other successful operations respond 200. All return
`protocol_version:1`, `assignment` and `workflow_ready:false`. Responses expose
session handles and profile/task snapshots, not token or access-user bindings.

Offer body:

```json
{
  "session_id": "COORDINATOR_SESSION_ID",
  "generation": 1,
  "coordinator_generation": 1,
  "task_id": "TASK_ID",
  "expected_revision": 3,
  "worker": {"session_id": "WORKER_SESSION_ID", "generation": 1},
  "suitability_rationale": "S task; required Go capability and model fit confirmed",
  "cost_rationale": "Least costly suitable available member"
}
```

Use revisions and generations actually returned by the hub. Both rationales are
required and limited to 4096 UTF-8 bytes each. Assess complexity, capability and
model suitability first; clarify unknown suitability or decompose uncertain work.
Record a reason when a stronger model is needed. The offer snapshots the task
requirements and worker profile/model/source; later profile changes do not rewrite
it. These assessments do not grant access or attest model competence.

Acceptance body contains only the intended worker's `session_id` and `generation`.
Decline adds a nonempty `reason`, at most 4096 UTF-8 bytes. Withdrawal uses the
current coordinator's handle, `coordinator_generation` and a nonempty `reason`.
Decline and withdrawal close only OFFERED work. A worker may not choose a different
task through acceptance or decline. Substantial peer investigation needs its own
coordinator offer. A message or inbox acknowledgement never assigns work.

The service checks live caller authority on every request, even receipt retries.
For new offers and acceptances it also checks the persisted worker session's live
token, user status, project scope and grants through the access database. Revocation
prevents new effects; it does not erase an already accepted command's receipt for
an otherwise authorized caller. A coordinator may withdraw an unaccepted offer
after the worker loses access. It cannot use withdrawal to cancel RUNNING work.

Typical refusals are 400 for invalid inputs, 401 for invalid caller credentials,
403 for scope/grant/enrollment/binding denial, 404 for missing scoped resources,
and 409 for stale generations, revision/readiness/dependency conflicts, occupied
capacity, invalid transitions or mismatched retries. A revision conflict includes
the current task response. Re-read before choosing a new command; never emulate
assignment by generic PUT. Managed-task edits/archive remain refused even for
admins, including between attempts. Missing heartbeat and session resume/leave
never release ownership; a worker resume rebinds its accepted attempt to the new
generation, while an open offer stays with the generation that received it.
A later recovery operation will require reconciliation.
