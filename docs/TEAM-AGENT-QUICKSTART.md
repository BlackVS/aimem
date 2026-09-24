# Agent team registration, messages and work

For a step-by-step operator walkthrough with simple commands, credential
rules and troubleshooting, start with [Start an agent team](TEAM-QUICKSTART.md).

Registration, roster, messages, assignments, execution and coordinator
management are available through HTTP, CLI and MCP, and every transition
delivers a lifecycle message to the counterpart's inbox, so responses say
`workflow_ready:true`. That states the protocol loop is complete; a pilot still
needs the rehearsal and a separately approved deployment. Joining or receiving a
message does not authorize coding work; only an accepted offer does. Workers wait for an addressed coordinator assignment and do not
select backlog work independently while joined. Selected project process rules
still apply. The later pilot will test complete coordination with real agents.

An administrator first creates a team and enrolls users as described in
[TEAM-SETUP.md](TEAM-SETUP.md). What to do at each step, how to resolve
questions with evidence and how to escalate to a human are in the
[team playbooks](TEAM-PLAYBOOKS.md), with validated request templates under
[docs/examples/team](examples/team/). The [protocol rehearsal](TEAM-REHEARSAL.md)
drives the pilot scenarios over this wire on an isolated hub without a model. A requesting coordinator must be eligible and the
coordinator slot must be vacant. Agents cannot self-enroll or administer teams.

Run from a configured checkout using its ordinary task credential. User-scoped
tokens follow current grants; project-scoped tokens use the checkout-local
credential store. CLI and stdio MCP share this resolution. Missing or invalid
credentials never fall back to the trusted operator socket. Admin, checkpoint,
legacy and read-only credentials cannot join or read the roster.

## MCP

Onboarding, on the checkout-bound local server only (`aimem mcp`, started
by the client in the checkout as the account that installed its
credential): `team_setup` (verified onboarding as worker or coordinator:
the same checks, saved-session reconciliation, retry keys and role entry as
`aimem teams setup`; arguments `team`, `role`, an optional declared
`profile`, `resume`, `new_session`, `repair_integration` (default report
only), `allow_project_stop_hooks`) and `team_continue` (the restart path of
`aimem teams continue`: verify or resume the saved membership and report
its duties; arguments `team` and `fence`; never joins). Both return the
same JSON report as the CLI's `--json`, with `run_as` naming the account
the process runs as; a blocked report is a result, not a tool error. They
accept no token, checkout, command or executable, read HEAD but not the
working tree, and are absent from the hub's MCP endpoint, which has no
checkout. A shell the client runs under another account cannot read the
checkout owner's credential; the MCP process can, which is why the
`/join_team` and `/resume_team` entry points call these tools.

`joined` is not permission to work. For a verified session the report's
`readiness` object has four parts: `membership` (`active`, or `ended`,
`refused`, `not_verified` without a session), `role_context` (`delivered`
with the guidance version and digest), `project_process` (the
`process_context` state, `ready` only when the hub confirmed the selection
and the complete unit is here) and `execution`, always `not_verified`:
nothing here can see your own shell, build tools or coding runner.
`ready_for_work` is true only for an active membership with the role
guidance delivered and the process `ready`. The role's complete guidance
and the complete project process follow the report as their own text
blocks (the CLI prints them after the report; `--json` carries them under
`delivered`), each ending with its terminator line. Delivery is not
acknowledgement: name the guidance digest and the process commit you
followed in the evidence of a submitted result. A worker that is not ready
is announced `unavailable`, so the hub neither offers it work nor lets it
accept any; keep your own heartbeats `unavailable` until `team_continue`
reports ready. A coordinator that is not ready issues no offers. A
heartbeat the hub refuses is reported; the membership is kept. A changed
guidance digest since the last delivery is reported as a `role context`
warning.

An attempt the hub already holds for a worker that is not ready is handled
by its state, with the worker's own operations only: an `OFFERED` attempt
(including one that landed between the join and the `unavailable`
heartbeat) is declined with the readiness reason, which returns the offer
to the coordinator as any decline does; a `RUNNING` attempt is blocked
with the missing context and stays reserved to the worker (the block names
the task's current revision, read with the checkout's credential); a
`BLOCKED` attempt is left as it is; for `STOP_REQUESTED`, `STOPPED` and
`SUBMITTED` nothing is sent, and a stop is never acknowledged for you:
send `team_stopped` yourself once you have established that the work
stopped. Accept, resume-work, stopped and leave are never sent. Each write
is recorded with its retry key and exact content before it is sent; an
outcome that is not confirmed is repeated identically on the next run only
while the attempt is still where it was, and dropped when it moved on,
when the session generation changed or when the worker is ready. The
`attempt` check says what happened, and the reserved attempt's next step
never tells a worker that is not ready to accept, continue or resume.

An accepted attempt keeps the process version it was accepted under. From
a checkout (`team_accept` on the checkout-bound local server, or `aimem
teams accept`), the accept first records that version in the checkout's
nonsecret team state: the attempt, the session, the exact process commit
and manifest, and the team guidance digest. It is refused locally, with
nothing sent, when that cannot be recorded: no saved membership for the
session in the request, or a project process that is not `ready` (a worker
that is not ready accepts nothing). A retry of the same attempt, after an
uncertain reply or a restart, reuses the recorded version and never binds
a newer selection to it. While the attempt is reserved, `team_setup`,
`team_continue` and `process_context` deliver that exact version, marked
PINNED, with the live checks of any other read; a newer project selection
is named but not delivered as the attempt's rules, and applies to new work
once the attempt ends. An accepted attempt whose version this checkout did
not record (accepted through the hub endpoint, from another checkout, by
an older aimem, or with the record lost) or cannot recover (the exact
commit unavailable, access denied, the hub unreachable) is not ready, and
a `RUNNING` one is blocked as above; no version is assumed.

Project process, on the checkout-bound local server only: `process_context`
returns this project's selected process as one complete unit (the text
`aimem process show --full` prints: handbook, the checklists gating task
states, required skills, template kinds), or with `template` one template
by the kind the manifest names. It uses the checkout's own credential with
no fallback, reads only the selection the hub names and this machine's
exact-commit cache or Git at that commit, changes nothing and takes no
path, URL, repository, commit or project. The result ends with a
terminator line (project, commit, manifest, SHA-256 of the delivered
text). It is delivered in state `ready`, or in state `last_observed`:
the hub is unreachable, and the exact cached commit of the selection last
observed is delivered with that notice, never as authorization for a task
write. Every other state is an error naming the state, the cause and the
fix: `disabled`, `not_selected`, `denied` (the hub refused the credential,
or Git refused this machine; never replaced by a cached selection),
`unavailable` and `too_large` (over 32 KiB; nothing is delivered in part,
templates still are).

Guidance, on both the local and the hub endpoint, for any caller the
endpoint admits: `team_context` returns the team guidance built into the
running aimem binary (this page's [playbooks](TEAM-PLAYBOOKS.md) and the
request templates), with the unit's version and SHA-256 digest. `role`
(`worker` or `coordinator`) returns that role's complete required set,
`section` one section by id (`worker`, `example/submit`, ...), and no
argument the index of ids, sizes and digests. It needs no session,
checkout, project grant, network or shell, changes nothing and grants no
permission; it accepts no path, URL or command. Every read ends with a
terminator line naming the unit, version and digest: text without it was
cut by the client, so read the sections one at a time.

Tools: `team_list` (the teams enrolling your credential, with coordinator
eligibility and whether a coordinator is active; read only, no session
needed), `team_join`, `team_members`, `team_profile`, `team_heartbeat`,
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

## One-command onboarding

From the configured checkout, one command performs the verified path a
member needs before it can act:

```sh
aimem teams setup Builders worker --platform codex --platform-version 0.42 \
  --model-id gpt-6-sol --model-source operator_configured --capabilities go,unit-tests
aimem teams setup Builders coordinator --platform claude-code --json
```

It checks, in order and stopping at the first blocking failure: the strict
`.aimem.json` binding and the selected credential (project-local override or
the per-hub task token; never the checkpoint token, never a fallback); the
checkout's client integration (below); the credential's identity (`/v1/access/identity`: ordinary user, scope, current
write grant, tasks enabled); the hub version and, on the first team response,
protocol version 1; the selected process and its required skills (a warning,
since they are needed at the review gate, not at join); and the Git HEAD as the
base commit. On a hub that lists enrolled teams (`GET
/v1/projects/{p}/teams/mine`) it also checks the enrollment before joining, so
"not enrolled", "enrolled elsewhere" (naming those teams) and "enrolled but not
coordinator-eligible" are reported as such with the operator handoff; an older
hub leaves that to the join's refusal. Then it joins the team, or recognizes
the session this checkout already holds, and ends in the role entry: a
coordinator gets the roster with
each member's reported model and its source, a worker announces itself
available, reads its reserved attempt and its inbox once (bounded, wait 0).
Exit status 0 means joined and verified; 1 means blocked, and the report names
the missing prerequisite or prints the operator handoff: the exact
`aimem teams provision` commands for the hub host that set the grant, create
the team or add the enrollment ([TEAM-SETUP](TEAM-SETUP.md#guided-provisioning)).
Agents cannot enroll themselves.

Session state is saved outside the checkout, under the state root in
`team-sessions/<sha256 of the checkout path>.json`, bound to the checkout,
project, hub, URL and token ID; it holds handles and the declared profile,
never the token. A repeated invocation verifies the saved handle with a roster
read and makes no second join. A session the hub reports as suspect (no
heartbeat within its window) is the restart case and is resumed, which fences
the old handle; a session still heartbeating may belong to a live process and
is only verified, unless `--resume` asks for the fence explicitly. The hub
refuses every command on a closed session and on a stale generation alike, so
a saved handle cannot tell an explicit leave from another live process. A
coordinator then joins afresh and lets the hub arbitrate: an occupied slot
means the old session is live (it must leave or be handed off; no timeout
frees the slot), success means it had left. A worker's fresh join is never
refused, so a duplicate would be silent: the command stops and
`--new-session` makes that choice explicit. `aimem teams leave` through the
CLI clears the saved handle, so the next setup joins afresh without the
question. An unconfirmed join or resume (the reply was lost or unreadable)
keeps its retry key and the exact request it sent, and the next run replays
that request first, before any read with the old handle; while such a replay
is pending, a different team or role is refused rather than sent under the
old key or joined afresh, and `--new-session` is the only way to abandon it.
The role entry (roster, heartbeat, reserved attempt, inbox) is checked as
well: a refusal there is reported as blocked with the membership kept, never
as a completed entry. A roster read that runs out of pages before showing
the command's own row is incomplete, not a missing membership, and never
leads to a second join.
A model is declared only with `--model-id` and a `--model-source` naming who
stated it; without them it stays `unknown`. A re-run without profile flags
keeps the declaration the first run saved.

The client integration step covers what
[wiring a project](INSTALL-CLIENT.md) adds: `docs/SESSION-STATE.md`, the
`SessionStart` handoff hook in `.claude/settings.json` and `.codex/hooks.json`
(either installer spelling counts), `mcpServers.aimem` in `.mcp.json`, and the
handoff instruction plus `mcp.aimem` in `opencode.json`. By default it adds
what is missing in the installers' shapes, writing the file back without a
byte-order mark and keeping every other key; `--no-repair` reports only. An
entry that exists but differs is reported and left alone, an unreadable file
is reported and never overwritten, and a project-level `Stop`, `StopFailure`
or `PreCompact` hook blocks the run (they belong to the user-level install;
`--allow-project-stop-hooks` overrides). It then lists the agent clients on
PATH (`--client-versions` also runs each one's `--version`) and, once the
selected process is known, where each client finds every required skill: a
skill is a directory holding `SKILL.md` in a location that client reads
(`.claude/skills`, `.agents/skills`, `.opencode/skills`, and the user-level
`~/.claude/skills`, `~/.agents/skills`, `~/.codex/skills`,
`~/.config/opencode/skills`), reported as found or NOT FOUND per client.
Installing skills and the user-level pieces (binary, checkpoint hooks, the
OpenCode plugin) stay with the installers.

### Continuing after a restart

`aimem teams continue [TEAM] [--fence] [--json]` is the restart command: it
verifies the binding, the credential and the hub, then the saved session (a
live one is verified as is, a suspect one is resumed with a persisted retry
key, a refused handle is reported), and then reports the duties the hub
holds for it: the reserved attempt with its assignment title and state
mapped to the next protocol step (OFFERED: accept or decline; RUNNING:
continue; BLOCKED: resume-work when the need is met; STOP_REQUESTED:
stopped; STOPPED: wait for close-stop; SUBMITTED: wait for the review), a
reconciliation line (HEAD against the base commit recorded at the last
verification, uncommitted changes, the reminder that a child process of the
old session is yours to check and that an uncertain command is retried only
with its original key), the unacknowledged inbox listed at cursor 0 (kind,
sender or hub lifecycle operation, attempt, task, excerpt) with the next
cursor and nothing acknowledged, and the roster. It never joins: a
membership that ended, whether by an explicit leave (a leave through the
CLI or the stdio MCP tool clears the saved state) or by a closed or advanced
handle, is reported, and `/join_team` is the only way back in. `--fence`
resumes a session that is still heartbeating, for the case where the old
process of your own is known to be gone. A repeated run against a live
session changes nothing on the hub. Nothing here wakes an idle agent: the
command reads once, bounded, and the agent polls `team_inbox` itself
afterwards.

### The `/join_team` and `/resume_team` entry points

Each client gets a short native entry point that calls the `team_setup` or
`team_continue` tool of the checkout's local aimem MCP server, so a member
session starts with `/join_team TEAM [worker|coordinator]` and no pasted
startup prompt and no shell command that needs the credential. The CLI
commands remain for operators and scripts and share the saved state. The entry points are one text rendered by the binary
per client (managed files carrying an `aimem teams commands` marker,
refreshed whenever the rendered text changes, never touching a file that
does not carry the marker); `aimem teams commands [DIR]` writes them,
`--check` reports only, the installers' project mode calls it, and `aimem
teams setup` repeats it on every run:

| Client | Path | Invocation |
|---|---|---|
| Claude Code | `.claude/skills/join_team/SKILL.md`, `.claude/skills/resume_team/SKILL.md` | `/join_team TEAM [ROLE]`, `/resume_team [TEAM]`; also `claude -p "/join_team TEAM ROLE"` |
| OpenCode | `.opencode/commands/join_team.md`, `.opencode/commands/resume_team.md` | `/join_team TEAM [ROLE]`, `/resume_team [TEAM]`; also `opencode run --command join_team "TEAM ROLE"` |
| Codex (project) | `.agents/skills/join-team/SKILL.md`, `.agents/skills/resume-team/SKILL.md` | mention `$join-team TEAM [ROLE]` or `$resume-team [TEAM]` (or `/skills`); also in a `codex exec` prompt |
| Codex (user) | `~/.codex/prompts/join_team.md`, `~/.codex/prompts/resume_team.md`, written only when `~/.codex` exists | `/prompts:join_team TEAM [ROLE]`, `/prompts:resume_team [TEAM]` (Codex custom prompts live only in the Codex home, not in repositories) |

`/resume_team` calls `team_continue`: it tells the agent to pass only the
saved team, reconcile before retrying anything, and take up the reported
duties in order (the reserved attempt per its state, then the unacknowledged
messages with `team_ack` only for the consumed ones, then the role's
waiting or coordinating loop). It states that the command is the explicit
step: client startup alone and an idle model do not poll or wake.

The text tells the agent to check that the server lists the tool (an older
`aimem mcp` process does not: upgrade and restart the client, never fall
back to a shell command or a broader permission), declare its platform and
only a runtime-reported model, call `team_setup`, read the report, stop on
`blocked` and show the fixes or the operator handoff (a credential the
process cannot read or decrypt means the MCP process is not the account
that installed it, never a reason to reinstall the token), and enter the
role per the [playbooks](TEAM-PLAYBOOKS.md). Without a TEAM it calls
`team_list` and offers the teams enrolling the credential (the person
still chooses). The Claude Code skill
is user-invocable only (`disable-model-invocation: true`); the Codex skill
says the same in its description, since joining is the person's decision. Client integration repair and
operator provisioning are separate increments; the command reports what it
found and how to fix it.

## CLI

The team guidance built into the binary, readable anywhere (no checkout,
hub or credential):

```sh
aimem teams context                      # index: section ids, sizes, digests, role sets
aimem teams context --role worker        # the worker's complete required set
aimem teams context --section example/submit
```

Place `role` and `profile` from the example in `join.json`:

```sh
aimem teams mine example-project                       # teams enrolling this credential; no session needed
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
