Continue the aimem team membership this checkout already holds, after a
client restart or a context compaction, through the `team_continue` tool of
the aimem MCP server registered for this checkout (the local `aimem mcp`
process, running as the account that installed the credential). This is an
explicit step: client startup alone and an idle model never poll the team
or wake up; calling this tool is how you find out what waits for you. A
compaction discards the context you were given: this call delivers it
again, and nothing you remember from before it replaces what it delivers.

Arguments: `$ARGUMENTS` = [TEAM]. Optional; when given it must be the saved
team. Never pass a role: it comes from the saved membership.

0. Check the tools exist. The aimem MCP server must list `team_continue`,
   `team_context` and `process_context`. If any is missing, the running
   `aimem mcp` process predates this release: tell the user which tool is
   missing, to upgrade aimem on this machine and to restart the client,
   then stop. Do not run `aimem teams continue` or any other shell command
   instead, do not read the guidance from files in a repository, and do
   not escalate permissions, disable a sandbox, build, download or
   substitute a binary, or edit settings.

1. Call `team_continue` with `team` = TEAM when given, nothing else.

   It verifies the binding, the credential and the hub, then the saved
   session: a session still heartbeating is verified as is; one the hub
   reports suspect (no heartbeat within its window) is resumed, which
   fences the old handle; a handle the hub refuses is reported. It never
   joins: a membership that has ended is reported, and `/join_team` is the
   only way back in. Pass `fence: true` only when you know the old process
   of your own is gone and the hub still shows the session live.

2. Read the report.
   - `status` is `blocked`: show the user each failing check with its `fix`
     line, then stop. No context is delivered for a blocked run. Do not
     join, do not change credentials or configuration, do not retry
     blindly. A `binding` check saying the credential cannot be read or
     decrypted by `run_as` means the MCP process is not running as the
     account that installed it: report that mismatch and stop.
   - `status` is `joined`: `session.team_id`, `session.id` and
     `session.generation` are your handle for every `team_*` MCP tool. If
     the report says the session was resumed, the generation changed: use
     the new one everywhere from now on. `joined` is membership, not
     permission to work: read `readiness` (step 3) before anything else.

3. Read the readiness and the delivered context.
   - `readiness.ready_for_work` is true only when `membership` is `active`,
     `role_context` is `delivered` and `project_process` is `ready`.
     `execution` is always `not_verified`: a working MCP server and a
     verified session say nothing about your own shell, build tools or
     coding runner; check those yourself before accepting a coding attempt
     or continuing a running one, and decline or block with the reason
     when they fail.
   - The role guidance and the project process follow the JSON report as
     their own text blocks. Each is complete only if its last line is the
     terminator its first lines quote: the role guidance's names
     `readiness.role_context.version` and `digest`; the project process's
     names `readiness.project_process.version` as its `commit`, and its
     `sha256` value is `readiness.project_process.digest` without the
     `sha256:` prefix. A block marked PINNED is the version your accepted
     attempt was taken under; a newer selection it names is not your
     rules. A `role context` warning says the guidance changed since it
     was last delivered to this membership: read it again.
   - A part the readiness reports as delivered (`role_context`
     `delivered`; `project_process` with a `digest`) whose block is
     missing, lacks its terminator or names another version or digest was
     cut or is stale: re-read it, the guidance with `team_context` `role` =
     the saved role (one section at a time with `section` when that is cut
     too) and the process with `process_context`, and use it only when its
     terminator matches the readiness fields. If it still does not match,
     run this command again; never combine parts of different versions.
     Until you hold both complete, act as a member that is not ready,
     whatever `ready_for_work` or `next` says: accept nothing, heartbeat
     `unavailable`, and do not continue a `RUNNING` attempt: block it with
     the incomplete part as the reason (`team_block`) and stop that work
     yourself.
   - Delivery is not acknowledgement: nothing records that you read it.
     Read both before acting, and name the guidance digest and the process
     commit you followed in the evidence of a submitted result.
   - The report's `next` lists first what readiness allows. Follow it;
     where it differs from a step below, `next` wins (an incomplete
     delivery above still makes you not ready). Not ready: a worker
     is announced unavailable, accepts nothing, keeps every heartbeat
     `unavailable` and does not continue or resume work; a coordinator
     issues no offers. Show the user the not-ready parts and their `fix`,
     and run this command again once they are fixed; nothing re-checks by
     itself.

4. Reconcile before you retry anything (the hub cannot see local effects):
   the `reserved` attempt and its `state` are what the hub acknowledged;
   compare HEAD with the recorded base commit the report names (the tool
   records HEAD but does not probe the working tree); check `git status`
   yourself for uncommitted work; check whether a child process of the old
   session still runs. Retry an uncertain command only with its original
   idempotency key and content; never re-issue a new one "to make sure".

5. Take up the duties the report lists, in this order. The delivered role
   guidance is the authority for the team protocol and the delivered
   project process for project policy; read a section the guidance lists
   as not included with `team_context` `section` = its id, and a template
   with `process_context` `template` = its kind.
   - Worker with a reserved attempt: act on its state exactly as the
     report's next step for it says. While ready: OFFERED: accept or
     decline with a reason; RUNNING: continue the work; BLOCKED: wait for
     the answer, then resume-work; STOP_REQUESTED: stop safely, then
     `team_stopped`; STOPPED: wait for close-stop; SUBMITTED: wait for the
     review. While not ready the report has already declined an OFFERED
     attempt or blocked a RUNNING one, or says why it could not; a block
     does not stop anything running here: stop that work yourself. Never
     resume a blocked attempt (`team_resume_work`) because readiness came
     back; read the inbox for the coordinator's answer first. A stop is
     never acknowledged for you: send `team_stopped` only once you have
     established that the work stopped.
   - Then the unacknowledged inbox messages the report lists. The list is a
     summary (an excerpt of each text, no refs, deadlines or reply
     context): before acting on or acknowledging any of them, read them in
     full with `team_inbox` from cursor 0 (`after` 0, `wait_seconds` 0),
     then act (offers, cancellations, reviews, recoveries, questions) and
     `team_ack` only the ones you consumed; the read's `next_cursor` is
     where your later reads start.
   - Worker without a reserved attempt: wait for an addressed offer by
     polling `team_inbox` (`wait_seconds` 25), accept one only while ready,
     and never select, claim or edit backlog tasks while joined, even
     while the coordinator is disconnected.
   - Coordinator: read the roster in the report (availability, reported
     model and its source, suspect flags), answer questions and act on
     submissions in the inbox, then continue the coordinator guidance;
     issue offers only while the report says `ready_for_work`.
   - Both: heartbeat (`team_heartbeat`) every 30 s while active, and read
     the inbox with a bounded wait after every command you issue. A
     worker heartbeats `available` only while the last report said
     `ready_for_work` and it holds that report's complete context, else
     `unavailable`; a coordinator heartbeats `available`.

6. If the client denies a tool call, or it returns an error instead of a
   report, say exactly what failed and what the user should check (the MCP
   registration for this checkout and the client's MCP permissions). Do not
   fall back to a shell command, escalate permissions or edit settings.
