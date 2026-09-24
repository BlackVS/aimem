# Optional agent supervisor

Status: proposed design amendment, 2026-09-22. Complements
[agent teams](DESIGN-agent-teams.md); introduces no routes, tools, schema fields,
runtime permissions or deployment. The current implementation through PR67 has
team administration, session interfaces and message storage. Message transport,
execution assignments and supervision still require their own increments.

## Purpose and ownership

Help a coordinator notice why a member cannot continue, deliver an answer through
a supported structured interface, and recover runtime connections. Aimem remains
the authority for durable coordination. A supervisor is optional host-side software;
workers started manually can continue using the same team protocol without it.

| Owner | Authoritative records or actions | Boundary |
| --- | --- | --- |
| Aimem | Tasks, enrollment, sessions/generations, messages, future attempts/results and recovery authorization | Only authenticated typed commands change coordination state |
| Coordinator | Decomposition, assignment choice, evidence assessment and escalation within delegation | Role and model declarations do not widen access or override repository gates |
| Supervisor | Backend bindings, delivery checkpoints, observed runtime state and authorized local control | No independently editable assignment state or implicit claims |
| Adapter | Version-specific structured backend operations and observations | Reports unsupported behavior; never invents delivery or stop guarantees |
| Operator | Project delegation, host execution policy and exceptional recovery | Approval is explicit, scoped, revocable and recorded |

The supervisor may cache an aimem snapshot with its revision and freshness. It
must refresh authority before actions; a cached assignment cannot authorize new
work while disconnected. Runtime telemetry can remain in bounded local logs.
Decisions, useful failure summaries and task outcomes belong in durable aimem
records with evidence references. Do not copy every tool event into memory.

## Identity, credentials and delegation

Keep these identifiers distinct; none is a credential:

- Stable project instance, team ID, aimem member session and generation identify
  the coordination context. Coordinator generation fences coordinator commands.
- Task ID/revision and attempt ID/generation identify the accepted scope and owner.
- Supervisor installation ID and process-instance epoch distinguish a restart
  from continuation of the same event producer.
- Backend type/version, backend session ID and turn/request ID identify the
  runtime target. A turn finishing does not complete an aimem task.
- Workspace identity and recorded base commit identify the isolated candidate;
  a path is only a host-local locator, not sufficient identity or isolation.

An operator-approved binding maps those identities to one allowed backend and
workspace. Rebinding or restarting requires reconciliation, not a guessed match
by readable label. Store delivery checkpoints and bindings durably enough to
recover, but retrieve current aimem authority before resuming effects.

Existing ordinary project tokens retain their current grants and session binding.
A supervisor must not borrow a worker's token and masquerade as that member.
Until explicit delegation exists, only the member's authenticated client can
make member-bound calls; the supervisor has no new hub authority. An initial
adapter fixture can exercise local transport without adding production delegation.

A production integration needs a separately reviewed delegation contract: an
authenticated supervisor principal, operator-granted project/team/member binding,
allowed operations, expiry and revocation. Hub checks both live principal access
and current delegation on each request. Observing or relaying a request is not
authority to submit worker results, acknowledge model understanding, administer
teams or recover attempts. Audit distinguishes initiating member, acting service
and authorizing operator/policy. Do not add an unchecked actor override to tools.

Host control is a separate boundary from hub access. The local control endpoint
must authenticate callers and restrict them to the configured workspace/backend;
possession of a project task token does not grant remote command execution.
The later integration design must freeze concrete credential storage, endpoint
exposure and delegation operations before exposing a supervisor service.

## Runtime observations

Avoid one flat state that conflates independent facts. Track process state,
connectivity, pending requests and current tool/turn separately from aimem task
state. Pending requests can coexist: input, permission and waiting for a peer are
distinct reasons. Idle, completed turn, dead process and suspected stall are
observations, not assignment transitions. Heartbeats establish contact, not progress.

A normalized observation envelope needs a version, event ID, producer instance
epoch and sequence, source backend/version, backend session/turn/request IDs,
bound aimem context when known, observation time and receiving time. Optional
attempt/workspace references must match the binding. Unknown association remains
unknown; it cannot be attached to whichever task was most recently active.

Consumers deduplicate by producer epoch and event ID, order within an epoch and
surface gaps. Reconnect reconciles a fresh backend snapshot and aimem snapshot;
it does not treat a sequence reset as continuous history. Stale-generation events
may be retained as non-authoritative diagnostics but cannot replace current state.
Expose source, last-observed/received times and configurable freshness thresholds.
Unknown, unsupported, disconnected and stale are distinct; none means completed.
Client clocks are untrusted and cannot determine ownership or approval expiry.

Payloads are bounded typed data. Use safe identifiers, reason/error codes and
artifact references by default. Do not send raw terminal transcripts, environment
variables, secrets, full commands or permission payloads to shared logs. Sensitive
operation details stay in the authorized local policy boundary; shared audit may
carry a nonsecret operation fingerprint and decision reference. Collection limits,
backpressure, dropped-event counters and diagnostic retention are explicit policy.

## Delivery, questions and permissions

The normal flow is durable aimem message -> supervisor checkpoint -> supported
adapter delivery -> correlated outcome. Sending, backend acceptance, a model's
explicit acknowledgement, answering and completing work are separate events.
An adapter receipt must never fabricate the member's acknowledgement. If delivery
is uncertain, reconcile the backend request before resending text that could start
another execution. Queue while busy only when the backend contract proves safe
queuing; otherwise expose waiting/manual-read behavior.

Factual input can be resolved asynchronously from task context, repository and
aimem evidence, then another member or coordinator. Preserve sources and freshness,
surface conflicts, bound delegation depth/retries/time and detect circular waits.
Substantial investigation gets its own coordinator-issued attempt. A question does
not authorize taking over another worker's files. New policy decisions need the
appropriate decision authority; an old memory entry cannot silently make them.

Bind a reply to the exact outstanding request, backend session/turn and aimem
generation. Reject expired, cancelled, already-resolved or mismatched requests.
An unresolved human escalation carries the question, task, evidence, attempted
resolution, options and impact. A negative answer or empty valid value is still
an answer; resolution must not rely on truthiness of text.

Permission requests follow operator policy, not semantic question answering.
Delegation covers an exact operation class, arguments/resource scope, resolved
working directory, relevant environment constraints and policy revision. Decisions
bind to the pending request and current session generation, with expiry and live
revocation checks. Changed operations require a fresh decision. Unsupported request
formats escalate instead of applying a best-effort approval.

Builds and tests execute repository code; a worktree is not a sandbox. Command
names alone cannot establish safety. Enforce host filesystem/network/credential
boundaries appropriate to the delegated operation and retain redacted decision
provenance. Neither a coordinator nor an LLM can expand operator delegation.
Existing human merge, review and release requirements continue to apply. Automatic
permission handling is a separate security-reviewed increment, not enabled here.

## Recovery and failure behavior

Reconcile aimem attempt state, backend request status, worktree and surviving local
commands before retry, resend or restart. Retry only a known idempotent request
under its original identity; uncertain external effects require evidence or operator
escalation. A backend disconnect can leave its child process running.

Use bounded recovery attempts with recorded action, reason and outcome. Stop and
reassignment follow the existing stop-acknowledgement/generation contract. Missing
heartbeat, DEAD or STALLED cannot release ownership. Fencing prevents stale hub
writes, not local process effects; verify stop/reconciliation before another worker
executes the scope. Supervisor restart grants no new lease or execution authority.

## Capability probe and delivery order

No vendor-specific event names or API support are asserted here. Probe exact
installed client/backend versions with disposable structured fixtures. Record
supported, unsupported and untested separately for attach versus spawn, explicit
read, idle wakeup, busy delivery, input/permission correlation, interrupt, reconnect
and event replay. Registering through MCP does not prove external control of an
arbitrary existing CLI session. Start with one backend, then a second to establish
interoperability. If attachment is unsupported, report the limit; do not silently
replace a user's session or fall back to terminal input.

The [initial capability probe](AGENT-CAPABILITY-PROBE.md) records measured Windows
Codex/OpenCode/Claude transport behavior and its untested boundaries. It introduces no
production adapter or delegation. The [Claude channel probe](CLAUDE-CHANNEL-PROBE.md)
measures idle wake-up through Claude's documented channel mechanism and recommends the
smallest opt-in integration.

1. Review this authority/identity amendment and run the early capability probe.
2. Complete core messaging, fenced assignments, results/recovery and audit. Add
   coordinator question/escalation guidance through the normal process reviews.
3. Run the manual cross-platform coordination pilot independently of supervision.
4. Add runtime observation and one structured adapter as separate bounded slices.
   Production delegation must be designed and reviewed before live hub integration.
5. Add bounded host recovery and narrowly delegated permission handling separately;
   broader launching, more adapters and dashboards require later justified tasks.

No terminal scraping, PTY/tmux control, automatic fleet launch, blanket approval,
schema migration or deployment is part of this amendment. Existing clients keep
their protocol-v1 behavior. Future optional fields/operations need explicit schema
and capability negotiation: never send new fields to today's strict decoders or
emulate unavailable coordination with generic task writes.

Design acceptance is a documented walkthrough of lost delivery, supervisor restart
with a live child process, stale input/approval, revoked delegation, conflicting
answers, unknown backend association and an unsupported attachment. Each must
preserve the authority boundaries above. Runtime increments require executable
fixtures at those boundaries before claiming unattended operation.

| Walkthrough input | Required outcome under this design |
| --- | --- |
| Backend accepts a message but its response is lost | Retain uncertain checkpoint, query the correlated request; no blind execution replay or fabricated member ack |
| Supervisor restarts while a worker child process survives | Rebuild bindings and inspect process/worktree plus aimem attempt; retain reservation until reconciled |
| An input or approval arrives after turn/generation changes | Reject the stale response; the current pending request needs its own decision |
| Operator revokes delegation during disconnection | Cached delegation cannot authorize the next action; refresh live authority and expose denial |
| Two references disagree about required compatibility | Preserve both references and escalate the unresolved decision; do not synthesize permission or silently change policy |
| Backend event has no trustworthy aimem binding | Record unknown association; do not attach it to a recent task or change that task's state |
| Installed client cannot attach to an existing session | Record unsupported capability and use explicit member reads; no replacement process or terminal-control fallback |
