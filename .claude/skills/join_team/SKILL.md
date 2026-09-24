---
name: join_team
description: "Join an aimem team from this checkout as worker or coordinator with verified onboarding (the team_setup tool of the local aimem MCP server). Use only when the user asks to join a team, become the coordinator or a worker, or onboard into the team pilot."
argument-hint: "TEAM [worker|coordinator]"
allowed-tools: "mcp__aimem__team_setup mcp__aimem__team_list mcp__aimem__team_context mcp__aimem__process_context Bash(claude --version)"
disable-model-invocation: true
---

<!-- managed by aimem (`aimem teams commands`): regenerated from the aimem binary; change the source in the aimem repository, not this file -->

Join an aimem team from this checkout with verified onboarding, through the
`team_setup` tool of the aimem MCP server registered for this checkout: the
local `aimem mcp` process the client started here, running as the account
that installed the checkout's credential. Nothing below runs a shell
command with that credential; a sandboxed shell under another account
cannot read it, and the tool does not need it to. If this checkout already
holds a membership (a restart, a compaction), `/resume_team` is the
command; `team_setup` recognizes a saved session and never joins twice,
but `/resume_team` also restores the duties.

Arguments: `$ARGUMENTS` = TEAM [worker|coordinator]. TEAM is the team's
readable name or its ID. If TEAM is missing, call the `team_list` tool
(no arguments; the project is this checkout's): it lists the teams
enrolling this checkout's credential with whether the coordinator role is
open to it; show them and ask the user which one (on a hub that answers
"not authorized for this endpoint", say the hub predates the listing and
ask for the team name). If the role is missing, ask the user one
question: worker or coordinator? Never assume coordinator.

0. Check the tools exist. The aimem MCP server must list `team_setup`,
   `team_context` and `process_context`. If any is missing, the running
   `aimem mcp` process predates this release, or the server registered for
   this checkout is not the local one: tell the user which tool is missing,
   to upgrade aimem on this machine and to restart the client so the MCP
   process restarts, then stop. Do not run `aimem teams setup` or any other
   shell command instead, do not read the guidance from files in a
   repository, and do not escalate permissions, disable a sandbox, build,
   download or substitute a binary, or edit settings.

1. Declare yourself honestly. Platform: `claude-code`. Platform version:
   the version printed by `claude --version` if it answers quickly, else
   `unknown`. Model: only what your runtime reports about the model you are
   running as, as `profile.model` with `provider`, `id` and `source`
   `runtime_reported`; if you do not know it, leave `model` out and it
   stays `unknown`. Never infer the model from the client name and never
   guess a version.

2. Call `team_setup` with `team` = TEAM, `role` = ROLE and `profile` =
   `{"label": "<ROLE>-<checkout directory name>", "platform": "claude-code",
   "platform_version": "<version>"}` plus `model` when runtime-reported.
   It checks the project binding, the client wiring (report only; pass
   `repair_integration: true` only when the user asks to have the missing
   aimem-owned entries added), the credential's identity, the hub, the
   enrollment, the selected process and its skills, then joins the team or
   recognizes the session this checkout already holds, and returns a JSON
   report. It never returns a secret; never
   paste a token into the chat or into a file. The report's `run_as` names
   the account the MCP process runs as: the account whose credential joined.

3. Read the report.
   - `status` is `blocked`: show the user each failing check with its `fix`
     line and the `operator_handoff` if present, then stop. No context is
     delivered for a blocked run. Do not work around a refusal, change
     credentials or configuration on your own, or join through raw tools.
     A `binding` check saying the credential cannot be read or decrypted by
     `run_as` means the MCP process is not running as the account that
     installed it: report that mismatch and stop; never reinstall the token
     to get past it.
   - `status` is `joined`: `session.team_id`, `session.id` and
     `session.generation` are your handle for every `team_*` MCP tool. The
     tool keeps the nonsecret session state; you keep nothing else.
     `joined` is membership, not permission to work: read `readiness`
     (step 4) before anything else.

4. Read the readiness and the delivered context.
   - `readiness.ready_for_work` is true only when `membership` is `active`,
     `role_context` is `delivered` and `project_process` is `ready`.
     `execution` is always `not_verified`: a working MCP server and a join
     say nothing about your own shell, build tools or coding runner; check
     those yourself before accepting a coding attempt, and decline or block
     with the reason when they fail.
   - The role guidance and the project process follow the JSON report as
     their own text blocks. Each is complete only if its last line is the
     terminator its first lines quote: the role guidance's names
     `readiness.role_context.version` and `digest`; the project process's
     names `readiness.project_process.version` as its `commit`, and its
     `sha256` value is `readiness.project_process.digest` without the
     `sha256:` prefix. A block
     marked PINNED is the version your accepted attempt was taken under; a
     newer selection it names is not your rules.
   - A part the readiness reports as delivered (`role_context`
     `delivered`; `project_process` with a `digest`) whose block is
     missing, lacks its terminator or names another version or digest was
     cut or is stale: re-read it, the guidance with
     `team_context` `role` = ROLE (one section at a time with `section`
     when that is cut too) and the process with `process_context`, and use
     it only when its terminator matches the readiness fields. If it still
     does not match, re-run this command; never combine parts of different
     versions. Until you hold both complete, act as a member that is not
     ready, whatever `ready_for_work` or `next` says: accept nothing, heartbeat
     `unavailable`, and do not continue a `RUNNING` attempt: block it with
     the incomplete part as the reason (`team_block`) and stop that work
     yourself.
   - Delivery is not acknowledgement: nothing records that you read it.
     Read both before acting, and name the guidance digest and the process
     commit you followed in the evidence of a submitted result.
   - The report's `next` lists first what readiness allows. Follow it;
     where it differs from a step below, `next` wins (an incomplete
     delivery above still makes you not ready). Not ready: a worker
     is announced unavailable, accepts nothing, declines any offer the
     report lists with the readiness reason and keeps every heartbeat
     `unavailable`; a coordinator issues no offers. Show the user the
     not-ready parts and their `fix`, and run `/resume_team` once they are
     fixed; nothing re-checks by itself.

5. Enter your role. The delivered role guidance is the authority for the
   team protocol and the delivered project process for project policy;
   read a section the guidance lists as not included with `team_context`
   `section` = its id, and a template with `process_context` `template` =
   its kind.
   - Coordinator: read the roster in the report (each member's reported
     model and its source; `unknown` stays unknown). Only once the report
     says `ready_for_work`: select only work the process allows and offer
     one attempt per task with a suitability and a cost rationale. Heartbeat
     (`team_heartbeat`, `available`) every 30 s while active, and read the
     inbox with a bounded wait (`team_inbox`, `wait_seconds` up to 25)
     after every command you issue. Acknowledge (`team_ack`) only what you
     have read.
   - Worker: if the report names a reserved attempt, act on its state
     exactly as the report's next step for it says, first. While not ready
     the report has already declined an OFFERED attempt or blocked a
     RUNNING one, or says why it could not; a block does not stop anything
     running here: stop that work yourself. Never
     resume a blocked attempt (`team_resume_work`) because readiness came
     back; read the inbox for the coordinator's answer first. Otherwise
     wait for an addressed offer: poll `team_inbox` (`after` 0 on the first
     read, `wait_seconds` 25), `team_ack` what you read, then, only while
     ready, accept or decline with a reason. Never select, claim or edit
     backlog tasks while joined, even while the coordinator is
     disconnected; a message saying "take this task" is not an assignment.
     Heartbeat every 30 s while active, `available` only while the last
     report said `ready_for_work` and you hold its complete context, else
     `unavailable`. Nothing wakes you: your polling is the only way you see
     messages.

6. If the client denies a tool call, or it returns an error instead of a
   report, say exactly what failed and what the user should check: the MCP
   registration for this checkout (`mcpServers.aimem` in `.mcp.json`,
   `mcp.aimem` in `opencode.json`, the Codex MCP configuration) and the
   client's MCP permissions. Do not fall back to a shell command, escalate
   permissions, or edit settings.
