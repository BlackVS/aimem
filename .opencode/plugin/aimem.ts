// OpenCode adapter for aimem: observes the event bus and submits one
// normalized checkpoint per completed turn plus failure markers and
// compaction markers. Persistence goes through `aimem submit`, which
// redacts adapter-side and spools when the service is down.
//
// ONE file serves both OpenCode generations, because a machine may run
// either (the same `opencode` command name, installed exclusively):
//   - OpenCode 1.x (1.14 or newer) calls the V1 plugin function, `server`
//     on the default export, and observes session.idle / session.error /
//     session.compacted. Loaders before 1.14 call every export as a
//     function and fail on the default object, which 2.x requires; this
//     file therefore needs OpenCode 1.14+ on the 1.x line.
//   - OpenCode 2.x rejects V1 plugins outright and calls `setup(ctx)` on
//     the default export instead (v2 API: ctx.event.subscribe, session
//     hooks). V2 also ignores opencode.json `instructions`, so the
//     handoff file is injected by the context hook — see setupV2.
// OpenCode 1.18.x also calls `setup` from an experimental V2 host with a
// partial context (no location/event/session); setupV2 does nothing
// there, so a 1.x session is never journaled twice.
//
// Submits are a single DETACHED spawn (payload written synchronously,
// binary backgrounded): `opencode run` exits immediately after the turn
// ends, and any awaited subprocess would be killed mid-flight with the
// checkpoint silently lost. Detaching lets the submit outlive the host
// process. Residual risk: teardown can still preempt the spawn itself.
import { spawn } from "node:child_process"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import type { Plugin } from "@opencode-ai/plugin"

type TurnState = {
  userMsgID: string
  user: string
  reply: string
  tools: string[]
  lastAssistantID: string
}

// Relative context warnings: first at AIMEM_CTX_WARN_FRACTION (default
// 0.8), then ESCALATING every 5% — one missed toast must not be the
// only chance before the hard context error (observed live: a session
// sailed past its single warning and stuck). The limit comes from the
// model's own context limit (fetched from OpenCode's provider config,
// cached); AIMEM_CTX_LIMIT overrides. Models with no known limit (e.g.
// custom providers report 0) get NO warning — a guessed denominator is
// worse than silence — so declare limits in opencode.jsonc (ADMIN-
// MANUAL 3b) or set AIMEM_CTX_LIMIT for these features to see anything.
//
// AIMEM_AUTO_COMPACT=<fraction> (e.g. 0.9; unset/0 = off) additionally
// TRIGGERS compaction at that fraction, so long tasks survive without a
// human catching the moment: the journal already holds every turn, the
// compacting hook injects the handoff instruction, and the marker is
// written — the "prepare" half is automatic by construction.
// Knob resolution, most-specific-intent first:
//   process env  >  project .aimem.json  >  ~/.config/aimem/env  >  default
// The env file is folded with the CLI's exact semantics (AIMEM_* lines,
// quotes stripped) so the machine's ONE aimem config tunes the plugin;
// the PROJECT override exists because these are model-behavior knobs in
// disguise — a project on a model that degrades mid-context ("lost in
// the middle") wants auto_compact at 0.2-0.4 while the host default
// stays lax. An explicit env var at launch still wins, so a one-off
// `AIMEM_AUTO_COMPACT=0 opencode` run can disable anything. Values >1
// are read as percent (30 == 0.3). Read at plugin load.
function envFileVal(name: string): string | undefined {
  try {
    const p = path.join(os.homedir(), ".config", "aimem", "env")
    for (const line of fs.readFileSync(p, "utf8").split("\n")) {
      const t = line.trim()
      if (!t || t.startsWith("#")) continue
      const i = t.indexOf("=")
      if (i <= 0 || t.slice(0, i) !== name) continue
      return t.slice(i + 1).replace(/^["']|["']$/g, "")
    }
  } catch {
    // No env file is the common workstation case; env-only still works.
  }
  return undefined
}

function knob(dir: string, envName: string, projKey: string, dflt: number, fraction: boolean): number {
  // Fractions accept percent style too (30 == 0.3); token counts
  // (ctx_limit) pass through untouched.
  const norm = (v: number) => (fraction && v > 1 ? v / 100 : v)
  const pe = process.env[envName]
  if (pe !== undefined && pe !== "" && Number.isFinite(Number(pe))) return norm(Number(pe))
  try {
    const raw = fs.readFileSync(path.join(dir, ".aimem.json"), "utf8").replace(/^﻿/, "")
    const v = (JSON.parse(raw) as Record<string, unknown>)[projKey]
    if (typeof v === "number" && Number.isFinite(v)) return norm(v)
  } catch {}
  const fv = envFileVal(envName)
  if (fv !== undefined && Number.isFinite(Number(fv))) return norm(Number(fv))
  return dflt
}

const HANDOFF_NOTE =
  "\n\nIMPORTANT (aimem): end the summary with exactly this line so it " +
  "survives into the compacted context:\n" +
  "AIMEM HANDOFF: re-read docs/SESSION-STATE.md (canonical handoff) before " +
  "continuing; verify volatile state against git/tests; recent completed " +
  "turns are recoverable from the aimem journal."

// makePoster returns postDetached for one project: it hands one payload
// to `aimem submit` in a way that survives host-process teardown (see
// header comment). Binary resolution: project-local build first
// (development), else PATH (user-level install via install.sh). `$` is
// the V1 Bun shell helper; V2 has none, so without it the spawn goes
// through node:child_process with the same detach semantics.
function makePoster(directory: string, $?: any) {
  const localBin = `${directory}/aimem${process.platform === "win32" ? ".exe" : ""}`
  const bin = fs.existsSync(localBin) ? localBin : "aimem"
  return async (payload: unknown) => {
    try {
      if (bin.includes("/") && !fs.existsSync(bin)) return false
      const tmp = path.join(os.tmpdir(), `aimem-oc-${Date.now()}-${Math.random().toString(36).slice(2)}.json`)
      fs.writeFileSync(tmp, JSON.stringify(payload), { mode: 0o600 })
      if (process.platform === "win32") {
        // No nohup on Windows: detached spawn with stdin redirected from the
        // already-written temp file (payload safe even if the host dies).
        // The temp file is left behind — tiny, and the OS temp dir rotates.
        const fd = fs.openSync(tmp, "r")
        // An 'error' listener is required: a failed spawn (ENOENT when aimem
        // is not on PATH) is emitted asynchronously, past this try/catch,
        // and an unhandled 'error' event would take the OpenCode host down.
        spawn(bin, ["submit"], { stdio: [fd, "ignore", "ignore"], detached: true, windowsHide: true })
          .on("error", (e) => console.error("aimem submit failed:", e))
          .unref()
        fs.closeSync(fd)
        return true
      }
      if (typeof $ === "function") {
        const cmd = `nohup "${bin}" submit < "${tmp}" >/dev/null 2>&1 && rm -f "${tmp}" &`
        await $`bash -c ${cmd}`.quiet()
        return true
      }
      // Paths travel as positional parameters, never spliced into the
      // script, so no quoting of bin or tmp is needed.
      spawn("/bin/sh", ["-c", '"$0" submit < "$1" >/dev/null 2>&1 && rm -f "$1"', bin, tmp], {
        stdio: "ignore",
        detached: true,
      })
        .on("error", (e) => console.error("aimem submit failed:", e))
        .unref()
      return true
    } catch (e) {
      // Fail-open: checkpointing must never break the session.
      console.error("aimem submit failed:", e)
      return false
    }
  }
}

// turnPayload and markerPayload are the two journal events both plugin
// generations submit; the idempotency key shape is shared so a turn keeps
// one journal row whichever path reported it.
function turnPayload(
  directory: string,
  sid: string,
  turnID: string,
  outcome: "ok" | "failed",
  t: { user: string; reply: string; tools: string[] },
) {
  return {
    project_dir: directory,
    event: {
      schema_version: 1,
      idempotency_key: `opencode:${sid}:${turnID}`,
      client: "opencode",
      session_id: sid,
      turn_id: turnID,
      kind: outcome === "ok" ? "turn" : "failure",
      outcome,
      ts: new Date().toISOString(),
      user_request: t.user,
      assistant_response: t.reply,
      tool_summary: t.tools.slice(0, 50),
    },
  }
}

function markerPayload(directory: string, sid: string, anchor: string) {
  return {
    project_dir: directory,
    event: {
      schema_version: 1,
      idempotency_key: `opencode:${sid}:${anchor}-compacted`,
      client: "opencode",
      session_id: sid,
      turn_id: `${anchor}-compacted`,
      kind: "compaction-marker",
      outcome: "pre-compaction",
      ts: new Date().toISOString(),
      user_request: "session compacted",
    },
  }
}

const AimemPlugin: Plugin = async ({ directory, client, $ }) => {
  const CTX_LIMIT = knob(directory, "AIMEM_CTX_LIMIT", "ctx_limit", 0, false)
  const CTX_WARN_FRACTION = knob(directory, "AIMEM_CTX_WARN_FRACTION", "ctx_warn_fraction", 0.8, true)
  const AUTO_COMPACT = knob(directory, "AIMEM_AUTO_COMPACT", "auto_compact", 0, true)
  const roles = new Map<string, string>() // messageID -> role
  const turns = new Map<string, TurnState>() // sessionID -> current turn
  const submitted = new Map<string, string>() // sessionID -> last submitted turn id
  const ctxWarnedStep = new Map<string, number>() // sessionID -> last warned 5%-step
  const autoCompacted = new Set<string>() // sessions where auto-compact fired (cleared on compaction)
  const postDetached = makePoster(directory, $)

  const turn = (sid: string): TurnState => {
    let t = turns.get(sid)
    if (!t) {
      t = { userMsgID: "", user: "", reply: "", tools: [], lastAssistantID: "" }
      turns.set(sid, t)
    }
    return t
  }

  const submit = async (sid: string, outcome: "ok" | "failed") => {
    const t = turns.get(sid)
    if (!t) return
    const turnID = t.lastAssistantID || `no-assistant-${Date.now()}`
    if (submitted.get(sid) === turnID && outcome === "ok") return // idle re-fires
    // Deliberate: a turn that errors and then idles submits both outcomes
    // under ONE idempotency key — the journal keeps one event per turn,
    // whichever landed first; the service drops the other.
    const ok = await postDetached(turnPayload(directory, sid, turnID, outcome, t))
    if (ok && outcome === "ok") submitted.set(sid, turnID)
  }

  const submitCompactionMarker = async (sid: string) => {
    const anchor = turns.get(sid)?.lastAssistantID || `t${Date.now()}`
    await postDetached(markerPayload(directory, sid, anchor))
  }

  // providerID/modelID -> context limit, from OpenCode's provider config.
  let modelLimits: Map<string, number> | null = null
  const contextLimit = async (info: any): Promise<number> => {
    if (CTX_LIMIT > 0) return CTX_LIMIT
    if (!modelLimits) {
      modelLimits = new Map()
      try {
        const res: any = await (client as any)?.config?.providers?.()
        const provs = res?.data?.providers ?? res?.providers ?? []
        for (const pr of provs) {
          for (const [mid, m] of Object.entries<any>(pr?.models ?? {})) {
            const lim = Number(m?.limit?.context ?? 0)
            if (lim > 0) modelLimits.set(`${pr.id}/${mid}`, lim)
          }
        }
      } catch {
        // Fall through to the fallback limit below.
      }
    }
    return modelLimits.get(`${info?.providerID}/${info?.modelID}`) ?? 0
  }

  const maybeWarnContext = async (sid: string, info: any) => {
    const tokens = info?.tokens
    if (!tokens) return
    const used =
      (tokens.input ?? 0) +
      (tokens.output ?? 0) +
      (tokens.reasoning ?? 0) +
      (tokens.cache?.read ?? 0)
    const limit = await contextLimit(info)
    if (limit <= 0) return
    const frac = used / limit
    const pct = Math.round(100 * frac)

    // Opt-in auto-compaction: past the fraction, trigger the summarize
    // OpenCode would eventually need anyway - while there is still room
    // for the summarizer to run. Once per window; the session.compacted
    // event re-arms it for the next fill.
    if (AUTO_COMPACT > 0 && frac >= AUTO_COMPACT && !autoCompacted.has(sid)) {
      autoCompacted.add(sid)
      const msg = `aimem: context ~${pct}% - auto-compacting (AIMEM_AUTO_COMPACT=${AUTO_COMPACT}); the journal holds every turn`
      console.error(msg)
      try {
        await (client as any)?.tui?.showToast({ body: { message: msg, variant: "warning" } })
      } catch {}
      try {
        const s: any = (client as any)?.session
        // SDK surface has varied; try the known shapes, fail open.
        if (s?.summarize) await s.summarize({ path: { id: sid } })
        else if (s?.compact) await s.compact({ path: { id: sid } })
        else console.error("aimem: auto-compact unavailable in this OpenCode build - compact manually")
      } catch (e) {
        console.error("aimem auto-compact failed:", e)
      }
      return
    }

    // Escalating warnings: first past the fraction, again every 5% -
    // one missed toast must not be the last word before the hard error.
    if (frac < CTX_WARN_FRACTION) return
    const step = Math.floor(frac * 20)
    if (step <= (ctxWarnedStep.get(sid) ?? -1)) return
    ctxWarnedStep.set(sid, step)
    const msg =
      `aimem: context ~${pct}% of ` +
      `${limit} tokens (${info?.modelID ?? "model"}) - finish the smallest ` +
      `safe unit and update docs/SESSION-STATE.md before compaction`
    console.error(msg)
    try {
      await (client as any)?.tui?.showToast({
        body: { message: msg, variant: "warning" },
      })
    } catch {
      // Toast API is best-effort; the log line above always happens.
    }
  }

  return {
    // Deterministic pointer injection at compaction time (Phase 4). Only
    // ever APPEND to an existing prompt or context list — replacing
    // output.prompt wholesale would clobber the default compaction prompt.
    "experimental.session.compacting": async (_input: any, output: any) => {
      try {
        if (!output || typeof output !== "object") return
        if (typeof output.prompt === "string" && output.prompt.length > 0) {
          output.prompt += HANDOFF_NOTE
        } else if (Array.isArray(output.context)) {
          output.context.push(HANDOFF_NOTE)
        }
      } catch (e) {
        console.error("aimem compacting hook:", e)
      }
    },

    event: async (input: any) => {
      const event: any = input?.event ?? input
      const p: any = event?.properties
      switch (event?.type) {
        case "message.updated": {
          const info = p?.info
          if (info?.id && info?.role) {
            roles.set(info.id, info.role)
            if (info.role === "assistant" && info.sessionID) {
              turn(info.sessionID).lastAssistantID = info.id
              await maybeWarnContext(info.sessionID, info)
            }
            // Turn reset happens on a NEW user text part (see below), not
            // here: message.updated re-fires for the same user message at
            // end of turn and would wipe the state just before idle.
          }
          break
        }
        case "message.part.updated": {
          const part = p?.part
          if (!part?.sessionID || !part?.messageID) break
          const t = turn(part.sessionID)
          const role = roles.get(part.messageID)
          if (part.type === "text" && typeof part.text === "string") {
            if (role === "user") {
              if (t.userMsgID !== part.messageID) {
                // Genuinely new user message: start a fresh turn.
                turns.set(part.sessionID, {
                  userMsgID: part.messageID,
                  user: part.text,
                  reply: "",
                  tools: [],
                  lastAssistantID: t.lastAssistantID,
                })
              } else {
                t.user = part.text
              }
            } else if (role === "assistant") t.reply = part.text
          } else if (part.type === "tool" && part.tool) {
            if (!t.tools.includes(part.tool)) t.tools.push(part.tool)
          }
          break
        }
        case "session.idle":
          if (p?.sessionID) await submit(p.sessionID, "ok")
          break
        case "session.error":
          if (p?.sessionID) await submit(p.sessionID, "failed")
          break
        case "session.compacted":
          if (p?.sessionID) {
            await submitCompactionMarker(p.sessionID)
            // Fresh context window: re-arm the escalating warnings and
            // the auto-compact trigger for the next fill.
            ctxWarnedStep.delete(p.sessionID)
            autoCompacted.delete(p.sessionID)
          }
          break
      }
    },
  }
}

// ---------------------------------------------------------------------------
// OpenCode 2.x
// ---------------------------------------------------------------------------

const HANDOFF_REL = path.join("docs", "SESSION-STATE.md")
const HANDOFF_MAX_BYTES = 64 * 1024

// handoffRoot finds the project directory that opted into loading the
// handoff through opencode.json(c) `instructions` (what `aimem doctor` and
// install.sh wire): the nearest of `dir` and its parents, up to `stop`
// (the project root), whose config mentions docs/SESSION-STATE.md. The
// handoff file is resolved against that directory, as 1.x resolves
// `instructions` upward, so OpenCode started in a subdirectory still gets
// it. V1 resolves that entry itself; V2 accepts the field but ignores it,
// so the plugin injects the file for projects that asked.
function handoffRoot(dir: string, stop: string | undefined): string | undefined {
  const top = stop ? path.resolve(stop) : undefined
  for (let d = path.resolve(dir); ; ) {
    for (const rel of ["opencode.json", "opencode.jsonc", ".opencode/opencode.json", ".opencode/opencode.jsonc"]) {
      try {
        if (fs.readFileSync(path.join(d, rel), "utf8").includes("docs/SESSION-STATE.md")) return d
      } catch {}
    }
    const up = path.dirname(d)
    if (d === top || up === d || (top && !up.startsWith(top))) return undefined
    d = up
  }
}

// The 2.x events setupV2 acts on; anything else is skipped before the
// per-session ownership lookup.
const HANDLED = new Set([
  "session.inbox.enqueued",
  "session.inbox.cancelled",
  "session.inbox.delivered",
  "session.text.ended",
  "session.tool.input.started",
  "session.step.ended",
  "session.execution.succeeded",
  "session.execution.failed",
  "session.execution.interrupted",
  "session.compaction.started",
  "session.compaction.ended",
])

// setupV2 is the OpenCode 2.x entrypoint. It rebuilds the V1 plugin's
// behavior from V2 primitives:
//   turn journal     session.inbox.* (user text, taken on delivery) +
//                    session.text.ended (reply) + session.tool.input.started
//                    (tools), submitted on session.execution.succeeded /
//                    failed / interrupted
//   compaction       compaction hook appends HANDOFF_NOTE; a marker is
//                    journaled on session.compaction.ended
//   handoff          context hook injects docs/SESSION-STATE.md (V2
//                    ignores opencode.json `instructions`)
//   context warning  V2 has no toast API: past the warn fraction the
//                    context hook tells the MODEL instead, escalating
//                    by 5% step
// AIMEM_AUTO_COMPACT has no V2 equivalent — the plugin API cannot request
// compaction — so it is logged once and left to OpenCode's own
// `compaction` settings.
async function setupV2(ctx: any) {
  const directory: string | undefined = ctx?.location?.directory
  // OpenCode 1.18.x calls setup from an experimental host with a partial
  // context; only a full V2 context proceeds (see header comment).
  if (!directory || typeof ctx?.event?.subscribe !== "function" || typeof ctx?.session?.hook !== "function") return

  const CTX_LIMIT = knob(directory, "AIMEM_CTX_LIMIT", "ctx_limit", 0, false)
  const CTX_WARN_FRACTION = knob(directory, "AIMEM_CTX_WARN_FRACTION", "ctx_warn_fraction", 0.8, true)
  const AUTO_COMPACT = knob(directory, "AIMEM_AUTO_COMPACT", "auto_compact", 0, true)
  if (AUTO_COMPACT > 0) {
    console.error(
      "aimem: AIMEM_AUTO_COMPACT is not supported on OpenCode 2 (plugins cannot request compaction); " +
        "tune opencode.json `compaction` instead",
    )
  }
  const postDetached = makePoster(directory)
  // fallbackID names a turn that ends before any model step (it is fixed
  // when the turn starts); a turn is dropped once submitted.
  const turns = new Map<
    string,
    { user: string; reply: string; tools: string[]; lastAssistantID: string; fallbackID: string }
  >()
  const submitted = new Map<string, string>() // sessionID -> last submitted turn id
  const pending = new Map<string, string>() // inboxID -> user text, enqueued but not yet delivered
  const anchors = new Map<string, string>() // sessionID -> latest assistant message id, for markers
  const used = new Map<string, number>() // sessionID -> context tokens of the last model step
  const ctxWarnedStep = new Map<string, number>() // sessionID -> last logged 5%-step

  // The wiring changes rarely; re-check it at most every 30 s rather than
  // reading up to four config files per level on every model request.
  let wired: { root: string | undefined; at: number } | undefined
  const wiredRoot = (): string | undefined => {
    if (!wired || Date.now() - wired.at > 30_000) {
      wired = { root: handoffRoot(directory, ctx.location?.project?.directory), at: Date.now() }
    }
    return wired.root
  }

  // One V2 server can host several projects, and the event stream is not
  // guaranteed to be scoped to this plugin's location: journal only the
  // sessions that live here, resolved once per session.
  // Only definitive answers are cached; a failed or empty lookup (a
  // transient error, a session not yet readable) is retried next time.
  const owner = new Map<string, boolean>()
  // A thrown lookup is retried a few times: an event whose lookup fails is
  // dropped for good (the stream does not replay), so one transient error
  // must not cost a turn its request or its end event. A session that
  // stays unresolvable (e.g. deleted) is then treated as foreign for 5 s,
  // so a burst of its events does not each pay the retry delay.
  const unresolved = new Map<string, number>() // sessionID -> retry after (ms epoch)
  const mine = async (sid: string): Promise<boolean> => {
    const known = owner.get(sid)
    if (known !== undefined) return known
    if ((unresolved.get(sid) ?? 0) > Date.now()) return false
    for (let attempt = 0; attempt < 3; attempt++) {
      if (attempt > 0) await new Promise((r) => setTimeout(r, 200 * attempt))
      try {
        const res: any = await ctx.session.get({ sessionID: sid })
        const dir = res?.location?.directory ?? res?.data?.location?.directory
        if (typeof dir !== "string") continue
        const same = path.resolve(dir) === path.resolve(directory)
        owner.set(sid, same)
        unresolved.delete(sid)
        return same
      } catch {}
    }
    unresolved.set(sid, Date.now() + 5_000)
    return false
  }

  const turn = (sid: string) => {
    let t = turns.get(sid)
    if (!t) {
      t = { user: "", reply: "", tools: [], lastAssistantID: "", fallbackID: `no-assistant-${Date.now()}` }
      turns.set(sid, t)
    }
    return t
  }

  const submit = async (sid: string, outcome: "ok" | "failed") => {
    const t = turns.get(sid)
    if (!t) return
    const turnID = t.lastAssistantID || t.fallbackID
    // The execution is over either way: the next delivered prompt starts a
    // clean turn, and a second end event for this one finds nothing.
    turns.delete(sid)
    if (submitted.get(sid) === turnID) return
    const ok = await postDetached(turnPayload(directory, sid, turnID, outcome, t))
    if (ok) submitted.set(sid, turnID)
  }

  // A known limit is cached for the plugin's life. "No limit" (a model
  // without one, a failed or not-yet-loaded catalog) is re-checked after a
  // minute: never permanently, but not on every model request either.
  const limitCache = new Map<string, { value: Promise<number>; until: number }>()
  const contextLimit = (model: { providerID: string; id: string } | undefined): Promise<number> => {
    if (CTX_LIMIT > 0) return Promise.resolve(CTX_LIMIT)
    if (!model) return Promise.resolve(0)
    const key = `${model.providerID}/${model.id}`
    const hit = limitCache.get(key)
    if (hit && Date.now() < hit.until) return hit.value
    const entry = { value: Promise.resolve(0), until: Infinity }
    entry.value = (async () => {
      try {
        const res: any = await ctx.model?.list?.()
        const list: any[] = res?.data ?? (Array.isArray(res) ? res : [])
        const m = list.find((x) => x?.providerID === model.providerID && (x?.id === model.id || x?.modelID === model.id))
        const lim = Number(m?.limit?.context ?? 0)
        if (lim > 0) return lim
      } catch {}
      entry.until = Date.now() + 60_000
      return 0
    })()
    limitCache.set(key, entry)
    return entry.value
  }


  const tokensOf = (k: any): number =>
    k ? (k.input ?? 0) + (k.output ?? 0) + (k.reasoning ?? 0) + (k.cache?.read ?? 0) : 0

  // contextTokens is the usage of the latest model step still in context.
  // The session's own message list is read first: the step.ended event can
  // arrive after the next request's context hook has already run, which
  // would put every warning one request late. A compaction entry newer than
  // any measured step means the old usage no longer applies.
  const contextTokens = async (sid: string): Promise<number> => {
    try {
      const res: any = await ctx.session.context?.({ sessionID: sid })
      const msgs: any[] = Array.isArray(res) ? res : Array.isArray(res?.data) ? res.data : []
      for (let i = msgs.length - 1; i >= 0; i--) {
        if (msgs[i]?.type === "compaction") return 0
        if (msgs[i]?.type === "assistant" && msgs[i]?.tokens) return tokensOf(msgs[i].tokens)
      }
    } catch {
      // Fall back to the event-stream figure below.
    }
    return used.get(sid) ?? 0
  }

  await ctx.session.hook("compaction", async (e: any) => {
    // Only ever APPEND: the default compaction instructions stay intact.
    try {
      if (!e?.sessionID || !(await mine(e.sessionID))) return
      if (Array.isArray(e?.system)) e.system.push({ type: "text", text: HANDOFF_NOTE.trim() })
    } catch (err) {
      console.error("aimem compaction hook:", err)
    }
  })

  await ctx.session.hook("context", async (e: any) => {
    try {
      const sid: string | undefined = e?.sessionID
      if (!sid || !Array.isArray(e?.system) || !(await mine(sid))) return
      const existing = e.system.map((p: any) => (typeof p?.text === "string" ? p.text : "")).join("\n")

      const root = wiredRoot()
      if (root) {
        try {
          const file = path.join(root, HANDOFF_REL)
          const st = fs.statSync(file)
          if (st.isFile() && st.size > 0 && st.size <= HANDOFF_MAX_BYTES) {
            const body = fs.readFileSync(file, "utf8").replace(/^﻿/, "")
            // Skip when the handoff is already present, e.g. once OpenCode
            // starts honoring `instructions` itself.
            if (body.trim() && !existing.includes(body.trim().slice(0, 200))) {
              e.system.push({ type: "text", text: `Instructions from: ${file}\n${body}` })
            }
          }
        } catch {
          // No handoff yet is normal for a fresh project.
        }
      }

      // Limit first: without one no warning can fire, so the session's
      // message list is never fetched for nothing.
      const limit = await contextLimit(e.model)
      if (limit <= 0) return
      const tokens = await contextTokens(sid)
      if (tokens <= 0) return
      const frac = tokens / limit
      if (frac < CTX_WARN_FRACTION) return
      const pct = Math.round(100 * frac)
      const step = Math.floor(frac * 20)
      if (step > (ctxWarnedStep.get(sid) ?? -1)) {
        ctxWarnedStep.set(sid, step)
        console.error(`aimem: context ~${pct}% of ${limit} tokens (${e.model?.id ?? "model"})`)
      }
      // Worded by 5% step, not the live percentage: the system prompt then
      // changes at most once per step, so provider prompt caches survive.
      e.system.push({
        type: "text",
        text:
          `aimem: this session's context is over ${step * 5}% of the model's ${limit}-token window. ` +
          "Finish the smallest safe unit of work and update docs/SESSION-STATE.md before compaction.",
      })
    } catch (err) {
      console.error("aimem context hook:", err)
    }
  })

  const controller = new AbortController()
  const handle = async (ev: any) => {
    const d: any = ev?.data
    const sid: string | undefined = d?.sessionID
    if (!sid) return
    const type: string = ev?.type ?? ""
    if (!HANDLED.has(type) || !(await mine(sid))) return
    switch (type) {
      // The user request is taken at DELIVERY, not when the prompt is
      // submitted: a prompt queued behind a running turn (and maybe
      // cancelled) must not replace that turn's request. A steer
      // delivered into a running turn joins its request.
      case "session.inbox.enqueued":
        if (d.item?.type === "user" && typeof d.item?.payload?.text === "string" && d.inboxID) {
          pending.set(d.inboxID, d.item.payload.text)
          // Bounded: an item whose delivery never reaches this plugin
          // must not be kept for the server's life (oldest go first).
          while (pending.size > 200) pending.delete(pending.keys().next().value as string)
        }
        break
      case "session.inbox.cancelled":
        if (d.inboxID) pending.delete(d.inboxID)
        break
      case "session.inbox.delivered": {
        const text = d.inboxID ? pending.get(d.inboxID) : undefined
        if (text === undefined) break
        pending.delete(d.inboxID)
        const t = turn(sid)
        t.user = t.user ? `${t.user}\n\n${text}` : text
        break
      }
      case "session.text.ended":
        if (typeof d.text === "string") turn(sid).reply = d.text
        if (d.assistantMessageID) {
          turn(sid).lastAssistantID = d.assistantMessageID
          anchors.set(sid, d.assistantMessageID)
        }
        break
      case "session.tool.input.started": {
        const t = turn(sid)
        if (typeof d.name === "string" && !t.tools.includes(d.name)) t.tools.push(d.name)
        break
      }
      case "session.step.ended": {
        if (d.assistantMessageID) {
          turn(sid).lastAssistantID = d.assistantMessageID
          anchors.set(sid, d.assistantMessageID)
        }
        if (d.tokens) used.set(sid, tokensOf(d.tokens))
        break
      }
      case "session.execution.succeeded":
        await submit(sid, "ok")
        break
      case "session.execution.failed":
      case "session.execution.interrupted":
        await submit(sid, "failed")
        break
      case "session.compaction.started":
        // The pre-compaction usage no longer describes the window the
        // next request will see; drop it before the summary lands.
        used.delete(sid)
        break
      case "session.compaction.ended": {
        const anchor = turns.get(sid)?.lastAssistantID || anchors.get(sid) || `t${Date.now()}`
        await postDetached(markerPayload(directory, sid, anchor))
        // Fresh context window: re-arm the escalating warnings.
        used.delete(sid)
        ctxWarnedStep.delete(sid)
        break
      }
    }
  }
  // Re-subscribe whenever the stream ends or fails, with a short backoff:
  // a long-lived V2 server must not stop journaling after one stream reset.
  void (async () => {
    while (!controller.signal.aborted) {
      try {
        for await (const ev of ctx.event.subscribe({ signal: controller.signal })) await handle(ev)
      } catch (err) {
        if (!controller.signal.aborted) console.error("aimem event stream:", err)
      }
      if (!controller.signal.aborted) await new Promise((r) => setTimeout(r, 1000))
    }
  })()
  return () => controller.abort()
}

// Default export: `id` + `setup` for OpenCode 2.x, `server` for OpenCode
// 1.14+ (its loader takes `server` from a default object).
export default { id: "aimem", setup: setupV2, server: AimemPlugin }
