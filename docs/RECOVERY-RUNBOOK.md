# Recovery Runbook

Companion to `DESIGN.md`. Every command below was executed and verified
on 2026-08-26 with OpenCode 1.18.23 and Claude Code 2.1.246. Observed
results are stated with each step; anything not tested is marked as
such.

## OpenCode

### Where the data lives

- Database: `~/.local/share/opencode/opencode.db` (SQLite + `-wal`/`-shm`),
  found via `opencode db path`.
- Data dir also contains: `auth.json` (credentials — never include in any
  backup that leaves the machine unencrypted), `log/`, `repos/`, `snapshot/`,
  `tool-output/`.
- Logs: single file `~/.local/share/opencode/log/opencode.log` (observed at
  25MB — no rotation evident; collect with `tail -n 500` first, not `cat`).

### Listing sessions

```
opencode session list --format json          # scoped to the cwd's project
opencode session list --format json -n 10    # limit to 10 most recent
```

Observed: listing scope depends on cwd. From this project's directory, only
its one session appeared. From a directory that is not a tracked project, 29
sessions appeared — sessions created by older OpenCode versions carry
`projectID: "global"` and surface there. When hunting a lost session, list
from a neutral directory (e.g. `/tmp`) to see the global pool, and match on
the `directory` field.

Session identity: `projectId` for this repo is the root git commit hash
(`0f05aa6d...`), confirming project identity survives directory renames but
not re-inits.

### Resume and fork

```
opencode --session <id>                # interactive resume (TUI)
opencode run -s <id> "message"        # headless resume — appends to session
opencode run -s <id> --fork "message" # fork first, then run in the fork
```

Observed: `run -s <id> --fork` created `"<title> (fork #1)"` as a new session
ID and answered with full prior context. **Prefer `--fork` for any recovery
probe** — plain `run -s` appends to the real session. Delete a probe fork
afterward with `opencode session delete <fork-id>` (verified working).

### Export

```
opencode export <id> > session.json          # full content
opencode export <id> --sanitize > safe.json  # structure only
```

Two verified warnings:

1. **`--sanitize` removes ALL content, not just secrets.** Every message
   text, tool output, title, and path becomes `[redacted:...:id]`. A
   sanitized export is shareable evidence of structure and timing, and is
   useless for content recovery. Recovery requires the raw export, which must
   be treated as secret-bearing.
2. The `Exporting session: <id>` banner goes to stderr. Do **not** redirect
   with `2>&1` into the output file — it corrupts the JSON (verified: `jq`
   fails on the result).

Export shape: `{ "info": {...}, "messages": [...] }`.

### Import — DANGEROUS, read first

```
opencode import session.json
```

Verified behavior: importing an export whose session ID still exists does
**not** create a copy. It overwrites the existing session in place — same ID —
and **rebinds the session's `directory` to the cwd where the import ran**.
During testing this silently moved the real session to a scratch directory;
re-running the import from the correct project directory restored it.

Rules: only import into the directory the session should belong to; treat
import as restore-over, not restore-as-copy. To keep the current state before
an import, export it first.

### Recovery exercise (the previously hung project)

The incident that motivated this work — a project whose session hung the
client — was exercised read-only: `opencode export <session-id>`
succeeded and produced a **331MB** JSON file. Conclusions: the "lost" session data was
intact in the database the whole time and is recoverable by export; and the
session's sheer size is the plausible cause of the original hang —
supporting the proposal's per-record size caps. The client hanging does not
mean the data is gone: export by ID works without the TUI.

## Claude Code

### Where the data lives

- Sessions: `~/.claude/projects/<flattened-project-path>/<session-uuid>.jsonl`,
  one JSONL transcript per session, mode `0600` (verified). This session's
  file was 933KB while active.
- Auto memory: `~/.claude/projects/<project>/memory/`.
- Logs/cache: `~/.cache/claude-cli-nodejs/<flattened-project-path>/`
  (created per project as needed; contains MCP and tool logs when present).

### Listing sessions

No list subcommand; inventory by file:

```
ls -lt ~/.claude/projects/<flattened-project-path>/*.jsonl
```

Newest-modified file is the most recent session. The UUID filename is the
session ID used by `--resume`.

### Resume, clean start, and scripted checks

```
claude --resume            # interactive picker
claude --resume <uuid>     # resume specific session
claude -c                  # continue most recent session
claude -p "..." --output-format json   # fresh headless session
claude -p --resume <uuid> "..." --output-format json  # headless resume
```

Verified: a headless session was created (`session_id` returned in the JSON
result) and then resumed headlessly by ID; the resumed run kept the same
session ID and full context. This is the scripted path for testing whether a
session is resumable without opening the TUI.

No CLI deletion of sessions exists; test sessions remain as small `.jsonl`
files (acceptable residue — remove the file manually only if certain).

### Export

No native export command. The `.jsonl` transcript is the export: copy the
file. It contains full message content including tool outputs — treat as
secret-bearing, like the OpenCode raw export.

## Recovery workflow (tested order)

1. Inventory: `opencode session list --format json` from a neutral dir;
   `ls -lt ~/.claude/projects/<project>/` for Claude Code. Match on
   `directory` / project path.
2. Attempt normal resume (`opencode --session <id>` / `claude --resume <id>`).
   For a scriptable health check, use the headless forms above with a trivial
   prompt.
3. If resume hangs or fails: OpenCode — `opencode run -s <id> --fork "..."`
   or export by ID (works without the TUI, verified on the 331MB session).
   Claude Code — copy the `.jsonl` transcript aside, then start clean.
4. Start a clean session in the project directory and load
   `docs/SESSION-STATE.md`.
5. Consult only the needed slices of the export/transcript to reconstruct
   missing context. Expect huge files (331MB observed) — slice with `jq`/
   `grep`, never load whole.
6. Verify repository, test, and CI state before continuing work.

## Log collection after a failure

```
tail -n 500 ~/.local/share/opencode/log/opencode.log
ls -lt ~/.cache/claude-cli-nodejs/<project>/ 2>/dev/null
ls -lt ~/.claude/projects/<project>/          # newest .jsonl = last session
```

Copy relevant slices into the incident note; do not commit raw logs (both may
contain repository content and command output).
