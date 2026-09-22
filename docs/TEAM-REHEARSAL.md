# Team protocol rehearsal

A scripted fixture, without a model, that drives one coordinator and two
workers as three distinct principals through the pilot scenarios of the
[agent-team design](DESIGN-agent-teams.md) ("Verification before enabling a
pilot") on an isolated hub. Agent operations travel over the same MCP wire
real clients use (`/mcp` with a bearer token, the `team_*` tools); operator
actions use the admin HTTP routes; the audit export is the evidence. It runs
in CI with the ordinary test suite:

```sh
go test ./internal/mcp -run TeamRehearsal -v
```

The hub is a fresh registry in a temporary directory behind the real handler,
with tasks enabled on two projects, three ordinary users holding project-scoped
tokens, and one admin token. Nothing touches a production hub, and no client
process or model is started. The test logs one line per scenario and writes the
complete JSONL export to its temporary directory.

## Scenarios

| # | Scenario | What the fixture proves |
| --- | --- | --- |
| 1 | Three joins, roster | Three principals join with honest profiles; the roster lists all three; no user or token identifier crosses the wire. |
| 2 | Direct question and answer | A worker asks a peer; the peer reads its inbox, acknowledges, answers with a source and `reply_to`; the asker reads and acknowledges. |
| 3 | Assignment collision | A second offer for a reserved task is refused; an offer to a worker already holding a reserved attempt is refused. |
| 4 | Complete loop | Offer, accept, progress message, submit with evidence, review accept, finalize DONE. |
| 5 | Worker restart | Resume advances the session generation; the reserved attempt follows; a command with the old handle is refused. |
| 6 | Duplicate commands and results | Replaying a submit with its key returns the original result; re-issuing it under a new key is refused. |
| 7 | Coordinator loss | An admin hands the slot to a designated member with reconciliation evidence; the old coordinator handle is refused; the successor reviews at the new coordinator generation. |
| 8 | Token revocation | The revoked token is refused; an operator rebinds the session to a replacement token of the same user; the reserved attempt follows the new generation. |
| 9 | Cross-project denial | A token scoped to another project can neither join nor read the team. |
| 10 | Legacy mutation | Generic task update and archive on a managed task are refused with `managed_task`, for ordinary and admin credentials alike. |
| 11 | Integration base change | A rework decision returns the first result; a new offer, resubmission on the new base commit and acceptance follow. |
| 12 | Operator recovery | An abandoned running attempt is recovered with reconciliation evidence (task back to READY); the task is released from management. |
| 13 | Audit export | The export at one snapshot completes and names every operation above, with delivered and acknowledged delivery records. |

Each step asserts the hub's response, not a log line: refusals are refusals
from the hub through the MCP dispatcher, and state changes are read back.

## What it does not prove

- **Model behavior.** No model reads a message, decides or writes code. The
  fixture proves protocol semantics and authority, not that an agent follows
  the [playbooks](TEAM-PLAYBOOKS.md).
- **Client transport.** No Claude, Codex or OpenCode process is involved. The
  [capability probe](AGENT-CAPABILITY-PROBE.md) measured explicit reads,
  bounded waits, retry, acknowledgement, interrupt and restart per client and
  observed no idle wakeup; the manual cross-platform pilot must record
  explicit-read, bounded-wait, busy delivery, idle wakeup and restart behavior
  separately with real clients.
- **Concurrency under load.** Races are covered by the storage tests
  (offer/offer, accept/withdraw, submit/cancel, resume/old progress, handoff
  versus assignment); the rehearsal runs the scenarios sequentially.
- **Deployment.** The hub is in-process. Installer, service and upgrade
  behavior are release gates, not rehearsal scope.

## Documented limits the rehearsal relies on

These statements are part of the increment's acceptance criteria; the
rehearsal exercises the mechanisms behind them and the sources state the
limits.

**Offline execution limitations.** Liveness proves neither model progress nor
that a process stopped; a bounded wait is one request, not a client loop; no
idle wakeup is provided; resume fences old handles but cannot stop local
commands, so a member reconciles its own execution before any retry
([quickstart](TEAM-AGENT-QUICKSTART.md), [session storage](TEAM-SESSION-STORAGE.md),
[playbooks](TEAM-PLAYBOOKS.md)). Scenarios 5 and 8 show the fencing; nothing in
them stops a process.

**Observations never finalize or release an attempt.** The hub is the sole
authority for tasks, assignments, results and recovery; an adapter or
supervisor observation (idle, turn completed, dead, stalled) or a supervisor
restart carries source and freshness and never changes an attempt
([supervisor design](DESIGN-agent-supervisor.md), "Authority" and "Recovery and
failure behavior"). In the rehearsal every attempt state change is a
coordinator review or finalize, a worker command, or an admin recover, handoff,
token replacement or unmanage with recorded reconciliation; the export lists
exactly those operations.

**Reconciliation distinguishes three things.** The backend session or turn
(what a client runtime is doing), the aimem session and generation (which
handle may command an attempt) and surviving local work (a worktree or child
process that may still be running). Restart or resume changes only the second;
the first and third must be reconciled by the member or operator before a
retry, a reassignment or a recovery ([supervisor design](DESIGN-agent-supervisor.md),
[recovery storage](TEAM-RECOVERY-STORAGE.md), playbooks worker step 7).
Scenarios 7, 8 and 12 record that reconciliation as evidence in the request;
the hub stores it and does not verify it.

## After the rehearsal

The pilot is a separate, owner-approved step: a release, a deployment to an
isolated hub, and three manually started real clients following the playbooks
on small independent tasks, with the audit export and the probe's client
observations as its evidence. Nothing in this document authorizes it.
