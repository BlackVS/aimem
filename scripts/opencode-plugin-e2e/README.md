# OpenCode plugin end-to-end check

`run.cjs` runs the OpenCode plugin (`.opencode/plugin/aimem.ts`) inside a
real OpenCode executable and checks what it journals. One file has to work
on both OpenCode generations, so run it against a 1.x and a 2.x binary
after changing the plugin. The supported 1.x floor is 1.18.0 (loaders
before 1.14 call every export as a function and cannot load a file that
also serves 2.x).
Design and results: [docs/DESIGN-opencode-v2.md](../../docs/DESIGN-opencode-v2.md).

```sh
CGO_ENABLED=0 go build -o /tmp/aimem ./cmd/aimem                     # for recall/norecall
export AIMEM_E2E_AIMEM=/tmp/aimem
node scripts/opencode-plugin-e2e/run.cjs /path/to/opencode            # all scenarios
node scripts/opencode-plugin-e2e/run.cjs /path/to/opencode text fail  # a subset
```

Each scenario uses a disposable project and HOME and a scripted
OpenAI-compatible provider on 127.0.0.1. It needs no API key and never
calls a paid model. A fake `aimem`, first on PATH (in the launch
directory for `text`, which the plugin prefers), records every
`aimem submit` payload. The checks cover the journal events and what
reached the model; the real `aimem` binary and MCP are not involved,
except in `recall` and `norecall`. There the fake `aimem` hands
`aimem failure-recall` to the real binary named by `AIMEM_E2E_AIMEM`, which
talks to a fake store on a Unix socket. Without `AIMEM_E2E_AIMEM` those
two scenarios are skipped, with a line that says so.
Every scenario also requires a clean exit: a run that hangs (killed after
150 s, or `AIMEM_E2E_TIMEOUT_MS`) or exits non-zero fails even when its
payloads are right.

| Scenario | Checks |
|---|---|
| `text` | one `turn` event with the user request and the reply; `docs/SESSION-STATE.md` reached the model |
| `tool` | the turn's `tool_summary` names the tool that ran |
| `fail` | a `failure` event; every submit shares one idempotency key |
| `subdir` | OpenCode started in a subdirectory still gets the handoff wired in the project root's opencode.json |
| `warn` | 2.x only: the context warning reached the model |
| `compact` | 2.x only: the compaction request carries the `AIMEM HANDOFF` note, and one compaction marker is journaled |
| `queued` | 2.x only, via `opencode serve`: a prompt queued behind a slow turn and cancelled leaves that turn's request intact and is never journaled |
| `second` | 2.x only, via `opencode serve`: turn 1 succeeds and turn 2 fails; the failure is journaled under its own turn |
| `big` | 2.x only: a 20 KB handoff reaches the model as its first 8 KiB and the limit note, never its tail |
| `recall` | a shell call (`shell` in 2.x, `bash` in 1.x) exits 128 with an SSH error; the matching stored memory reaches the model as the failure recall |
| `norecall` | the same failure with only an unrelated memory stored; no failure recall reaches the model |

Get a binary without installing it over your own OpenCode:

```sh
npm pack opencode-linux-x64@1.18.32 && tar xzf opencode-linux-x64-1.18.32.tgz   # 1.x: package/bin/opencode
npm pack @opencode/cli-linux-x64@2.0.18 && tar xzf opencode-cli-linux-x64-2.0.18.tgz  # 2.x: package/bin/opencode
```

Linux and macOS only: the fake `aimem` is a shell script. OpenCode fetches
its model catalog from models.dev at startup; where that is blocked it logs
an error and the run still passes. Set `AIMEM_E2E_VERBOSE=1` to print
OpenCode's stderr for a failing scenario.
