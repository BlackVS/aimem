# Claude channel wake-up probe

Observed on Windows on 2026-09-24, as discovery evidence for the
[optional supervisor](DESIGN-agent-supervisor.md). It extends the
[structured client probe](AGENT-CAPABILITY-PROBE.md), which found that an MCP
log notification does not wake an idle Claude, Codex or OpenCode session.
This probe tests the mechanism Claude Code documents for exactly that purpose:
[channels](https://code.claude.com/docs/en/channels), MCP servers that push
events into a running session ([reference](https://code.claude.com/docs/en/channels-reference)).
Nothing here is a shipped integration, and no aimem hub, credential or live
team session was involved.

The required outcome is a chain: a message wakes an idle agent, the agent
replies, and the reply wakes the waiting coordinator, with no human prompting
and no model calls spent polling an empty inbox. This increment covers the
Claude end of that chain only.

## Setup

| Component | Observed |
| --- | --- |
| Claude Code | 2.1.281, interactive terminal session |
| Model | Haiku 4.5 through the operator's own Claude subscription login |
| MCP protocol negotiated | 2025-11-25 (the client reports the connection as the "legacy" era) |
| Channel server | `scripts/agent-probe/channel.cjs`, a disposable stdio server with tools `probe_ack`, `probe_reply`, `probe_wait` |
| Node runtime | 24.11.1 |

The operator started the session from an empty workspace outside any
repository:

```powershell
claude --model haiku --permission-mode default --setting-sources project,local `
  --strict-mcp-config --mcp-config RUN\mcp.json `
  --dangerously-load-development-channels server:fixture `
  --debug-file RUN\debug.log -n aimem-channel-probe
```

`RUN\mcp.json` names only the fixture server: an absolute Node path running
`channel.cjs` with `RUN\state.json` as its argument. `--setting-sources
project,local` keeps user-level hooks and plugins out of the disposable session.
The login itself is not a setting and still applied. Permission prompts stayed
on. The operator approved each fixture tool the first time it was used and
chose "don't ask again", which stored allow rules in the disposable
workspace's local settings.

`scripts/agent-probe/channel-drive.cjs` pushes synthetic events (`emit RUN ID
TEXT`), stops the server to simulate a crash (`exit RUN`), and prints a merged
timeline (`report RUN`). The timeline merges the fixture's own state with the
channel, turn and API-request lines from the session's debug file. The model
received five inputs: four channel events and one typed prompt.

## Results

**Observed** means seen in this run's debug log and fixture state.
**Documented** means stated by the vendor documentation and not measured here.
**Untested** means no conclusion is justified.

| Question | Result |
| --- | --- |
| Channel availability | Observed: the debug log records `Channel notifications registered` for the fixture 7-130 ms after each connection is established. A built-in MCP server without the capability is logged as skipped. |
| Startup consent | Observed by the operator: a full-screen "WARNING: Loading development channels" dialog, answered with "I am using this for local development". There was also a workspace-trust prompt on first use. Whether the dialog returns on every start was not recorded reliably. |
| Idle cost | Observed: no model request during 58 s of idle before the first event. Later idle gaps contained only client-initiated `away_summary` requests (the "recap" feature, which the client says can be turned off in `/config`), each about 3 minutes after a turn ended. There was no inbox polling. |
| Idle wake-up | Observed: event received 1 ms after emission, turn started at +70 ms, first model request at +114 ms. The same held after a server reconnect (+30/+62 ms) and after a client restart (+99/+155 ms). |
| Agent reply | Observed once instructed: when the event text named the tools, the model called `probe_ack` and then `probe_reply` within about 2.5 s. For the first event, with the instruction only in the server's `instructions` field, the model answered in plain text and called neither tool. |
| Receipt, ack and answer kept separate | Observed as three different records: transport receipt (debug line), the model's explicit `probe_ack`, and its `probe_reply`. None implies the others. The documentation states that Claude Code never acknowledges notifications to the server. |
| Busy session | Observed: three events sent while a 25 s tool call ran were each received within about 1 ms and did not interrupt the call. They reached the model at the next model request (78 ms after the tool returned), inside the same turn, and were handled together: one `probe_ack` with both ids, then one reply each. The documentation says events that arrive while busy are delivered together "on the next turn". Here that was the next model request, not a new turn. |
| Duplicates | Observed: the same `message_id` sent twice was delivered twice (two receipt lines); Claude Code does not deduplicate. The model acked and answered it once, but that is model behaviour, not a guarantee. Deduplication has to happen by id on the aimem side. |
| Server crash | Observed: after the channel server process exited, Claude Code removed its tools and made no attempt to restart it in the 3.5 minutes before the operator intervened. Events cannot reach a dead server. The operator's `/mcp` reconnect started a new process and the channel re-registered (`Channel notifications registered` again). |
| Client restart | Observed: `--continue` resumed the same session id and the channel re-registered. An event the new server process sent 109 ms before registration was silently dropped: no receipt, no turn, no error to the server. The documentation states that events are dropped silently when the session has not loaded the server as a channel. |
| Permission relay | Not enabled and not tested: the fixture does not declare `claude/channel/permission`. Permission prompts stayed in the terminal. |

### Where channels do not work

These negative results come from disposable non-interactive sessions (`claude
-p --input-format stream-json --output-format stream-json`) with an isolated
configuration directory:

- With `--dangerously-load-development-channels server:fixture`, the channel
  never registered and an event sent to the idle session caused no model
  request within 12 s. This held with and without `--bare`, and both with a
  loopback scripted provider and with the first-party endpoint using a dummy
  key (every model call failed with 401, so nothing was billed).
- `--channels server:fixture` and `--channels plugin:fakechat@claude-plugins-official`
  (not installed) produced no channel gate log line at all.
- With the loopback provider, the debug log says remote feature flags are off
  for "a third-party provider". The CLI binary contains the reason string
  "channels are not available on third-party providers". So the earlier probe's
  scripted, unbilled provider cannot exercise channels, and the positive
  results above needed real model turns.

The documentation says `-p` works with channels selected through `--channels`
from an approved allowlist. That path was not tested (it needs an allowlisted
plugin and the Bun runtime).

## What this means for aimem

A Claude worker can be woken without polling. There are five constraints.

1. **The channel is a wake-up hint; the inbox stays authoritative.** Delivery
   is silent on loss (before registration, or while the server is down) and
   neither acknowledged nor deduplicated. Each notification should therefore
   say only that unacknowledged messages exist, carrying stable
   `message_id`/`sequence` meta. The agent then reads with `team_inbox` and
   acknowledges with `team_ack`, exactly as it does now. A lost or duplicated
   hint costs one extra read, never a lost message.
2. **Re-announce while unacknowledged.** The server cannot tell when Claude
   has registered the channel. It should announce outstanding messages after
   connecting and again with a bounded backoff while they remain
   unacknowledged. An empty inbox produces no notification and so no model call.
3. **Put the instruction in the event text.** Relying on the server's
   `instructions` field alone did not make the model act. The notification
   content should state the required action (read the inbox, acknowledge, act
   within the accepted attempt only).
4. **Never crash the server.** A dead stdio server is not restarted, and the
   channel shares its process with the team tools. Hub errors must back off
   inside the process, never exit it.
5. **Opt-in and consent stay with the operator.** Custom channels need
   `--dangerously-load-development-channels` during the research preview, with
   a startup warning. On claude.ai Team and Enterprise plans an administrator
   must also set `channelsEnabled`. Channels require first-party Anthropic
   authentication and do not work in print mode through the development flag.

## Recommended smallest implementation

Add an opt-in channel mode to the existing checkout-bound `aimem mcp` server,
the process `/join_team` already uses, instead of a new supervisor:

- **Opt-in:** a flag or setting makes `aimem mcp` declare
  `experimental['claude/channel']`. The operator adds
  `--dangerously-load-development-channels server:aimem` to the Claude launch
  command. Nothing changes for sessions that do not opt in, or for Codex and
  OpenCode, which ignore the capability.
- **Hub wait:** while this checkout holds a joined team session, one goroutine
  waits on the hub inbox with the existing bounded server-side wait. This uses
  the member's own session and credential in its own process, with no
  borrowed token and no model call. On new unacknowledged messages it emits a
  short notification with the message ids, kind and task id, re-announcing
  with backoff until they are acknowledged.
- **Scope:** no permission relay, no automatic acceptance, no new hub route.
  Receipt, acknowledgement and answer stay the existing separate operations.

Qualification for that change: repeat this probe against the real `aimem mcp`
with a hub fixture, covering loss before registration, duplicate hints,
hub-down backoff and a stop request that arrives while busy.

## Connecting a Codex coordinator

Codex has no observed push path into an interactive session. The first probe
saw no wake-up from MCP notifications, and nothing here changes that. The
smallest bridge that fits the measured interfaces is a host process that owns a
spawned `codex app-server` for the coordinator and waits on the hub inbox for
the coordinator's session. On a message it calls `turn/start` when the thread
is idle or `turn/steer` on the active turn. Both were measured on Codex 0.154.0
in the first probe. The hub wait costs no model calls; each message costs one
turn.

This replaces the interactive Codex terminal for the coordinator. The bridge
is the single owner of that thread, so the operator watches or steers through
the bridge rather than a separate TUI. Per the supervisor design, the bridge
needs its own reviewed delegation before it acts for a member; until then it
may only start turns in a session the operator launched through it.

## Untested

- The Codex bridge and the full chain end to end (worker reply waking a real
  coordinator), with installed Codex 0.156.1 rather than the 0.154.0 measured
  before.
- Real aimem messages, generations and acknowledgement through a hub.
- Linux and macOS; Claude Code versions other than 2.1.281; claude.ai Team or
  Enterprise organization policy; Console API-key authentication.
- `--channels` with an allowlisted plugin, including in print mode.
- Idle periods longer than about 5 minutes, sleep/resume, network loss, high
  event rates and backpressure.
- An event arriving while a permission prompt is pending.
- What exactly the model receives for batched or duplicate events: request
  bodies were not logged.
- Whether turning off recaps removes the idle `away_summary` requests, and
  whether the development-channel warning returns on every start.
- Automatic restart of a crashed stdio server over windows longer than 3.5 minutes.

## Reproduce

Use the disposable run directory `RUN`, outside any repository, with an empty
`workspace` subdirectory and the `mcp.json` described above. Start Claude with
the command above and accept its prompts. From another terminal:

```powershell
node scripts/agent-probe/channel-drive.cjs emit RUN m1 "Synthetic fixture message m1. Call probe_ack with the message_ids of every fixture message not yet acked, then probe_reply once per id with answer ok."
node scripts/agent-probe/channel-drive.cjs report RUN
```

For the busy case, type `Call probe_wait with seconds 25, then say waited.` in
the session and emit events while it waits. `exit RUN` stops the server. After
a client restart with `--continue`, an event file left in `RUN` is emitted as
soon as the new server starts, which reproduces the loss before registration.
Each event starts real model turns on the operator's account; keep the run
small. Inspect `RUN` locally; do not publish debug logs or machine paths.
