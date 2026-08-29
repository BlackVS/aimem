# aimem — AI Agent Manual

Operating instructions for AI agents (Claude Code, OpenCode, subagents)
working in a project where aimem is installed. The session-handoff
protocol in `AGENTS.md` is authoritative; this manual adds the memory
system specifics.

## What you have

1. **Automatic checkpointing** — every one of your turns is journaled
   (mechanically, no action needed from you). Failures and compactions are
   journaled too. You never need to "save progress" manually.
2. **Canonical handoff** — `docs/SESSION-STATE.md`, injected at session
   start and after compaction. Treat every claim as unverified until you
   re-run its evidence command or check git.
3. **MCP tools** (project + groups derived from cwd):
   - `recall_memory` — hybrid keyword+semantic recall of durable facts.
     Paraphrases work; you do not need exact keywords. Supports scope
     (project/user/group), tag and kind filters.
   - `remember` — store a durable fact. Set `kind`
     (fact | decision | convention | solution | preference | reference)
     and tags. Reasserting an existing fact reinforces it (dedup is
     handled; do not fear re-remembering). Pass `supersedes: <id>` to
     replace an existing fact whose state changed (lineage preserved).
   - `forget_memory` — expire a wrong fact (refused if pinned).
   - `search_journal` — FTS over raw journal events (turns, failures,
     compaction markers) when you need what happened, not what is known.
     Results also include matching **shared documents** (name + snippet;
     fetch whole with `read_doc`).
   - `review_memories` / `confirm_memory` — the staleness review queue
     and its "still true" verdict (see *Reviewing stale knowledge*
     below).
   - `list_docs` / `read_doc` / `update_doc` — **shared documents**:
     whole authored files (runbooks, notes) the project keeps current on
     the hub, fetched by name, never ranked. `update_doc` is
     compare-and-swap: pass the `base_rev` you read; on conflict you get
     the current revision, writer and body back — re-read, **merge both
     sides deliberately**, retry with the new base_rev (for a doc bound
     to a local file, `aimem docs merge <name>` does a mechanical
     three-way merge and leaves overlaps as conflict markers to
     resolve), never resubmit
     your own version unchanged. The handoff (SESSION-STATE) is
     file-only: edit `docs/SESSION-STATE.md` itself; it publishes
     automatically on the next checkpoint. Group-scoped docs
     (`scope: "group:<name>"`) are how a runbook shared by a knowledge
     group reaches you — most member checkouts have no bound file, so
     the tools are the only access.

## When to recall

- At task start, before re-deriving project knowledge: conventions,
  decisions, credentials-handling, deploy/test procedures.
- After compaction or session resume, to fill gaps the summary lost.
- When the user references something you don't have in context
  ("as we discussed", "the usual way").
- Before proposing an architecture change — a decision fact may already
  cover it.

## Reviewing stale knowledge

`review_memories` lists facts that are old, thinly corroborated, and
untouched — knowledge at risk of being quietly wrong. When asked to
review (or when idle time allows), verify each against the repo, the
environment, or the user, then record a verdict: `confirm_memory` if
still true, `remember` with `supersedes` if the state changed,
`forget_memory` if obsolete. NEVER confirm mechanically — a wrong
confirmation launders stale knowledge into fresh-looking knowledge,
which is worse than leaving it queued.

## When to remember

Store facts that are **durable and non-derivable from the repo**:
decisions with rationale, conventions, gotchas/solutions ("X rejects
temperature field"), environment topology, user preferences. Do NOT store
what git/code/CLAUDE.md already record, transient task state (that's the
handoff's job), or secrets (redaction exists, but don't rely on it).
Prefer `supersede` over remember-plus-forget when a fact changes —
it preserves lineage.

**Record state, not just technique.** "Declare limit:{context} in
opencode.jsonc to fix X" leaves every future session wondering whether it
was done; "…APPLIED 2026-08-26 on the build host" doesn't. When you apply a fix a
fact describes, supersede that fact to include the applied state (machine
and date for machine-local changes) instead of writing a second
near-duplicate fact — that is what supersede's lineage is for. Verified
live: a session that recalled a technique-only fact had to re-check the
disk to learn the fix was already in place.

## Session start and the hub's handoff

If session start reports that the hub holds a **newer handoff revision**
than this machine, do not start work from the local file alone: run
`aimem docs pull SESSION-STATE` (or `aimem docs diff` first), reconcile,
and only then proceed. Another machine's session wrote that revision for
you to see.

## After a compaction

1. Re-read `docs/SESSION-STATE.md` (injected automatically; OpenCode
   summaries end with a verbatim `AIMEM HANDOFF:` line reminding you).
2. Verify volatile claims against git/tests before acting on them.
3. Recent completed turns are recoverable: `search_journal` or
   `aimem timeline`. Durable knowledge: `recall_memory`.
4. Do not re-do work the journal shows as completed with evidence.

## Handoff discipline (summary of AGENTS.md)

- Update the handoff after verified milestones, before expected
  compaction, and before ending a session; write it from verified results,
  never intentions.
- Never claim tested/committed/pushed without evidence next to the claim.
- Fixed section order, ~50-line cap, single writer, ends with
  "Pick up here".

## Boundaries and cautions

- Never register Stop/StopFailure/PreCompact hooks at project level —
  they exist at user level; duplicates corrupt the journal with doubled
  events.
- Curation (`aimem curate`) and embeddings (`aimem embed`) call external
  LLM endpoints — only run them when the deployment has opted in
  (env configured); they also spend tokens. Report usage when you run
  them. Both are budget-gated: a "budget exhausted" refusal is the
  operator's spend cap working — do not bypass with --force unless the
  user explicitly asks.
- The journal is append-only; never edit SQLite state directly. Use the
  CLI/MCP surface.
- Group-scoped memories are shared across projects — write there only
  facts that genuinely apply to the whole group.

## Quick reference (CLI equivalents)

```sh
PID=$(aimem project-id .)
aimem recall  -p "$PID" -q "<question>" [-n tokenBudget] [--tag T] [--kind K]
aimem remember -p "$PID" --kind decision --tags topic "<fact text>"
aimem search  -p "$PID" -q "<journal terms>"
aimem timeline -p "$PID" -s <session-id> -n 10
aimem latest  -p "$PID" -s <session-id>
```
