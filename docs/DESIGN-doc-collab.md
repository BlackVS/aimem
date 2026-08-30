# Multi-writer document collaboration — proposal

Status: **proposal**, drafted 2026-08-30 (user request: "design first").
Companion to DESIGN-shared-docs.md, whose storage contract this
deliberately does NOT change.

## The problem

Shared documents already have safe storage (CAS, refuse-never-
overwrite) and a real merge tool (`aimem docs merge`), but the
*reconciliation* between writers is manual at every step:

1. **Console edit → clients.** After an edit in the console, every
   machine keeps serving its now-stale local file until a human runs
   `docs pull`. Only the handoff, and only at session start, even says
   a newer revision exists. A machine with NO local changes — the
   overwhelmingly common case — should just receive the new version,
   the way `git pull` fast-forwards.
2. **Console edit ∧ client edit.** The diverged machine's checkpoint
   gets a 409 and a log line; a human must notice it and run
   `docs merge`. When the merge would be CLEAN (disjoint edits — the
   console touched the header while the agent rewrote "Pick up here"),
   requiring a human at all is pure friction.
3. **Client edit → console, mid-edit.** Someone editing in the console
   learns about a newer revision only when their save bounces, and the
   recovery is "copy your text somewhere" — the console has no merge.

## The model (git vocabulary, deliberately)

Per machine and document: the bound file is the **working tree**, the
sidecar `{rev, hash}` is the **last-synced base** (like a remote-
tracking ref), the hub is the **remote**. Four states, four verdicts:

| local file vs sidecar | hub vs sidecar rev | verdict |
|---|---|---|
| unchanged | equal | in sync — nothing |
| unchanged | newer | **fast-forward pull** (safe: nothing local to lose) |
| changed | equal | publish (exists today) |
| changed | newer | **diverged** → three-way merge, base = sidecar rev from the hub's retained history |

A diverged merge has two outcomes: **clean** (no overlapping hunks) →
the merged text is both written locally and published — no human; or
**conflicted** → a human/agent must resolve, and nothing is touched
silently.

## Proposal

### 1. Client side: reconcile during periodic sync

The 10-minute `aimem sync` gains a docs step (one `docs list` call per
hub — bulk revs — then per-doc work only where revs differ). Per bound
doc of each synced project:

- unchanged + hub newer → overwrite the file, update the sidecar, note
  it in adapter.log ("SESSION-STATE fast-forwarded to rev N (by
  console)"). Machines converge within ten minutes of a console edit.
- diverged + clean merge → write merged file, publish it, update
  sidecar; adapter.log records "auto-merged rev N + local edits →
  rev N+1". The push-back the use case asks for, with no human.
- diverged + conflicts → do NOT touch the file. Persist a loud
  adapter.log warning (surfaces in `aimem logs`), and the session-start
  notice (extended beyond the handoff to any conflicted bound doc)
  tells the agent: run `aimem docs merge <name>`. Writing conflict
  markers into a file nobody asked to merge is a git behavior we
  deliberately do NOT copy — an unattended editor buffer or a running
  agent could be mid-read.
- hub tombstoned → never delete a local file automatically; warn only.

The checkpoint-time publisher stays exactly as it is (push-only, zero
HTTP when unchanged — the free case stays free). Reconciliation is
sync's job; capture never grows a network dependency.

### 2. Console side: know sooner, merge in place

- **Staleness banner.** While a doc is open in the editor, poll its rev
  (~every 15s, HEAD-weight GET). If it moves: a banner — "rev N+1 by
  dmbunker/… arrived — [merge into my draft] [discard mine, load
  theirs]" — editor text untouched.
- **Server-side merge preview.** `POST /v1/projects/{p}/docs/{name}/
  merge {base_rev, body}` → `{merged, conflicts, against_rev}`.
  Compute-only (no write): the server fetches the base revision from
  retained history and the current doc, runs the existing
  `internal/diff3`, returns the result. Writer role; in the OpenAPI
  spec via the parity test. This gives the console (and any future
  client) three-way merge without porting diff3 to JS.
- **One-click conflict recovery.** A 409 on save now auto-calls the
  merge preview: clean → editor holds the merged text against the
  current rev, one more save finishes; conflicts → editor holds the
  marker-annotated text with markers highlighted, the user resolves
  and saves. "Copy your text somewhere" dies.

Retry convergence: a save racing yet another writer just 409s again and
re-merges against the newer rev — the loop converges because each
iteration rebases onto the latest revision.

### 3. What explicitly does not change

- **The hub never merges on write.** CAS refuse-and-return stands; the
  merge endpoint is a calculator, not a writer.
- Conflict markers can never reach the hub unnoticed: client-side, the
  sidecar-hash trick already guarantees a conflicted merge result
  cannot auto-publish; the console shows markers in the editor before
  any save.
- The handoff's session-start injection, `docs merge`, `docs pull
  --force`, and the MCP conflict flow all remain; this layers on top.

## Non-goals

- **Not live co-editing.** No OT/CRDT, no cursors, no presence. The
  unit of collaboration stays "one revision at a time, merged like
  git", which matches how agents and humans actually touch these docs.
- **Not auto-resolution of overlapping edits.** Overlap = human/agent
  judgment, always.
- **Not per-keystroke sync.** Ten-minute convergence plus the session-
  start notice plus the console banner cover the real cadences.

## Open questions

1. Opt-out knob? Propose none initially — FF-pull of an unchanged file
   and clean-merge-and-push are both loss-free by construction; an
   `.aimem.json "docs_autosync": false` can be added if a real workflow
   objects.
2. Should the *conflicted* case also drop a `<name>.merge` preview file
   next to the bound file (markers included, original untouched)?
   Leaning yes — it makes `docs merge`'s work inspectable before
   running it — but it creates an untracked file some repos will hate.
3. Console poll interval and whether the banner also appears in the
   Docs *listing* (not just the open editor).
4. Does the sync docs step cover group docs (no bound file anywhere)?
   Nothing to reconcile locally — listing them is enough; MCP remains
   their surface.

## Why this is worth building

Every piece already exists — diff3, retained history, sidecar bases,
the sync loop, the CAS contract. This proposal only moves the *running*
of those pieces from "a human notices a log line" to the machinery
itself, at exactly the two moments the use cases name: a clean
follower should follow silently, and a clean merge should merge
silently. Humans are reserved for the one case that genuinely needs
them: overlapping intent.
