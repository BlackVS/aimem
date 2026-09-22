# Agent teams and coordinated backlog execution

Status: proposed protocol v1 for review, 2026-09-21. No implementation or
change to repository merge permissions is implied. This planning work now
precedes the access console, at the owner's request.

## Verdict and scope

aimem is a suitable home for durable coordination because it already owns the
backlog, authorization, task history and process context. Start with explicitly
assigned work for manually started agents; automatic scheduling and launching
agents are separate increments.

Implementation risk is high: task ownership under failure, session identity and
integration control are new contracts. The immediate design work has no runtime
effect. This proposal does not claim exactly-once execution of local commands.

The README describes "Session resilience and shared memory for AI coding agents."
The task implementation adds durable project work records. The proposed extension
lets one coordinator plan/decompose work and evaluate results while several
workers code in isolated checkouts, independently of their agent platform.

Inputs are authenticated requests, task revisions, process references, membership
requests, progress/results and eventual host-adapter signals. aimem owns the
coordination schema; platform adapters own launching and local execution. Missing
authority denies access; missing or stale session information must never imply
that a worker has stopped. Missing result evidence leaves work unaccepted.

## Existing contracts and gaps

Reviewed against master `42ee271` by source inspection, not a concurrency test.

| Artifact | Producer | Consumer | Contract | Failure if misused |
| --- | --- | --- | --- | --- |
| Task and history | Project task store | MCP, HTTP, board, agents | Revision-checked replacement and retry receipts | A fresh revision does not prove execution ownership |
| Assignee | Authorized task writer | Agents and board | Access user/group, not agent session | Two sessions using one user are indistinguishable as workers |
| Comments and evidence | Authenticated writers | Coordinator, reviewer, human | Durable task context | Free text is not a reliable assignment inbox |
| Token authority | Access service | Every task request | Live grants and token scope | A team role must not widen project access |
| Pinned process | Selected process repository | Session bootstrap | Exact selected assets | Coordination policy must agree with repository delivery rules |
| Work candidate (proposed) | Worker | Coordinator and integrator | Task/attempt, base commit, branch/commit, tests | Shared working trees or stale bases can corrupt integration |

Current flow is MCP task call -> HTTP with the configured task credential ->
project authorization -> task-store transaction -> durable history/receipt ->
response. See `internal/mcp/tasks.go`, `internal/server/tasks.go` and
`internal/store/tasks.go`. Clients provide neither credentials nor actor identity
as model-controlled task fields.

Three structural gaps follow directly from the source:

1. `TaskAssignee` identifies only user/group (`internal/store/tasks.go:89`).
   Introduce a separate execution session; preserve the existing assignee meaning.
2. `UpdateTask` checks revision but has no ownership predicate
   (`internal/store/tasks.go:418`). Revision checks prevent lost updates, not a
   second worker executing or replacing an existing assignment. Managed tasks
   need dedicated ownership checks, including on existing mutation routes.
3. Current MCP tools expose tasks/comments but no membership, claim or inbox
   (`internal/mcp/tasks.go:35`). Add explicit coordination operations; do not make
   agents infer authoritative commands from journal text or comments.

Existing retry receipts and server-derived actors are useful foundations. Keep
them; define retries after token/session replacement explicitly rather than
assuming existing token-bound receipt keys survive replacement.

## Proposed minimum model

The intended user flow is: start an agent client, ask it to join a named team
with a requested role through MCP, and receive the accepted role, session ID,
team roster, process instructions and inbox cursor. Tool names below are examples,
not existing commands: `team_join`, `team_members`, `team_send`, `team_inbox`,
`team_ack`, `team_leave`. Resolve readable team names within the configured project;
return stable IDs for subsequent calls. Rejoining after restart must reconcile
the prior session rather than silently create another owner of its work.

Joining puts a worker in waiting/available state. Team workers do not independently
select or claim backlog tasks, including while the coordinator is disconnected.
Only the coordinator creates an offer addressed to a specific member; that member
can accept, decline or ask for clarification. After submitting work, the worker
waits for review/another offer. It may answer questions without taking over the
sender's work. Joining a team does not change standalone pickup policy for other
projects or grant permission to edit a task outside an accepted assignment.

An agent that has not joined a team keeps today's standalone workflow: select
eligible READY work from the backlog according to project policy and dependencies,
then claim it with revision checks, one task at a time. Team-managed tasks are
excluded from standalone pickup and generic mutation, even when idle between
attempts. Joining changes the cooperative agent's workflow for that team; it does
not silently remove the underlying user's ordinary permissions on unmanaged work.
The hub enforces managed ownership, while process instructions govern whether a
session acts as a standalone agent or a waiting team worker. A clean leave with no
outstanding attempt allows an explicit return to standalone mode. Losing contact
with a coordinator or restarting does not implicitly leave the team.

Both coordinator-to-worker and worker-to-worker communication are first-class.
A message has a server-derived sender, recipient session or team, message ID,
optional task/attempt reference, thread/reply reference and body. Support questions,
answers, progress and blockers, with bounded size and project/team visibility.
Direct messages travel through the hub and remain recoverable; they do not require
agents to connect to one another. Questions do not transfer task ownership, and a
message saying "take this task" does not replace an accepted assignment operation.

MCP is the common agent-facing interface; HTTP/CLI can expose the same operations
for adapters. Durable inbox polling is the baseline. Notifications or an adapter
may prompt an inbox read, but notification delivery is not evidence that the model
has read or acted on the message. An agent waiting for an answer should persist
its question and yield or do independent work, not block a chain of synchronous
agent calls. Verify each client's idle/busy delivery behavior during the pilot.

A team belongs to one stable project instance. Initially allow one active
coordinator per team, and at most one active execution attempt per task across
teams. Keep tasks/epics as the only backlog. A team is neither an access group nor
a new authentication domain.

A member session has a server-issued ID, authenticated principal binding,
display label, declared platform/capabilities, role, availability and last-seen
time. Labels and declared capabilities are informational. Join requests cannot
self-appoint a coordinator; an authorized project operator establishes the team
and delegation policy. The permission matrix below defines operator authority;
a task write grant alone does not include team administration.
Sessions sharing a credential remain the same security principal, even with
different session IDs. Independent security isolation requires separate credentials.

An assignment references the existing task, intended session, task revision,
coordinator generation and a unique attempt ID. Explicit accept/claim creates
execution ownership atomically. A result carries that attempt, immutable commit
references, base commit, verification evidence and remaining issues. Coordination
records are separate from task content, with transactionally consistent changes.

The worker submits a result; the coordinator accepts it for review or requests
specific corrections. Neither submission nor coordinator acceptance automatically
means DONE: repository review, CI and human merge requirements still apply.
Cancellation is a durable stop request with acknowledgement, not proof that a
local process was killed.

## Reliability and authority

Use a durable event stream/inbox with monotonic cursors and idempotent commands.
Delivery may repeat. Acknowledgement records receipt, not task completion. On a
cursor gap or expired retention window, rebuild from an authoritative snapshot.
Retain task outcomes/history independently of inbox retention.

Proposed first recovery policy: missing heartbeat makes a session suspect and
halts new assignments; it does not automatically reassign its running work.
Require explicit reconciliation of the prior attempt and repository state before
handoff. An incremented ownership generation rejects stale progress/results and
stale coordinator commands at the hub. It cannot stop a disconnected agent from
editing files or pushing commits. Worktree isolation and integration checks are
therefore necessary even when hub ownership is correct.

Coordinator restart reconstructs state from the hub. Coordinator replacement
requires an authorized compare-and-swap handoff that invalidates the old
generation. Workers stop taking new work when authority or connectivity is lost;
an adapter must define how an already-running command is allowed to finish or
stopped. Never promise instantaneous revocation of local execution.

Every operation checks current project/token authority and session binding. All
routes that can alter managed task lifecycle, ownership or accepted results must
respect the same rules; generic task updates cannot be a bypass. Unmanaged tasks
retain their existing behavior. Old clients must receive an explicit actionable
refusal for unsupported managed mutations. Denied team reads must not expose
other projects or members. Human overrides are explicit and audited.

Start with polling, batching heartbeat and inbox where practical. At N idle
sessions polling every T seconds, baseline traffic is approximately N/T requests
per second; for 20 sessions at 15 seconds this is 1.33, before task writes and
separate heartbeats. These are design estimates, not measurements. Benchmark
SQLite writes/latency and apply jitter/backoff. The first version supports explicit
inbox reads at safe points and bounded waits; idle responsiveness depends on the
client. Avoiding an LLM turn per empty poll requires verified client support or a
later adapter, and is not a first-version guarantee.
Host/client behavior needs a capability probe before promising background polling
or wakeup. Protocol access alone is not an execution supervisor.

## Parallel work and integration

The optional supervisor boundary is specified in
[DESIGN-agent-supervisor.md](DESIGN-agent-supervisor.md). It adds runtime
observations and supported adapter control without replacing aimem ownership.
Manually started members and the core coordination pilot remain independent of it.

Each coding attempt uses a separate worktree or clone and a recorded base commit.
The coordinator selects tasks with independent outcomes, dependencies and likely
overlap made explicit. Shared generated outputs, migrations and common files are
integration dependencies even when task titles look unrelated.

The current repository permits one active PR at a time. Preserve that rule for
the first pilot: workers can produce candidates concurrently, while one integrator
rebases, verifies, opens and delivers PRs serially. A later change to allow several
open PRs requires an explicit repository-policy decision. External-review labels,
reviewed-head validity and human merge ownership continue to apply.

The repository's single `docs/SESSION-STATE.md` belongs to the integrator.
Workers put task-specific handoffs in aimem and their isolated local state; they
must not concurrently overwrite the integrator's handoff. Coordinator context
must be reconstructible without relying on any one platform's conversation.

## Delivery plan

Priority assessment: P1 for this owner-selected design, M for the bounded design
pass; the overall initiative is XL and must be split. The following are proposed
increments, not READY implementation tasks or estimates of elapsed time.

| Order | Increment | Exit evidence | Initial complexity |
| --- | --- | --- | --- |
| 1 | Freeze protocol/state machine and compatibility decisions | Reviewed schemas, permission matrix, failure cases and bounded pilot contract | M |
| 2 | Team/session registration and roster | Two sessions under one principal distinguishable; role escalation and cross-project reads denied; restart/expiry visible | M |
| 2a | Supervisor authority and identity design | Reviewed observation, delegation and recovery boundaries; no runtime changes | M |
| 2b | Early client capability probe | One backend measured first, then a second; attach, idle/busy delivery, pending requests and reconnect limitations recorded | M |
| 3 | Structured messages and durable inbox | Coordinator/member and member/member request/reply survive retries and reconnects with correct routing | M |
| 4a | Protected task assignment through storage/HTTP | Concurrent claims yield one owner; generic update bypass denied; disconnect never silently releases ownership | M |
| 4b | Result/recovery operations and MCP workflow | Stale results/coordinator rejected; correction cycle, safe manual recovery and MCP parity pass | M |
| 4c | Communication audit export and analysis | Complete task/team timelines, delivery latency, retry/failure accounting and redaction checks | M |
| 5 | Process guidance and interoperability pilot | One coordinator and two workers on different platforms finish independent tasks with isolated worktrees and serial integration | M |
| 6 | Priority/complexity-aware pickup | Reuse existing metadata/pickup backlog tasks; explicit assignments take precedence and selection reason is recorded | Reassess existing tasks |
| 7a | Runtime observations and one structured adapter | Sourced waiting reasons and exact-request delivery in a supported manually started session | Separate M slices |
| 7b | Supervisor recovery and permission policy | Reconciliation before retries; scoped decisions with independent security review | Separate M slices |
| 8 | Richer team UI and additional adapters | Separate proposals justified by measured pilot limitations | Separate investigation |

The access-console work is not a prerequisite: existing access administration
can provision pilot credentials. Structured priorities are useful for automatic
pickup, but explicit assignment allows the first pilot without waiting for them.
Do not build a generic scheduler, broker or remote shell in the first version.

## Protocol v1 decisions

These are proposed implementation contracts, not descriptions of shipped routes.
Use manually started agents, explicit coordinator assignment, one coordinator per
team, manual recovery of uncertain work and serial integration. Platform capability
probes start before adapter implementation and inform the pilot; no particular
client's background behavior is assumed.

### Permission matrix and session lifecycle

| Operation | Required authority in addition to enabled project/tasks |
| --- | --- |
| Create/configure team, enroll users, designate coordinator users, force recovery | Existing hub admin or trusted local operator |
| Join as worker/reviewer | Current ordinary write token for project and user enrolled in team |
| Join as coordinator | Same checks plus designated coordinator user and atomic acquisition of vacant coordinator slot |
| Read roster/messages or send/ack | Active session bound to caller's user/token, current grant and team enrollment |
| Assign/review result/request cancellation | Current coordinator session and generation |
| Accept/progress/submit/acknowledge stop | Assigned worker session and current attempt generation |
| Ordinary task editing outside teams | Existing authorization contract unchanged |

Admin setup has CLI/HTTP operations; agent MCP exposes no admin credential or
self-enrollment bypass. Initial enrollment names individual access users; group
enrollment is deferred. Read-only and legacy writer credentials cannot join.
Human admins retain audit access. Readable team names are unique within a stable
project instance, resolved to IDs by join; team rename does not change IDs.

Session fields: `id`, `team_id`, `user_id`, `token_id`, `generation`, `role`,
`label`, `platform`, `model`, `capabilities`, `availability`, `last_seen_at`,
`state`, `profile_revision`.
The server supplies identity and timestamps. Each join has a retry key; repeating
that join returns the same session, while an intentional second agent uses a new
key. V1 caps a worker at one reserved/running attempt; the coordinator delegates
coding instead of holding a worker attempt itself. Reviewer role has no assignment
or result-acceptance privilege in v1; it can send review evidence.

Normal disconnect changes only liveness. Explicit resume uses the old session ID
and expected generation, authenticates the same user/token and increments generation
atomically, invalidating the old session handle. Transfer to a replacement token
requires operator recovery after checking the old execution has stopped. A session
ID/generation is a concurrency handle, not a secret or an isolation boundary from
another agent sharing the same credential. Distinct tokens are needed for that.
Resume also advances the bound active-attempt generation, or coordinator generation
for that role, in the same transaction and returns the outstanding-work snapshot.
It does not re-execute a command or release an assignment. The returning client
must reconcile any local command still running before continuing work; it cannot
infer that the old process stopped merely because resume succeeded.
Heartbeat every 30 seconds and suspect after 120 seconds are initial configurable
defaults, using hub time. Explicit requests may refresh last-seen; a background
transport heartbeat never proves model progress. Leave with active work enters
draining and requires stop acknowledgement/recovery before closing.

### Agent identity and assignment suitability

Every member introduces itself with a readable label, client/platform version,
reported model provider/identifier/version (or `unknown`), and relevant capabilities.
Model metadata includes a source: `runtime_reported`, `operator_configured`,
`agent_reported` or `unknown`, plus observation time. Never infer the model from
the client name or claim a model the runtime does not expose. A provider alias
does not promise an immutable model version. Tools, repository/language familiarity,
test/browser capability and operator-configured limits can be declared separately.
Declarations describe suitability; they confer no permission or verified skill.

The coordinator sees this profile alongside availability, assignments and prior
task evidence. Each offer records the chosen member's profile revision, declared
model, task requirements and assignment rationale. Priority/readiness/dependencies
still come first; model/capability fit helps choose among eligible available members.
Examples include requesting a browser-capable worker for UI validation or a
different model family for a review when project policy requires it. Do not bake
a fixed model leaderboard into aimem; model quality/cost assumptions belong to
explicit, revisable project/operator policy with evidence.

Coordinator assignment guidance is to choose the least costly suitable available
member, avoiding both a weak model on complex work and unexplained expensive-model
use on trivial work. Assess task complexity and required capabilities first, then
compare the roster. If model suitability or task complexity is unknown, clarify or
decompose before offering. L/XL tasks follow the process split guidance. Record
the assessment and choice with the offer; urgency or lack of a cheaper suitable
member can justify a stronger model, with an explicit reason.

V1 uses this coordinator decision and recorded rationale rather than a new model
scheduler. Project-configured model guidance may provide supported complexity
ranges and relative cost preferences; it is not a universal provider ranking.
The offer records assessed complexity and requirements against the task revision;
once structured task estimates exist, derive this snapshot from that revision.
Do not maintain a second independently editable task estimate. A worker can decline
an offer it cannot safely complete, but cannot substitute another backlog task.

`team_update_profile` updates informational fields with expected profile revision;
it cannot change role, identity or grants. A model switch during a session is
recorded as a profile-change event. Keep the assigned profile snapshot immutable,
annotate active work/results with the change, and notify the coordinator so it
can reassess fit. It does not automatically cancel or reassign the task. Record
unknown model information honestly in results and post-analysis, rather than
attributing outcomes to a guessed model.

### Structured communication

Use a structured envelope; natural-language content belongs only in bounded text
fields. No terminal scraping, keystroke injection, transcript parser or remote
terminal app is needed. MCP and HTTP/CLI serialize the same schema.

```json
{
  "protocol_version": 1,
  "session_id": "SESSION_ID",
  "session_generation": 1,
  "idempotency_key": "question-unique-key",
  "recipient": {"kind": "member", "id": "RECIPIENT_SESSION_ID"},
  "kind": "question",
  "task_id": "TASK_ID",
  "attempt_id": "ATTEMPT_ID",
  "reply_to": null,
  "payload": {"text": "Which API contract should this implementation use?"}
}
```

Request excludes sender identity, sequence and time; the response supplies those
from authentication and committed storage. Kinds are `question`, `answer`,
`note`, `progress`, `blocker`, and `review_feedback`. All have `payload.text` and
optional typed `refs` using existing task-reference objects; unknown fields/kinds
are refused. `question` may include an advisory deadline; `answer` requires
`reply_to` pointing to a question. Message receipt, answer and completed work are
different states. A progress/blocker message is informational; changing execution
state requires the corresponding typed attempt operation.
Task/attempt references are optional, but when supplied must exist in the team's
project and match each other. Reply targets must be visible in the same team.

Recipient is one member session or the team. V1 messages are team-visible;
direct recipient means inbox routing, not private conversation. The coordinator
can inspect task discussions. Explicit inbox recipients are materialized at send
time for broadcasts; later joiners may read history but get no retroactive delivery.
Sender and all current members can read team history; revoked enrollment loses
access. Task outcomes must cite relevant conclusions so a successor need not scan
unrelated chat. Do not send secrets in message bodies.

Assignments, acceptance, cancellation, results and recovery are typed commands,
not arbitrary chat kinds. A question cannot authorize editing another worker's
files or transfer ownership. A result has `attempt_id`, `generation`, task revision,
base/candidate commit references, verification references, summary and limitations;
the server stores it immutably and emits a result event. Coordinator feedback
references the result ID. Each correction gets a new attempt after the old attempt
is closed; the prior result remains readable.

### Persistence, retries and polling

Store teams, enrollment, sessions, attempts, messages, per-recipient delivery/ack
state, coordinator generations and command receipts in the project's task SQLite
database. They are excluded from memory curation/sync. Foreign keys and a unique
active-attempt constraint on task ID enforce one reservation/attempt across teams.
Storage migration and backup coverage are part of the first storage increment.
Task mutation, attempt transition, receipt and resulting inbox event commit in the
same database transaction. Access-grant checks remain at the existing request
authorization boundary; check current enrollment/session generation again inside
the project transaction. Do not introduce a pretend cross-database transaction.

Every mutating command has an idempotency key, scoped to authenticated principal,
operation and team/session target. Same key/different payload conflicts. Validate
current authorization before returning a receipt. A retry of an already committed
command returns its original result, never replays effects; a stale generation
cannot authorize a new command. After uncertain delivery or credential replacement,
read the snapshot/result by stable ID before attempting a new command.

Inbox reads use a cursor and return ordered events, `next_cursor`, server time and
`has_more`. Reading does not acknowledge. Ack identifies delivered message IDs and
is idempotent; it must not acknowledge unseen messages just by moving a cursor.
Reconnect begins from last acknowledged state and may redeliver. Never interpret
an empty page as completion of an assignment. Long-poll wait is bounded to 25
seconds; initial page size 50, maximum 100. Apply request cancellation on disconnect.

V1 retains messages and receipts without automatic expiry. Impose explicit limits:
32 KiB message body, 64 KiB command body, 10,000 messages per team and 100 active
sessions per team (configurable). Full storage returns a clear refusal, never
drops unacknowledged messages; operator exports/raises limits pending a separate
retention design. Enforce quotas only for new commands after receipt lookup.
Do not permit team deletion while active attempts exist. Project drop/merge must
refuse projects with coordination state until a separate lifecycle design exists.

### Communication logs and improvement evidence

Structured audit records are part of membership, messaging and assignment storage
from their first increment, not deferred until an analytics UI. Each accepted
command records event ID, team/project instance, server timestamp and sequence,
authenticated user/token IDs (never token value), session/generation, operation,
request correlation ID, affected message/task/attempt/result IDs, previous/next
state or revision, outcome, protocol version, server version and selected process
commit when available, plus the applicable agent-profile revision and reported
model/source snapshot. Message bodies are stored once in the message record;
audit entries reference them. Preserve authorship on both sides of a conversation.

Record send acceptance, recipient delivery, acknowledgement, answer and result
disposition separately. Delivery means an inbox response was served, not that a
model consumed it. Acknowledgement is an explicit client report, not proof of
understanding. Client timing is optional and untrusted; use server timestamps for
elapsed-time metrics. Repeated deliveries/requests need their own counters or
observations without duplicating the accepted domain event or its side effects.

Accepted business events commit with state changes. Rejections, malformed requests,
authorization failures and storage failures also produce bounded structured server
logs with correlation ID, outcome/error code, duration and safe identity/target
IDs when known. Do not log authorization headers, session credentials, raw rejected
payloads or message text again. If the database is unavailable, no claim of a
durable project audit is possible: use the existing server log sink and report the
failure. Read-delivery observations may fail independently; record
that limitation and never treat a missing observation as proof of non-delivery.
Log-write health/counters must expose dropped diagnostic records.

Provide authorized team/task/session/time-range timeline reads and JSONL export,
including a schema-version header, applied filters, snapshot upper sequence and
export completion marker. Enforce live access for every export page. Message
content is opt-in; metadata-only exports are the default. Operator diagnostics
join to the project timeline by correlation ID when available. Document distinct
retention for durable team records and rotated diagnostic logs, and report gaps
explicitly. Preserve accepted audit records without automatic purge in v1; size
limits must surface operational pressure rather than silently discard history.

Initial analysis reports unanswered questions, assignment-to-accept delay,
question-to-answer delay, time blocked, repeated deliveries, rejected stale writes,
reassignments and correction cycles. These measure process behavior, not individual
agent quality. Do not invent token usage/cost from message length; any future usage
measurements must identify their reporting source. Team/task summaries can suggest
process or aimem backlog improvements with event references. They cannot silently
modify the selected process, change policy or train a model. Improvements pass
through ordinary review and process-version selection.

### Assignment state machine and compatibility

The schema 17 [storage foundation](TEAM-ASSIGNMENT-STORAGE.md) implements offers,
acceptance, decline/withdraw and managed-task protection first. The subsequent
[HTTP primitives](TEAM-ASSIGNMENT-HTTP.md) expose those operations. The remaining
lifecycle follows separately. The [result storage](TEAM-RESULT-STORAGE.md) increment
implements internal submission and accept/return disposition, including the
terminal RETURNED attempt state for corrections. These primitives do not make
the full agent execution workflow available.

`OFFERED -> RUNNING -> SUBMITTED -> ACCEPTED` is the normal attempt path.
OFFERED reserves the task before the worker accepts. Worker decline or coordinator
withdrawal before acceptance closes the offer. Only the intended current worker
session can accept; the acceptance transaction updates task state to IN_PROGRESS.
Assignment checks task READY, expected revision, all dependencies DONE and the
worker's availability/current authority. No dependency means no dependency gate.
The hub also checks that the caller is the current coordinator and acceptance is
by the exact offered member. There is no worker `claim_next` or self-assignment
operation. Coordinator-only assignment is enforced even when a worker can read
the backlog; model-fit assessment is documented process, not a server attestation
of model competence.
Dependencies must resolve in the same project for v1 managed work. Coordinator
must supply a reason to handle an exceptional task through an audited operator
override instead of silently skipping these checks.

At first offer, mark the task managed by that team persistently, even after an
attempt closes. Submitted results move it to REVIEW and keep the reservation
until coordinator disposition. Acceptance releases execution ownership but keeps
the managed task in REVIEW pending ordinary delivery gates. Coordinator's finalize
operation records merge/validation evidence before DONE; policy cannot mechanically
prove the quality of evidence. Returning for fixes closes the old attempt, moves
the task to READY and requires a new offer/accept. A blocked worker retains its
attempt; task BLOCKED and structured blocker event commit together, with explicit
resume back to IN_PROGRESS.

The [cooperative work-control storage](TEAM-WORK-CONTROL-STORAGE.md) increment
makes the intermediate states explicit: blocked work has attempt BLOCKED, and
cancellation of RUNNING or BLOCKED work enters STOP_REQUESTED. Worker acknowledgement
enters STOPPED, still reserved; the current coordinator's `close-stop` command then
closes the attempt as CANCELLED and requeues the managed task to READY. This
clarifies the earlier two-phase description without granting process-control or
forced-recovery authority. Suspect sessions never expire ownership automatically. Forced recovery needs
admin authority, expected attempt/coordinator generations and recorded reconciliation
evidence; closing the old attempt fences future writes. It cannot undo external
commands. A delayed old result is rejected as stale and may be attached only as
non-authoritative evidence by the coordinator.

The [operator recovery storage](TEAM-RECOVERY-STORAGE.md) increment makes forced
closure explicit: RUNNING, BLOCKED, STOP_REQUESTED or STOPPED becomes terminal
RECOVERED and the managed task returns to READY. It requires the original assigned
worker handle, separately observed current worker-session generation, current
coordinator-slot generation and task revision. An admin records an explicit stopped
affirmation, runtime/process and worktree reconciliation, reason and evidence refs.
The hub validates and preserves this assessment; it does not prove execution stopped.
Offers, submitted results and terminal attempts cannot use this path. Token
rebinding, coordinator handoff and recovery client operations remain deferred.

The [session rebinding](TEAM-ASSIGNMENT-STORAGE.md#session-rebinding) increment
implements the resume rule above for workers: a worker resume carries its reserved
non-offer attempt to the new session generation in the same transaction, with an
immutable rebind record and audit event, so a restarted client commands its own
work with the returned handle. Offers stay bound to the generation that received
them, leave keeps the reservation for operator recovery, and a storage read
returns the calling session's reserved attempt as the outstanding-work snapshot.
Rebinding never releases work or proves a local command stopped.

For managed tasks generic PUT/archive is refused with `409 managed_task` and a
pointer to coordination operations, including for admins; explicit audited admin
override is a separate operation. Comments remain available under existing write
authorization but cannot change ownership or accepted results. Coordinator edits
to managed task content use a dedicated revision-checked operation, allowed only
without a reserved attempt; changing scope during execution requires stop and a
new offer. Unmanage is operator-only with no active attempt and explicit audit.

Team responses advertise `protocol_version: 1` and supported operations. Old hubs
return unavailable; clients must not emulate claims with generic task writes.
New clients reject unsupported versions. Existing unmanaged task routes and data
remain compatible. Keep `TaskAssignee` as access-user/group responsibility; session
ownership is a separate projection. New board projections are later UI work.
Task GET/list responses add a read-only optional `coordination` projection with
team ID and active attempt ID/state when present; it is derived from coordination
records, never accepted through generic task content. Standalone clients skip all
managed tasks, including READY tasks between attempts. Older clients that ignore
the projection receive the actionable managed-task refusal and must skip/re-triage,
not retry a generic update indefinitely. Dedicated board rendering is deferred.

### Proposed API surface

Under `/v1/projects/{project}/teams`, admin routes create/configure teams and
enrollment. `/teams/{team}/join`, `/members`, `/messages`, `/inbox`, `/ack`,
`/resume`, `/leave`, `/profile`, and `/heartbeat` implement membership and messaging.
Read operations use GET; command operations POST. Configure operations use PUT
with expected revision. IDs in these suffixes are relative to the project prefix.
Session handle travels in command body or validated read query, never as authority.

`/teams/{team}/assignments` creates offers; `/assignments/{attempt}/accept`,
`/decline`, `/withdraw`, `/progress`, `/block`, `/resume-work`, `/submit`, `/review`,
`/cancel`, `/stopped`, `/close-stop`, and `/recover` define typed transitions. `/handoff` transfers
coordinator ownership (current coordinator to an eligible designated session, or
admin with evidence after loss). The target has no active worker attempt; handoff
changes its role and coordinator generation atomically. Existing worker attempts
survive handoff; their issuing coordinator generation remains audit data, while
new coordinator commands must match the team's current generation.
`/tasks/{task}/edit`, `/finalize`, `/unmanage`
under the team prefix handle managed lifecycle. Enforce identical authorization
and invariants whether a call comes from MCP, HTTP or CLI; pin OpenAPI to routes.
`GET /teams/{team}` returns the authoritative recovery snapshot; `/events` and
`/export` provide filtered audit reads and export pages. All suffixes in this
section are under `/v1/projects/{project}`, including the team prefix on assignment
and managed-task operations. Protocol/schema tests must pin actual route spelling
when each increment lands; none of these proposal routes exist yet.

MCP tool families mirror these operations with `team_` prefix and explicit JSON
schemas. The local MCP bridge resolves project credentials exactly as task tools
do today. Session IDs are returned to the caller for subsequent calls; secrets
remain in the existing credential store. Add only implemented tools at each stage
and advertise assignment readiness only once its full workflow is usable.

### Verification before enabling a pilot

The pilot must cover conflicting claims, worker crash during execution, old worker
returning after handoff, coordinator crash/handoff, token revocation, duplicate
commands/results, cross-project denial, legacy mutation attempts, and a change
to the integration base after a worker finishes. Success means independently
verified accepted candidates and correct recovery, not merely visible team members.

Test operation races transactionally, including offer/offer, accept/withdraw,
submit/cancel, resume/old progress, coordinator handoff/assignment, and generic
PUT versus assignment. Prove rollback leaves no orphan event or reserved task.
Exercise changed enrollment/grants/token state, quota retries, broadcast routing,
typed-message validation and old-client refusal. Run a protocol fixture without
an LLM first, then two actual client platforms. Record explicit-read, bounded-wait,
busy delivery, idle wakeup and restart behavior separately. A failed wakeup probe
limits the pilot to manual reads; it does not justify a terminal-control workaround.
