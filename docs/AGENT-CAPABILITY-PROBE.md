# Structured client capability probe

Observed on Windows on 2026-09-22, after the
[supervisor design amendment](DESIGN-agent-supervisor.md). This is discovery
evidence for a future adapter, not a shipped supervisor or a live team pilot.

The real Codex and OpenCode clients executed MCP calls against a disposable
fixture. A local scripted Responses provider selected the calls and supplied
fixed text. No paid model was called. Consequently these results establish
client transport behavior, not model understanding, judgement, model identity,
task completion or reliable unattended coordination.

## Versions and measured results

| Component | Version observed |
| --- | --- |
| Codex CLI / app-server | 0.154.0 |
| Codex MCP initialize request | 2025-06-18 |
| OpenCode executable / HTTP server | 1.18.3 |
| OpenCode MCP initialize request | 2025-11-25 |
| OpenCode published `/doc` | OpenAPI 3.1.0 |

The MCP fixture echoes the requested protocol version. This records negotiation
for the tools/logging subset exercised here; it is not full conformance testing.
The runner checks the two exact client versions and refuses other versions.
Linux and Claude remain untested by this fixture.

**Observed** means the executable check passed. **Not observed** gives the
specific negative observation and its bounds. **Untested** means no capability
conclusion is justified; it must not be advertised as unsupported.

| Capability | Codex 0.154.0 | OpenCode 1.18.3 |
| --- | --- | --- |
| Spawn an owned structured backend | Observed: app-server over JSONL stdio; initialize, create, list and read thread | Observed: loopback HTTP server with disposable Basic auth; health, create and read session |
| Attach to an arbitrary already-running CLI | Untested; spawning app-server proves no such attachment | Untested; `serve` starts another server, not attachment to an existing CLI |
| Join-shaped JSON and typed question/answer | Observed: real MCP tool execution with fixture session ID and correlated answer | Observed: same fixture and sequence |
| Explicit inbox read, retry, cursor and ack | Observed: repeated cursor 0 returns the same question; answer and explicit ack precede an empty read at cursor 1 | Observed: same sequence |
| Bounded tool wait | Observed: inbox tool delays 100 ms and returns | Observed: same; long-running polling and timeout limits untested |
| Idle wakeup from MCP log notification | Not observed: zero new provider requests in 2 seconds after fixture emission | Not observed: zero new provider requests in the same window |
| Delivery while busy | Observed: `turn/steer` accepts input for the exact active turn ID; consumption by a model untested | Busy status observed; concurrent input/queue semantics untested |
| Input and permission request correlation | Untested at runtime; no pending input or permission request was answered | Untested at runtime; no pending input or permission request was answered |
| Interrupt | Observed: request succeeds and matching turn reports `interrupted` | Observed: busy status, abort returns true, pending message request settles; arbitrary child-process termination untested |
| Restart and reconnect | Observed: stop only the fixture-owned app-server, start a new one with its disposable state, resume the same thread ID | Observed: restart only the fixture-owned server, read the same session ID, then delete it |
| Structured event stream | Observed: correlated turn-completed events | Observed: SSE session-created event identifies the fixture session |
| Event replay / lossless reconnect | Untested; resume does not prove replay or gap detection | Untested; session read does not prove SSE replay |

Both clients passed the sequence `join -> inbox(0) -> inbox(0) -> reply -> ack ->
inbox(1)`. The fixture stores only synthetic data. Its answer is correlated with
`fixture-message`; a tool result or transport receipt never becomes an aimem
member acknowledgement. Real aimem access checks, generations, message transport
and assignments are outside this fixture.

The idle check emits an MCP `notifications/message` log entry. That notification
is not an inbox subscription or a vendor-specific wakeup primitive. A two-second
negative observation cannot establish that every notification mechanism is
unsupported. Use explicit inbox reads as the fallback and bounded polling once
the real messaging transport is implemented; qualify other wakeup mechanisms
separately before promising background delivery.

## Reproduce

Use Node with built-in `fetch`/`AbortSignal.timeout` and the exact Windows client
versions above. Supply absolute paths to the actual executables, especially the
OpenCode binary: a package-manager launcher can leave its server child alive when
only the launcher is stopped. The discovery runner does not install clients or
download dependencies.

```powershell
node scripts/agent-probe/run.cjs $CodexExe $OpenCodeExe
```

`$CodexExe` and `$OpenCodeExe` are operator-provided paths. Success exits zero and
prints `pass: true`, the client/protocol versions and the observations above.
Failure exits nonzero and records `error` or `cleanup_error`. Output includes a
new temporary evidence directory containing `result.json`, synthetic MCP state,
disposable client configuration and local diagnostics. Inspect locally; do not
publish complete client logs or machine-specific paths. Temporary evidence is
retained for diagnosis; the script never recursively deletes directories.

The runner creates an empty workspace and separate temporary home/config/data
directories, copies only basic OS/path environment variables, and supplies no
production credentials. Its provider listens on loopback and returns scripted
responses; the model label in the OpenCode configuration is just a client routing
fixture. It allows only the named disposable MCP tools in the generated client
configuration, uses no terminal automation and does not contact an aimem hub.
These configuration measures are not an OS sandbox or proof of zero incidental
client network activity. Run trusted installed executables on a trusted local
host. No production approval policy is changed.

Only child processes created by this run are stopped. A protocol abort is tested
against a held provider response, never a real shell command or live user session.
Normal success/failure cleanup waits for the owned backend processes to exit;
force-killing the runner or using an executable launcher is outside that cleanup
guarantee. A supervisor must still reconcile surviving commands before reassigning
real work.

Two fixture pitfalls were reproduced during discovery. Sending control commands
immediately after `turn/start` raced initialization: steering was accepted but
interrupt returned “no active turn.” Waiting for the held provider request made
steering and the correlated interrupted event observable. After HTTP server
restart, a stale connection could consume a long request timeout; short health
requests within a bounded retry window allowed reconnection. Neither result
justifies changing aimem production code.

## Consequences for implementation

Start the optional adapter with an explicitly spawned Codex app-server bound to a
disposable workspace: exact turn steering and interruption were measured there.
Keep OpenCode as the second interoperability target. This is a recommendation for
the later adapter task, not authorization to replace a user's existing session.

First finish core message transport, fenced assignments, lifecycle/results and
audit. The manual multi-client pilot remains independent of optional supervision.
That pilot must establish actual model receipt/answers and worker waiting behavior.
The later observation/adapter work must qualify pending input and permission
correlation, stale replies, busy queues, disconnect gaps and attachment before
claiming those capabilities. Permission delegation and host recovery remain their
own reviewed increments; this probe grants neither.

Upstream interface references: [Codex app-server](https://learn.chatgpt.com/docs/app-server),
[Codex MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli),
and [OpenCode server](https://opencode.ai/docs/server/). The capability conclusions
above come from the executable fixture, not extrapolation from documentation.
