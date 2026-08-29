# aimem — User Manual

Daily use of aimem in a project. Setup lives in `ADMIN-MANUAL.md`.

## What happens automatically

Once installed, you normally never run aimem by hand:

- **Every Claude Code / OpenCode turn** is checkpointed to the project
  journal (request, reply summary, tools used, outcome) and pushed to the
  hub in real time. No LLM involved, no cost, no latency you'd notice.
- **On session start** (and after every compaction) the canonical handoff
  `docs/SESSION-STATE.md` is re-injected into the agent's context.
- **On compaction** a marker is journaled, so "what got compacted away" is
  recoverable from the journal afterwards.
- **Hourly** the hub distills recent journal events into durable facts
  (curation) and embeds them for semantic search; sync distributes facts
  to all machines. Projects with no new events cost nothing.
- **On every checkpoint** the handoff (`docs/SESSION-STATE.md`) is
  published to the hub as a shared document when its content changed —
  so other machines see the current handoff without a git push.
- **In-session**, agents use the aimem MCP tools (`recall_memory`,
  `remember`, `forget_memory`, `search_journal`, `review_memories` /
  `confirm_memory` for the staleness queue, and `list_docs` /
  `read_doc` / `update_doc` for shared documents) on their own.

## Recovering a broken session

A session crashed, hung, or compacted too aggressively:

1. Start a new session in the project — the handoff is injected
   automatically. Read it critically (claims are leads, not facts).
2. Recent turns: `aimem sessions -p $(aimem project-id .)` to find the
   session, then `aimem timeline -p <project> -s <session-id> -n 20`
   (or `aimem latest -p <project> -s <session-id>` for the newest event).
3. Full-text journal search: `aimem search -p <project> -q "the thing"`.
4. Durable knowledge: ask the agent to recall, or
   `aimem recall -p <project> -q "how do we deploy"`.

See `RECOVERY-RUNBOOK.md` for the step-by-step version.

## Working with memories

```sh
PID=$(aimem project-id .)

# Ask a question — hybrid BM25+semantic when embeddings are configured;
# paraphrases work, exact keywords not required. -n is a token budget:
aimem recall -p "$PID" -q "how do we authenticate to the code host"
                      [-n 500] [--tag storage] [--kind decision]

# Store a durable fact (kind: fact|decision|convention|solution|preference|reference):
aimem remember -p "$PID" --kind decision --tags deploy "we deploy via blue-green"

# List / maintain (-a includes stale/forgotten rows):
aimem memories -p "$PID"
aimem forget -p "$PID" --id <id>                  # refuses if pinned
aimem supersede -p "$PID" --id <id> "new text"    # keeps lineage
aimem pin -p "$PID" --id <id>                     # protect + rank first
aimem link -p "$PID" --id <id1> --to <id2> [--rel related]
```

Facts are never physically deleted — forget marks them expired
(bi-temporal), so history is auditable and sync can't resurrect them.

### Reviewing stale knowledge

Facts nobody contradicts can still quietly rot. The review queue lists
active, unpinned facts that are old, thinly corroborated, and untouched
since — derived from the audit trail, so reviewing is what empties it:

```sh
aimem review [-p "$PID"] [--days 30] [--max-corroboration 2]
aimem review confirm <id>          # verified still true (audited touch)
aimem supersede -p "$PID" --id <id> "updated text"   # state changed
aimem forget -p "$PID" --id <id>                     # obsolete
```

A confirmed fact re-queues after the age window passes again. Agents
see the same queue via the `review_memories` MCP tool.

Secrets are redacted on write; still, don't deliberately paste credentials
into memories.

### Scopes

- **project** — default, per-repo.
- **user** — your cross-project preferences (project ID `user`).
- **groups** — shared knowledge DBs; a repo opts in via `.aimem.json`:
  `{"groups":["ai-infra"]}`. Recall with scope=both surfaces group facts.
  The installer creates `.aimem.json` with empty groups (isolated) unless
  `AIMEM_GROUPS` was set; edit and commit it to join groups later.
- **session facts** — opt-in: `.aimem.json` `{"session_facts": 600}`
  injects up to that many tokens of recalled knowledge at session
  start, matched against the previous session's requests (mechanical,
  local, BM25-only without embeddings — zero egress). The agent is told
  to verify before relying. Absent or 0 = off.
- **context knobs** — optional per-project overrides for the OpenCode
  plugin: `{"auto_compact": 0.3, "ctx_warn_fraction": 0.25}` (and
  `ctx_limit`) for models that degrade mid-context well before the
  window fills. Host-wide defaults live in `~/.config/aimem/env`
  (ADMIN-MANUAL 3b); an explicit env var at launch wins over both.
- **hub binding** — with several hubs configured (e.g. work + home),
  `.aimem.json` `{"hub":"home"}` routes this project's data to that hub
  only; no `hub` key = the machine's default hub (`aimem hub` lists
  them, see ADMIN-MANUAL 3d).

## The handoff file (`docs/SESSION-STATE.md`)

Canonical session state, maintained by the agents per the protocol in
`AGENTS.md`: fixed section order, ~50-line cap, evidence next to every
claim, single writer, ends with a "Pick up here" line. You mostly just
benefit from it; edit it only if you're taking over the driving session.

## Shared documents

Whole files the project keeps current on every machine — the handoff by
default, plus anything listed in `.aimem.json` `{"docs":[...]}`
(`{"docs":[]}` opts a checkout out entirely). They version on the hub
with compare-and-swap: a conflicting write is refused and names the
other writer, never silently overwritten.

```sh
aimem docs list           # names, revisions, writers, local/hub state
aimem docs push [name]    # publish the local file (refuses on conflict)
aimem docs pull [name]    # fetch the hub copy (refuses to clobber local edits)
aimem docs diff [name]    # local vs hub
aimem docs merge <name>   # three-way merge vs the last-synced revision:
                          #   clean parts apply, overlaps become <<<<<<<
                          #   markers to resolve, then push
aimem docs log  [name]    # recent revisions
aimem docs rm   <name>    # retire (tombstone; --force required)
```

The handoff publishes automatically after any checkpoint that changed
it, and session start warns when the hub holds a newer revision than
this machine last saw. The console's Docs tab does the same over the
web, including revision history and restore.

## Compaction

- `/compact` (Claude Code) and OpenCode's summarize both leave a journal
  marker and re-inject the handoff afterwards; OpenCode summaries end with
  a verbatim `AIMEM HANDOFF:` reminder line.
- After any compaction, expect the agent to re-read the handoff and verify
  volatile claims against git/tests before continuing.
- Context lost to an aggressive compaction is not gone: recent turns are
  in the journal, durable facts in the knowledge DB.

## Dashboard

`aimem tui` opens a live read-only dashboard (refreshes every 2s) with
four tabs — `1:Projects` (stats table ranked by recent activity, selected
project detail, live event tail), `2:Groups` (shared scopes, declarers,
newest facts), `3:AI` (per-project, per-group, and per-model token usage
for today/7d/30d from the synced run history, plus budget state), and
`4:Hub` (real CPU gauge, memory/disk gauges, load, spool, timers).
Layout adapts to terminal size (compact mode under 80 columns). Keys:
`tab`/`1`-`4` switch view, `j`/`k` select project, `r` refresh, `q`
quit.

## Journal queries

```sh
aimem sessions -p "$PID"                       # sessions seen
aimem timeline -p "$PID" -s <session> -n 10    # events of one session
aimem latest  -p "$PID" -s <session>           # most recent event
aimem search  -p "$PID" -q "migration failed"  # journal FTS + shared docs
                                               # (doc hits show name+snippet;
                                               #  fetch with aimem docs pull)
```

## Curation (manual runs)

Normally the hub does this nightly. To distill fresh session events into
facts right now (needs LiteLLM key in env, see ADMIN-MANUAL):

```sh
aimem curate --backend openai --model gpt-4o-mini    # current project
aimem embed                                          # embed new facts
```

`--dry-run` previews proposals without writing. Each report prints token
usage. The claude backend (`--backend claude`) uses your headless `claude`
subscription instead of an API key. Spend limits: `aimem budget` (see
ADMIN-MANUAL 3c) — runs are refused before exceeding a cap; `--force`
bypasses.

## In-agent usage (MCP)

In Claude Code / OpenCode the same operations are exposed as MCP tools —
just ask in natural language: "recall what we decided about TLS",
"remember that X", "search the journal for the sync error". The project
and groups are derived from the working directory automatically.
