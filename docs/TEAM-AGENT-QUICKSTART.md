# Agent team registration

This increment supports registration and roster through HTTP, CLI and MCP.
Messaging and assignment are not implemented yet, so joining does not authorize
coding work. Workers wait for an addressed coordinator assignment and do not
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
`team_resume`, `team_leave`. Supply `project`, defaulting to the current checkout,
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
handle afterward. It cannot stop old local commands; reconcile those first.
Clean leave closes the session and permits an explicit return to standalone
workflow. Missing heartbeats never implicitly leave or release coordinator
ownership. Token replacement and forced recovery are deferred.

## HTTP and operator policy

POST `/v1/projects/{p}/teams/{team}/join|heartbeat|profile|resume|leave` uses the
same operation fields, without project/team/key, and requires `Idempotency-Key`.
GET `/v1/projects/{p}/teams/{team}/members` uses session handles and paging in the
query. Bodies are strict JSON bounded to 64 KiB. Every operation checks live token
scope, user grant and team enrollment, including reads and receipt replay.

Responses advertise protocol version 1, supported operations and waiting
instructions. Public session views omit internal user/token IDs. Unsupported
hubs/versions are refused; never emulate coordination through generic task writes.
Diagnostic logs record outcomes and correlation IDs without raw bodies or secrets.

Operators can set `AIMEM_TEAM_MAX_SESSIONS` (1-10000, default 100) and
`AIMEM_TEAM_SUSPECT_SECONDS` (30-86400, default 120) on the service. Invalid values
refuse session operations. Heartbeat guidance is every 30 seconds. Liveness does
not prove model progress or that a process stopped. No automatic polling loop or
idle wakeup is provided by these tools. Schema remains 15; no release or deployment
is implied by this increment.
