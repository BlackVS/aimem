// OpenCode adapter for aimem: observes the event bus and submits one
// normalized checkpoint per completed turn (session.idle) plus failure
// markers (session.error) and compaction markers (session.compacted).
// Persistence goes through `aimem submit`, which redacts adapter-side
// and spools when the service is down.
//
// Submits are a single DETACHED spawn (payload written synchronously,
// binary backgrounded via nohup): `opencode run` exits immediately after
// session.idle, and any awaited subprocess would be killed mid-flight with
// the checkpoint silently lost. Detaching lets the submit outlive the host
// process. Residual risk: teardown can still preempt the spawn itself.
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

export const AimemPlugin: Plugin = async ({ directory, client, $ }) => {
  const CTX_LIMIT = knob(directory, "AIMEM_CTX_LIMIT", "ctx_limit", 0, false)
  const CTX_WARN_FRACTION = knob(directory, "AIMEM_CTX_WARN_FRACTION", "ctx_warn_fraction", 0.8, true)
  const AUTO_COMPACT = knob(directory, "AIMEM_AUTO_COMPACT", "auto_compact", 0, true)
  const roles = new Map<string, string>() // messageID -> role
  const turns = new Map<string, TurnState>() // sessionID -> current turn
  const submitted = new Map<string, string>() // sessionID -> last submitted turn id
  const ctxWarnedStep = new Map<string, number>() // sessionID -> last warned 5%-step
  const autoCompacted = new Set<string>() // sessions where auto-compact fired (cleared on compaction)
  // Binary resolution: project-local build first (development), else PATH
  // (user-level install via install.sh).
  const localBin = `${directory}/aimem${process.platform === "win32" ? ".exe" : ""}`
  const bin = fs.existsSync(localBin) ? localBin : "aimem"

  const turn = (sid: string): TurnState => {
    let t = turns.get(sid)
    if (!t) {
      t = { userMsgID: "", user: "", reply: "", tools: [], lastAssistantID: "" }
      turns.set(sid, t)
    }
    return t
  }

  // postDetached hands one payload to `aimem submit` in a way that
  // survives host-process teardown (see header comment).
  const postDetached = async (payload: unknown) => {
    try {
      if (bin.includes("/") && !fs.existsSync(bin)) return
      const tmp = path.join(os.tmpdir(), `aimem-oc-${Date.now()}-${Math.random().toString(36).slice(2)}.json`)
      fs.writeFileSync(tmp, JSON.stringify(payload), { mode: 0o600 })
      if (process.platform === "win32") {
        // No nohup on Windows: detached spawn with stdin redirected from the
        // already-written temp file (payload safe even if the host dies).
        // The temp file is left behind — tiny, and the OS temp dir rotates.
        const { spawn } = await import("node:child_process")
        const fd = fs.openSync(tmp, "r")
        spawn(bin, ["submit"], { stdio: [fd, "ignore", "ignore"], detached: true, windowsHide: true }).unref()
        fs.closeSync(fd)
        return true
      }
      const cmd = `nohup "${bin}" submit < "${tmp}" >/dev/null 2>&1 && rm -f "${tmp}" &`
      await $`bash -c ${cmd}`.quiet()
      return true
    } catch (e) {
      // Fail-open: checkpointing must never break the session.
      console.error("aimem submit failed:", e)
      return false
    }
  }

  const submit = async (sid: string, outcome: "ok" | "failed") => {
    const t = turns.get(sid)
    if (!t) return
    const turnID = t.lastAssistantID || `no-assistant-${Date.now()}`
    if (submitted.get(sid) === turnID && outcome === "ok") return // idle re-fires
    // Deliberate: a turn that errors and then idles submits both outcomes
    // under ONE idempotency key — the journal keeps one event per turn,
    // whichever landed first; the service drops the other.
    const ok = await postDetached({
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
    })
    if (ok && outcome === "ok") submitted.set(sid, turnID)
  }

  const submitCompactionMarker = async (sid: string) => {
    const anchor = turns.get(sid)?.lastAssistantID || `t${Date.now()}`
    await postDetached({
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
    })
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
