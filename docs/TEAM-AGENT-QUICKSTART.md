# Agent team registration, messages and work

Registration, roster, messages, assignments, execution and coordinator
management are available through HTTP, CLI and MCP, and every transition
delivers a lifecycle message to the counterpart's inbox, so responses say
`workflow_ready:true`. That states the protocol loop is complete; a pilot still
needs the rehearsal and a separately approved deployment. Joining or receiving a
message does not authorize coding work; only an accepted offer does. Workers wait for an addressed coordinator assignment and do not
select backlog work independently while joined. Selected project process rules
still apply. The later pilot will test complete coordination with real agents.

An administrator first creates a team and enrolls users as described in
[TEAM-SETUP.md](TEAM-SETUP.md). A requesting coordinator must be eligible and the
coordinator slot must be vacant. Agents cannot self-enroll or administer teams.

Run from a configured checkout using its ordinary task credential. User-scoped
tokens follow current grants; project-scoped tokens use the checkout-local
credential store. CLI and stdio MCP share this resolution. Missing or invalid
credentials never fall back to the trusted operator socket. Admin, checkpoint,
legacy and read-only credentials cannot join or read the roster.

## MCP

Tools: `team_join`, `team_members`, `team_profile`, `team_heartbeat`,
`team_resume`, `team_leave`, `team_send`, `team_messages`, `team_inbox`, `team_ack`;
assignment and execution: `team_offer`, `team_assignment`, `team_reserved`,
`team_accept`, `team_decline`, `team_withdraw`, `team_block`, `team_resume_work`,
`team_cancel`, `team_stopped`, `team_close_stop`, `team_submit`, `team_review`;
coordinator management: `team_handoff`, `team_edit`, `team_finalize`. Each work
tool takes the same fields as its [execution](TEAM-EXECUTION-HTTP.md) or
[management](TEAM-MANAGEMENT-HTTP.md) route, plus `attempt` or `task` where the
route needs one; the hub decides authority and generations, so refusals match
HTTP. Admin recover and unmanage are never MCP tools.
Supply `project`, defaulting to the current checkout,
and `team`. Join accepts an exact readable name or stable ID; later operations
use the returned `session.team_id`. Use the ID for unusual names that cannot
form a canonical URL segment.

Example `team_join` arguments:

```json
{
  "project": "example-project",
  "team": "Builders",
  "role": "worker",
  "profile": {
    "label": "compiler-worker",
    "platform": "your-agent-client",
    "platform_version": "unknown",
    "model": {"provider": "unknown", "id": "unknown", "version": "unknown", "source": "unknown"},
    "capabilities": ["go", "unit-tests"]
  },
  "idempotency_key": "join-worker-001"
}
```

Report a model only when known; never infer it from the client. Model sources are
`runtime_reported`, `operator_configured`, `agent_reported` or `unknown`. These
are declarations, not attestation or permission. Save the returned session ID,
generation and profile revision. Two agents using the same credential have
distinct sessions but remain the same security principal.

## CLI

Place `role` and `profile` from the example in `join.json`:

```sh
aimem teams join example-project Builders join.json join-worker-001
aimem teams members example-project TEAM_ID handle.json
aimem teams heartbeat example-project TEAM_ID heartbeat.json heartbeat-001
aimem teams profile example-project TEAM_ID profile.json profile-001
aimem teams resume example-project TEAM_ID handle.json resume-001
aimem teams leave example-project TEAM_ID handle.json leave-001
```

`handle.json` contains `session_id` and `generation`. For roster paging, add
`after` and `limit` (1-100); an empty `next_cursor` ends paging. Heartbeat adds
`availability` (`available` or `unavailable`). Profile adds the full `profile`
and `expected_profile_revision`. The CLI supplies project/team/retry key; these
must not occur in the request file.

Each distinct write needs a new key. Retry an uncertain write using its original
key and identical content. A retry returns its original snapshot, which can now
be stale; it does not restore old authority or refresh liveness again.
Resume needs the same user/token and increments generation. Update the saved
handle afterward; a reserved attempt other than an open offer follows the new
generation, and the old handle is stale for it. Resume cannot stop old local
commands; reconcile those first.
Clean leave closes the session and permits an explicit return to standalone
workflow. Missing heartbeats never implicitly leave or release coordinator
ownership. Token replacement and forced recovery are deferred.

## Message exchange

Use `team_send` with the session handle, a recipient and typed content:

```json
{
  "project": "example-project",
  "team": "TEAM_ID",
  "session_id": "YOUR_SESSION_ID",
  "generation": 1,
  "recipient": {"kind": "member", "id": "RECIPIENT_SESSION_ID"},
  "kind": "question",
  "payload": {"text": "Which test covers the parser change?"},
  "idempotency_key": "question-001"
}
```

Recipients are a member session or `{"kind":"team"}`. All messages are visible
to current team members; a direct recipient routes the inbox and is not private.
Do not include secrets. Kinds are `question`, `answer`, `note`, `progress`,
`blocker` and `review_feedback`. An answer requires `reply_to` identifying a
question in this team. Optional `task_id` and payload task references must exist
in this project. Payload accepts typed `refs` and a question-only RFC3339
`deadline`. Attempt references remain unavailable until assignment integration.
The server derives sender identity and its profile snapshot; agents cannot
provide those fields. Content is limited to 32 KiB, commands to 64 KiB.

Read `team_inbox` with your handle, `after` (nonnegative sequence, default 0),
`limit` (1-100, default 50) and optionally `wait_seconds` (0-25, default 0).
It returns `messages`, `next_cursor`, `has_more` and `server_time`. An empty page
ends this poll, not the assignment. `team_messages` reads team-visible history
with the same paging fields but without a wait; history reads do not mark delivery.

Reading an inbox records an attempted delivery. Call `team_ack` with the same
handle, a new retry key and `message_ids` (1-100 distinct IDs delivered to this
session) only after receiving the messages. Ack is neither an answer nor task
completion. Retrying an uncertain send or ack with its original key/content
returns the original receipt after current authorization checks.

Advance the cursor within a page scan, but retain explicit acknowledgements as
the durable receipt boundary. On reconnect use `after: 0` to recover all messages
still unacknowledged, including earlier pages; deduplicate by message ID.
Advancing a cursor never acknowledges a message. Resume updates the generation;
old handles cannot perform new reads or commands. Messages never assign work.

For CLI use, omit project/team/idempotency_key from the JSON files:

```sh
aimem teams send example-project TEAM_ID message.json question-001
aimem teams inbox example-project TEAM_ID inbox.json
aimem teams messages example-project TEAM_ID handle.json
aimem teams ack example-project TEAM_ID ack.json ack-001
```

`inbox.json` contains the session handle and optional paging/wait fields;
`ack.json` contains the session handle and `message_ids`. Work commands use the
same shape, for example `aimem teams accept example-project TEAM_ID accept.json accept-001`
with `session_id`, `generation` and `attempt` in the file; `assignment` and
`reserved` are reads without a key. Operators run
`aimem teams recover PROJECT TEAM ATTEMPT reconciliation.json`,
`aimem teams unmanage PROJECT TEAM TASK request.json` and
`aimem teams rebind-token PROJECT TEAM SESSION request.json` on the hub host.
CLI and stdio MCP use the checkout's ordinary credential. They support the full 25-second wait;
third-party HTTP/MCP callers must also allow sufficient request time.

## The complete loop

A worker joins, then polls its inbox with bounded waits. An offer arrives as a
`lifecycle` message from the hub (no sender session) naming the attempt; the
worker reads the assignment, accepts or declines, and acknowledges the message.
While working it blocks and resumes as needed; a coordinator stop request
arrives the same way and is acknowledged with `stopped`. The worker submits a
result with commits and evidence, then waits: the coordinator's accept or rework
arrives as a lifecycle message, and rework means a new offer will follow. The
coordinator, in turn, sees acceptance, blocking, acknowledgement and submission
in its own inbox. A handoff is broadcast to every remaining member with the new
coordinator generation; an operator recovery is delivered to both parties.
Acknowledging a lifecycle message records receipt only; the assignment row
remains the authority, and nothing in the inbox authorizes work by itself.

## HTTP and operator policy

POST `/v1/projects/{p}/teams/{team}/join|heartbeat|profile|resume|leave` uses the
same operation fields, without project/team/key, and requires `Idempotency-Key`.
GET `/v1/projects/{p}/teams/{team}/members` uses session handles and paging in the
query. Bodies are strict JSON bounded to 64 KiB. Every operation checks live token
scope, user grant and team enrollment, including reads and receipt replay.
Message routes are POST/GET `/v1/projects/{p}/teams/{team}/messages`,
GET `/v1/projects/{p}/teams/{team}/inbox` and POST `/v1/projects/{p}/teams/{team}/ack`.
Assignment and execution routes live under `/v1/projects/{p}/teams/{team}/assignments`;
see [assignment](TEAM-ASSIGNMENT-HTTP.md), [execution](TEAM-EXECUTION-HTTP.md) and
[management](TEAM-MANAGEMENT-HTTP.md) routes (handoff, managed edit/finalize, and the
admin-only recover and unmanage).
Writes require `Idempotency-Key`. Inbox polls recheck token expiry/revocation,
grant, task enablement, enrollment and session generation each second; disconnect
cancels the wait. No database transaction is held while waiting.

Responses advertise protocol version 1, supported operations and waiting
instructions. Public session views omit internal user/token IDs. Unsupported
hubs/versions are refused; never emulate coordination through generic task writes.
Diagnostic logs record outcomes and correlation IDs without raw bodies or secrets.

Operators can set `AIMEM_TEAM_MAX_SESSIONS` (1-10000, default 100) and
`AIMEM_TEAM_SUSPECT_SECONDS` (30-86400, default 120) on the service. Invalid values
refuse session operations. Heartbeat guidance is every 30 seconds. Liveness does
not prove model progress or that a process stopped. No automatic polling loop or
idle wakeup is provided by these tools. A bounded wait is one request, not an
automatic client loop. `AIMEM_TEAM_MAX_MESSAGES` sets the retained per-team limit
(1-1000000, default 10000). Full storage refuses new sends with 409; accepted
retry receipts still work, and no unacknowledged content is evicted. Retention and
export policy remain separate work. Uses existing schema 16; no new migration,
release or deployment is implied by this increment.
