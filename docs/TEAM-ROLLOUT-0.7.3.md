# v0.7.3 rollout and pilot checklist

v0.7.3 delivers portable team context (increment 1a-1c of the
[contract](DESIGN-portable-team-context.md)): `team_setup` and
`team_continue` deliver the role's guidance and the project's selected
process with a readiness report, a worker that is not ready is announced
unavailable and its reserved attempt is handled by state, an accepted
attempt keeps the process version it was accepted under, and the
`/join_team` and `/resume_team` entry points use all of it. The
[team quickstart](TEAM-QUICKSTART.md#7-upgrade-an-existing-team) has the
upgrade steps; this page is the release, rollout and pilot checklist.

Merged code and a published release are not evidence that any machine runs
them. Record each step's result, with versions and dates, on the rollout
and pilot tasks of the task board. Keep hostnames, accounts, paths, tokens
and other environment details there and out of this repository, its pull
requests and its release notes.

## What the tests cover, and what they do not

| Covered by fixture tests (a fake hub behind the real local MCP handler) | Not covered: outstanding, measured in the pilot |
| --- | --- |
| The readiness report and its four parts for both roles, joined and resumed | Whether each real client shows every delivered block whole, and the size at which it cuts a tool result |
| The delivered guidance and process, their terminators, and that the terminators match the readiness version and digest | Whether agents in each client follow the entry points: check the terminators, act as not ready on a cut delivery, never infer execution readiness |
| `team_context` and `process_context` re-reads returning the same terminators, pinned versions included | Whether each client honours the Claude Code `allowed-tools` pre-approval and stops at step 0 on an old `aimem mcp` |
| A worker that is not ready: unavailable heartbeat, offered attempt declined, running attempt blocked, no stop acknowledged | Windows, Linux and macOS hosts, with real Git access to the process repository and a real hub |
| Accept under process A, select B, restart: A delivered pinned, B named and not delivered; unrecorded or unrecoverable versions blocked | Heartbeat, inbox and offer timing under real latency and real restarts or compactions |
| The committed entry points equal the binary's rendering; every tool they name is listed by the local server | The Codex shell-runner failure, a client problem this release does not address |

## 1. Release (owner)

1. Merge the release-preparation pull request after its review gates pass
   at its final head.
2. Tag the merged master commit with an annotated tag `v0.7.3`. The release
   workflow refuses a tag that master does not contain. Wait for it to test,
   build every platform asset and publish the checksums.
3. Check that the release body is the `[0.7.3]` section of the changelog,
   not the fallback line.

There is no schema change and no new hub route, so no data migration.

## 2. Before the rollout

- [ ] Record the aimem version on the hub and on each agent machine.
- [ ] For each team, list the members and every reserved attempt with its
      state. Let running attempts finish or have the coordinator settle
      them. An attempt accepted before the upgrade has no recorded process
      version, and its worker is not ready after the upgrade (a running
      attempt is blocked).
- [ ] Confirm that each team's project has a process selected: in a member
      checkout, `aimem process show` reports it. Without one, no member is
      ready for work.

## 3. Hub (optional)

The member flow needs no hub upgrade: hubs on v0.7.1 or later remain
compatible. Upgrading a hub adds `team_context` to its MCP endpoint.

- [ ] Back up the state per the [administrator manual](ADMIN-MANUAL.md),
      upgrade, and check `aimem version` and `aimem health` as the service
      user.

## 4. Each agent machine

- [ ] Upgrade through the normal installer, or the site's verified binary
      swap. `aimem version` reports v0.7.3.
- [ ] In each member checkout, run `aimem teams commands .`, then
      `aimem teams commands . --check`: every asset is current, including
      the Codex prompts under `~/.codex` where that directory exists.
- [ ] Restart every agent client. A client started earlier keeps its old
      `aimem mcp` process.
- [ ] In each client, check that the aimem MCP server lists `team_setup`,
      `team_continue`, `team_context` and `process_context`.
- [ ] Resume each member (`/resume_team`, `$resume-team`) and record its
      readiness: `ready_for_work`, or the missing part and its fix.

**Rollback:** reinstall the previous binary, run `aimem teams commands .`
with it and restart the clients. The hub state is unaffected, because
there is no schema change. An older binary rewrites a checkout's team state
without the fields this release adds (the accepted attempt's recorded
version, the pending retry, the last delivered guidance digest). A later
re-upgrade therefore treats attempts accepted in between as unrecorded, so
roll back between attempts too.

## 5. Pilot scenarios (real clients)

Run on a disposable team and project wherever a scenario changes state.
Never revoke, expire or disable a live identity to provoke a failure.
Cover each client (Claude Code, Codex, OpenCode) on each host operating
system taking part. For every scenario, record the client and its version,
the operating system, the model and its source, the outcome, and evidence
(report excerpts, terminator lines, task and attempt ids).

| # | Scenario | Expected |
| --- | --- | --- |
| P1 | Coordinator joins with `/join_team TEAM coordinator` | `joined`; `ready_for_work` true; the coordinator guidance and the process both arrive, each ending with its terminator; the terminators match `readiness.role_context` and `readiness.project_process` |
| P2 | Worker joins | The same for the worker, and the roster shows it `available` |
| P3 | Delivery size | Record whether the client shows each block whole. If it cuts one, the agent re-reads it with `team_context` or `process_context` and acts as not ready until both are complete |
| P4 | Entry point run against the pre-upgrade `aimem mcp` (before the restart in section 4) | The agent names the missing tool, asks for an upgrade and a restart, and stops. No shell command runs |
| P5 | Project with tasks on and no process selected | The worker is not ready and announced `unavailable`; the coordinator is told to issue no offers |
| P6 | One small task: offer, accept, work, submit | The worker checks its own shell and build tools before accepting. The submit evidence names the guidance digest and the process commit it followed |
| P7 | Accept under process A, select B, restart the worker's client, resume | A is delivered marked PINNED and B is named as applying to new work; the worker stays ready and nothing is blocked |
| P8 | Required context missing while an attempt runs, induced without touching live credentials | The attempt is blocked with the reason and stays reserved. Nothing resumes it once the context returns: the worker waits for the coordinator's answer. The operator confirms that the local work actually stopped |
| P9 | Stop request during running work | The worker stops its local work and sends `team_stopped` only after it has stopped |
| P10 | Compaction or restart mid-attempt, then resume | Duties are restored from the report, reconciled against HEAD and `git status`, and the new generation is used after a resume |

A blocked report that still carries context blocks (the coordinator's inbox
read failing after delivery) is covered by fixture tests only. Do not
induce it live; record it if it occurs.

The pilot is complete when every participating client and operating system
has passed P1-P10, or has a recorded, accepted exception.
