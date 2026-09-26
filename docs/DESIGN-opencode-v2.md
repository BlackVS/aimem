# OpenCode 2 support for the aimem plugin

Status: **proposed 2026-09-26**; implementation submitted for review
together with this document. It collects the analysis, decisions and test
evidence in one place so the change can be reviewed without the working
session. Review verdicts live on the pull request, not here.

Scope: the OpenCode plugin (`.opencode/plugin/aimem.ts`), the docs that
describe it, and a new end-to-end check (`scripts/opencode-plugin-e2e/`).
No Go code, schema, installer or hub change.

## 1. Problem

aimem's OpenCode integration was built and tested on OpenCode 1.x (1.18.3,
1.18.23). OpenCode 2 shipped on 2026-09-11 and changes the plugin API
incompatibly. Both generations install the same `opencode` command and
cannot coexist, so one machine runs 1.x and another runs 2.x, and the
installers copy one plugin file to `~/.config/opencode/plugins/aimem.ts`
on both. That file must therefore work on both.

## 2. What OpenCode 2 changed, as it affects aimem

Sources: the OpenCode 2.0.18 source and docs (github.com/anomalyco/opencode,
tag `v2.0.18`: `services/www/src/docs/content/migrate-v1.mdx`,
`build/plugins/migrate-v1.mdx`, `instructions.mdx`, `mcp-servers.mdx`,
`packages/core/src/plugin/module.ts`,
`packages/client/src/promise/generated/types.ts`), confirmed by running
the real binaries (section 6).

Packaging: 2.x ships as `@opencode/cli` (npm), installed by
`curl -fsSL https://opencode.ai/v2/install | bash`; 1.x continues as
`opencode-ai` (latest 1.18.32).

| What aimem relies on | OpenCode 1.x | OpenCode 2.x |
|---|---|---|
| Plugin shape | exported function `(ctx) => hooks` | default export `{ id, setup(ctx) }`; a 1.x plugin is rejected at load with "Plugin must export a default definition with an id and an effect or setup function" |
| Plugin locations | `.opencode/plugin(s)/`, `~/.config/opencode/plugins/` | same directories |
| Event names | `message.updated`, `message.part.updated`, `session.idle`, `session.error`, `session.compacted` | none were emitted in testing (`session.idle` is still declared, marked deprecated); replaced by `session.text.*`, `session.tool.*`, `session.step.*`, `session.execution.{started,succeeded,failed,interrupted}`, `session.compaction.{started,ended,failed}` |
| Compaction hook | `experimental.session.compacting` | `ctx.session.hook("compaction")` |
| Bun shell `$` in context | yes | removed |
| Toast (`client.tui.showToast`) | yes | no plugin or client API |
| Request compaction (`client.session.summarize`) | yes | not in the plugin API; the server has `POST /api/session/:id/compact`, but a plugin has no server URL or credentials |
| opencode.json `instructions` | files injected into the system prompt | **accepted but ignored** (`instructions.mdx`: "V2 does not currently resolve its files") |
| `AGENTS.md` | loaded | loaded |
| `CLAUDE.md` fallback | yes | removed |
| `mcp.<name>` (1.x shape) | yes | accepted; native form is `mcp.servers.<name>` |
| Commands `.opencode/command(s)/`, skills `.opencode`/`.claude`/`.agents` | yes | same |
| Server HTTP API (used only by `scripts/agent-probe`) | 1.x routes | replaced by `/api/...`; the probe script is not ported here |

## 3. Impact before this change (measured)

Same scratch project, wired as `install.sh project` wires it, with a
scripted local model:

| Capability | 1.18.32 | 2.0.18, master plugin |
|---|---|---|
| Plugin loads | yes | **no** (rejected, WARN in server log only) |
| Turns / failures / compaction markers journaled | yes | **none** |
| `AIMEM HANDOFF` line in compaction summaries | yes | **no** |
| Context warnings, auto-compact | yes | **no** |
| `docs/SESSION-STATE.md` reaches the model | yes | **no** (`instructions` ignored) |
| `AGENTS.md` reaches the model | yes | yes |
| aimem MCP server connects | yes | yes (56 tools listed) |

The failure is silent: OpenCode 2 logs a warning and the session runs
normally without aimem.

## 4. Design

### 4.1 One file, both generations

```ts
const AimemPlugin: Plugin = async (ctx) => { /* unchanged 1.x implementation */ }
async function setupV2(ctx) { /* 2.x implementation */ }
export default { id: "aimem", setup: setupV2, server: AimemPlugin }
```

- 2.x validates only `default` against `{ id: string, setup: function }`
  and ignores other members and exports.
- 1.x from 1.14 detects a default object with `server` and calls only
  `server` (loader code read from the 1.14.24, 1.18.3 and 1.18.32
  binaries: `readV1Plugin(..., "detect")`, then `getServerPlugin`).
- The 1.x implementation is unchanged except that three helpers were
  lifted out so both halves share them: `makePoster` (the detached
  `aimem submit` spawn), `turnPayload` and `markerPayload` (identical
  journal events and idempotency keys on both generations).

Alternatives rejected:

- **Two files** (`aimem.ts` and `aimem-v2.ts`). Each generation fails to
  load the other's file. On 1.x before 1.14 that failure takes OpenCode
  down (4.3), and the installers would need to know which OpenCode is
  installed, which can change after install.
- **Default export as a function carrying `id`/`setup`/`server`.** This
  was tried to keep pre-1.14 loaders working; 2.x rejects it (its schema
  requires an object).

### 4.2 OpenCode 1.18.x also calls `setup`

Observed, not documented: 1.18.x (1.18.3 through 1.18.32) bundles an
experimental 2.x plugin host and calls `setup` in addition to `server`,
with a partial context (`options, agent, aisdk, catalog, command,
integration, plugin, reference, skill`; no `location`, `event` or
`session`). `setupV2` returns immediately unless `ctx.location.directory`,
`ctx.event.subscribe` and `ctx.session.hook` all exist, so a 1.x
session is journaled once. Verified: exactly one submit per turn on 1.18.x.

### 4.3 1.x floor is 1.14

Loaders before 1.14 (checked: 1.0.0, 1.1.4) call **every** module export
as a plugin function with no guard. Given the default object they throw
`TypeError: fn3 is not a function`, and OpenCode stops with "Unexpected
error" (observed on 1.1.4). No single export shape satisfies both that
loader and 2.x (4.1). The floor is therefore OpenCode 1.14 on the 1.x
line (1.14.24 was released 2026-04-24; aimem's recorded testing was on
1.18.x). This is stated in the CHANGELOG upgrade notes and the e2e
README.

### 4.4 2.x implementation: behavior mapping

| 1.x behavior | 2.x source in `setupV2` |
|---|---|
| user request of a turn | `session.inbox.enqueued` (user item text) → taken on `session.inbox.delivered`, dropped on `session.inbox.cancelled`; a steer or queued prompt delivered into a running execution joins that turn's request. Taking it at delivery, not at submission, keeps a prompt queued behind a running turn (and maybe cancelled) from replacing that turn's request |
| turn boundary | the turn is dropped once submitted; the next delivered prompt starts a clean turn with a fresh fallback id |
| assistant reply (last text part) | `session.text.ended` → `data.text`, `data.assistantMessageID` |
| tool names | `session.tool.input.started` → `data.name` |
| turn id | last `assistantMessageID` (from `text.ended` / `step.ended`) |
| turn ok | `session.execution.succeeded` |
| turn failed | `session.execution.failed`, `session.execution.interrupted` |
| compaction marker | `session.compaction.ended` |
| `AIMEM HANDOFF` note | `ctx.session.hook("compaction")`: appends a system part |
| handoff via `instructions` | `ctx.session.hook("context")`: see 4.5 |
| context warning (toast) | `context` hook: system note to the model, and the server log; see 4.6 |
| `AIMEM_AUTO_COMPACT` | not supported on 2.x; logged once; use OpenCode's `compaction` settings |
| detached submit via `$` + nohup | `node:child_process` spawn of `/bin/sh -c` (detached), paths as positional parameters; the Windows branch is unchanged |

Submits keep the 1.x payload and idempotency key
(`opencode:<session>:<turn>`), so the journal and service are unaware of
which generation reported a turn.

### 4.5 Handoff injection

2.x ignores `instructions`, so the `context` hook, which runs before each
primary model request, appends `docs/SESSION-STATE.md` as a system part.
To match what the 1.x wiring did:

- only for projects whose `opencode.json(c)` or
  `.opencode/opencode.json(c)` mentions `docs/SESSION-STATE.md`, which
  is what `install.sh` and `aimem doctor` wire;
- read fresh on every request (the file changes during a session);
  skipped when missing, empty or over 64 KiB;
- skipped when the system prompt already contains it, so nothing is
  duplicated once OpenCode starts honoring `instructions`.

### 4.6 Context usage and warnings

The limit comes from `ctx.model.list()` (`limit.context`) or
`AIMEM_CTX_LIMIT`. Usage is read in the hook from `ctx.session.context()`:
the newest assistant message's `tokens`, the same formula as 1.x. A
compaction entry newer than any measured step counts as zero. The
`session.step.ended` event is only a fallback: it can arrive after the
next request's hook has run, which made warnings lag one request in
testing.

The note is worded by 5% step ("over 60%"), not the live percentage, so
the system prompt changes at most once per step and provider prompt
caches survive. The same `AIMEM_CTX_WARN_FRACTION`, project `.aimem.json`
and `~/.config/aimem/env` knobs apply.

### 4.7 Multi-project servers

By default one 2.x background server hosts every session of a user.
Plugins are still instantiated per location: the plugin supervisor is a
location-scoped service (`packages/core/src/plugin/supervisor.ts`,
`makeLocationNode`), so `setup` runs once per project directory with that
directory in `ctx.location`. The event stream and hooks are not
documented as location-scoped, though, so every hook and event is
filtered by the session's own `location.directory` (`ctx.session.get`,
cached per session). Only definitive answers are cached, so a transient
lookup failure is retried. The model context limit is likewise cached
only when the catalog returned one.
When the plugin is installed both globally and in the project, 2.x loads
it once (same `id`); 1.x loads both, as on master, and the service drops
the duplicate by idempotency key.

## 5. Files changed

| File | Change |
|---|---|
| `.opencode/plugin/aimem.ts` | dual export; shared helpers; `setupV2` |
| `scripts/opencode-plugin-e2e/run.cjs`, `README.md` | new end-to-end check (6.1) |
| `CHANGELOG.md` | `[Unreleased]`: Added + Upgrade notes (reinstall, `AIMEM_AUTO_COMPACT` 1.x only, 1.14 floor) |
| `docs/ADMIN-MANUAL.md` | knob notes for 2.x (warning to the model; auto-compact 1.x only) |
| `docs/DESIGN.md` | plugin bullet covers both generations |

Installers are unchanged: same file, same destination.

## 6. Verification

### 6.1 End-to-end check

`node scripts/opencode-plugin-e2e/run.cjs <opencode-binary>` runs
`opencode run` in a disposable project and HOME against a scripted
OpenAI-compatible provider on 127.0.0.1 (no key, no paid model). A fake
`aimem` records every submit. The `queued` and `second` scenarios drive a
long-lived 2.x `opencode serve` over its HTTP API instead, so several
turns reach one plugin process. Every scenario also requires a clean
process exit: a run that hangs (killed at the timeout) or exits non-zero
fails even when its payloads are right. Scenarios:

| Scenario | Asserts |
|---|---|
| `text` | exactly one `turn` with the request and reply; submit names the right project; handoff reached the model |
| `tool` | `tool_summary` is `["glob"]` |
| `fail` | a `failure` event; all submits share one idempotency key |
| `warn` (2.x) | the warning note reached the model |
| `compact` (2.x) | compaction request carries `AIMEM HANDOFF`; one compaction marker |
| `queued` (2.x) | B queued behind a slow A and cancelled: one turn, with A's request and reply; B never journaled |
| `second` (2.x) | turn 1 succeeds, turn 2 fails: one turn and one failure, each with its own request and idempotency key |

Results on the final code (2026-09-26, Linux x64):

| OpenCode | text | tool | fail | warn | compact | queued | second |
|---|---|---|---|---|---|---|---|
| 1.14.24 | ok | ok | ok (exit 0) | n/a | n/a | n/a | n/a |
| 1.18.32 | ok | ok | ok (exit 1) | n/a | n/a | n/a | n/a |
| 2.0.18 | ok | ok | ok (exit 1) | ok | ok | ok | ok |

1.18.3 passed `text`, `tool` and `fail`, and 1.18.23 and 1.18.28 the same
three, on earlier revisions; the later changes touched `setupV2` only,
which 1.x never reaches past its guard. `queued` fails against the
plugin as first reviewed (commit `87f358c`: the running turn is
journaled with the cancelled prompt's request), and passes with the fix.
With `AIMEM_E2E_TIMEOUT_MS=2000`, `text` fails with "opencode run timed
out", which shows the process-health check works.

Binaries were obtained with `npm pack opencode-linux-x64@<version>`
(1.x) and `npm pack @opencode/cli-linux-x64@2.0.18` (2.x).

### 6.2 Other checks

- Parity with master on the 1.x failure path: master and this plugin both
  submit `failure` + `turn` under one idempotency key on 1.18.32.
- Global install layout (`~/.config/opencode/plugins/aimem.ts` only, and
  global + project): one submit per turn on 2.0.18; on 1.18.32, one
  submit with the global copy only, and two with the same key when both
  copies are present (unchanged from master).
- Pre-1.14 failure mode reproduced on 1.1.4 (4.3).
- Pre-push reviews (the built-in code review at medium) found, and this
  change fixes: the pre-1.14 loader problem (resolved by the documented
  floor); a later-turn failure hidden by a reused turn id; a transient
  ownership lookup cached forever; the warning text defeating prompt
  caching; the compaction note not scoped to the project; a failed model
  catalog lookup cached as "no limit" (including a synchronous throw);
  a reply-less turn getting a new id on each end event; 1.x
  pre-releases misdetected as 2.x by the e2e script. One further finding
  (a single plugin instance serving every project) was checked against
  the 2.x source and does not apply (4.7).
- The external review of `87f358c` raised two follow-ups, both fixed: a
  prompt queued behind a running turn replaced that turn's request
  (reproduced, then fixed by taking the request at inbox delivery; the
  `queued` scenario covers it), and the e2e script passed runs that hung
  or exited non-zero after correct payloads (it now checks process
  health).
- No Go change: `gofmt -l .` prints nothing.

## 7. Known gaps and follow-ups

- **Not tested:** Windows (the win32 spawn branch is unchanged from
  master); a 2.x server hosting several aimem projects at once (4.7:
  per-location loading was read from source, the filtering is
  code-reviewed only); a steer delivered into a running turn (it joins
  that turn's request by design; the unmodified queued case was observed
  to do the same). Such a joined turn keeps only its last reply, as 1.x
  keeps only a turn's last text part.
- **MCP on 2.x:** the aimem MCP server connects and lists its tools, but
  in `opencode run --standalone` no MCP tool reached the model, for aimem
  and for a one-tool control server alike. This is 2.x behavior in that
  mode, not aimem-specific; it still needs checking in the interactive
  2.x client.
- **`scripts/agent-probe`** uses the 1.x server API and will not work
  against 2.x. It is a discovery probe, not a shipped path; not ported
  here.
- **Optional later:** have `aimem doctor` write the native 2.x MCP shape
  (`mcp.servers.aimem`) and warn about OpenCode older than 1.14. Today
  version probing runs only under `aimem teams setup --client-versions`.
- **A failure before any model reply** now gets a `no-assistant-<ts>`
  turn id on 2.x, fixed when the prompt arrives. 1.x keeps the previous turn's id there, which the
  service then drops as a duplicate; that 1.x behavior is unchanged here.
