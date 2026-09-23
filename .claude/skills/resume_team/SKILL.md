---
name: resume_team
description: Continue the aimem team membership this checkout already holds after a client restart or compaction (aimem teams continue): verify or resume the session and take up the reserved attempt and unacknowledged inbox. Use only when the user asks to resume, continue or pick up team work; it never joins.
argument-hint: [TEAM]
allowed-tools: Bash(aimem teams continue *) Bash(git status *)
disable-model-invocation: true
---

<!-- managed by aimem (`aimem teams commands`): regenerated from the aimem binary; change the source in the aimem repository, not this file -->

Continue the aimem team membership this checkout already holds, after a
client restart or a context compaction. This is an explicit step: client
startup alone and an idle model never poll the team or wake up; running this
command is how you find out what waits for you.

Arguments: `$ARGUMENTS` = [TEAM]. Optional; when given it must be the saved
team. Never pass a role: it comes from the saved membership.

1. Run, from the checkout root (the directory holding `.aimem.json`):

   aimem teams continue [TEAM] --json

   It verifies the binding, the credential and the hub, then the saved
   session: a session still heartbeating is verified as is; one the hub
   reports suspect (no heartbeat within its window) is resumed, which
   fences the old handle; a handle the hub refuses is reported. It never
   joins: a membership that has ended is reported, and `/join_team` is the
   only way back in. Add `--fence` only when you know the old process of
   your own is gone and the hub still shows the session live. If the
   installed `aimem` answers `teams continue` with a usage text, it predates
   this command: tell the user to upgrade aimem and stop; never build,
   download or substitute a binary.

2. Read the report.
   - `status` is `joined`: `session.team_id`, `session.id` and
     `session.generation` are your handle for every `team_*` MCP tool. If
     the report says the session was resumed, the generation changed: use
     the new one everywhere from now on.
   - `status` is `blocked`: show the user each failing check with its `fix`
     line, then stop. Do not join, do not change credentials or
     configuration, do not retry blindly.

3. Reconcile before you retry anything (the hub cannot see local effects):
   the `reserved` attempt and its `state` are what the hub acknowledged;
   compare HEAD with the recorded base commit the report names; check
   `git status` for uncommitted work; check yourself whether a child process
   of the old session still runs. Retry an uncertain command only with its
   original idempotency key and content; never re-issue a new one "to make
   sure".

4. Take up the duties the report lists, in this order:
   - Worker with a reserved attempt: act on its state exactly as the
     report's next step says (OFFERED: accept or decline with a reason;
     RUNNING: continue the work; BLOCKED: wait for the answer, then
     resume-work; STOP_REQUESTED: stop safely, then `team_stopped`;
     STOPPED: wait for close-stop; SUBMITTED: wait for the review).
   - Then the unacknowledged inbox messages listed in the report: read each
     (offers, cancellations, reviews, recoveries, questions), act, and
     `team_ack` only the ones you consumed; the report's `next_cursor` is
     where your next `team_inbox` read starts.
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

5. If `aimem` is not on PATH, the command is denied, or no report is
   produced, say exactly what failed and what the user should run. Do not
   escalate permissions or edit settings.
