Continue the aimem team membership this checkout already holds, after a
client restart or a context compaction, through the `team_continue` tool of
the aimem MCP server registered for this checkout (the local `aimem mcp`
process, running as the account that installed the credential). This is an
explicit step: client startup alone and an idle model never poll the team
or wake up; calling this tool is how you find out what waits for you.

Arguments: `$ARGUMENTS` = [TEAM]. Optional; when given it must be the saved
team. Never pass a role: it comes from the saved membership.

0. Check the tool exists. If the aimem MCP server does not list
   `team_continue`, the running `aimem mcp` process predates this release:
   tell the user to upgrade aimem on this machine and restart the client,
   then stop. Do not run `aimem teams continue` in a shell instead, do not
   escalate permissions, disable a sandbox, build, download or substitute
   a binary, or edit settings.

1. Call `team_continue` with `team` = TEAM when given, nothing else.

   It verifies the binding, the credential and the hub, then the saved
   session: a session still heartbeating is verified as is; one the hub
   reports suspect (no heartbeat within its window) is resumed, which
   fences the old handle; a handle the hub refuses is reported. It never
   joins: a membership that has ended is reported, and `/join_team` is the
   only way back in. Pass `fence: true` only when you know the old process
   of your own is gone and the hub still shows the session live.

2. Read the report.
   - `status` is `joined`: `session.team_id`, `session.id` and
     `session.generation` are your handle for every `team_*` MCP tool. If
     the report says the session was resumed, the generation changed: use
     the new one everywhere from now on.
   - `status` is `blocked`: show the user each failing check with its `fix`
     line, then stop. Do not join, do not change credentials or
     configuration, do not retry blindly. A `binding` check saying the
     credential cannot be read or decrypted by `run_as` means the MCP
     process is not running as the account that installed it: report that
     mismatch and stop.

3. Reconcile before you retry anything (the hub cannot see local effects):
   the `reserved` attempt and its `state` are what the hub acknowledged;
   compare HEAD with the recorded base commit the report names (the tool
   records HEAD but does not probe the working tree); check `git status`
   yourself for uncommitted work; check whether a child process of the old
   session still runs. Retry an uncertain command only with its original
   idempotency key and content; never re-issue a new one "to make sure".

4. Take up the duties the report lists, in this order:
   - Worker with a reserved attempt: act on its state exactly as the
     report's next step says (OFFERED: accept or decline with a reason;
     RUNNING: continue the work; BLOCKED: wait for the answer, then
     resume-work; STOP_REQUESTED: stop safely, then `team_stopped`;
     STOPPED: wait for close-stop; SUBMITTED: wait for the review).
   - Then the unacknowledged inbox messages the report lists. The list is a
     summary (an excerpt of each text, no refs, deadlines or reply
     context): before acting on or acknowledging any of them, read them in
     full with `team_inbox` from cursor 0 (`after` 0, `wait_seconds` 0),
     then act (offers, cancellations, reviews, recoveries, questions) and
     `team_ack` only the ones you consumed; the read's `next_cursor` is
     where your later reads start.
   - Worker without a reserved attempt: wait for an addressed offer by
     polling `team_inbox` (`wait_seconds` 25) and never select, claim or
     edit backlog tasks while joined, even while the coordinator is
     disconnected.
   - Coordinator: read the roster in the report (availability, reported
     model and its source, suspect flags), answer questions and act on
     submissions in the inbox, then continue the coordinator playbook in
     docs/TEAM-PLAYBOOKS.md.
   - Both: heartbeat (`team_heartbeat`, `available`) every 30 s while
     active, and read the inbox with a bounded wait after every command
     you issue.

5. If the client denies the tool call, or it returns an error instead of a
   report, say exactly what failed and what the user should check (the MCP
   registration for this checkout and the client's MCP permissions). Do not
   fall back to a shell command, escalate permissions or edit settings.
