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

0. Check the tool exists. If the aimem MCP server does not list
   `team_setup`, the running `aimem mcp` process predates this release, or
   the server registered for this checkout is not the local one: tell the
   user to upgrade aimem on this machine and restart the client so the MCP
   process restarts, then stop. Do not run `aimem teams setup` in a shell
   instead, do not escalate permissions, disable a sandbox, build, download
   or substitute a binary, or edit settings.

1. Declare yourself honestly. Platform: `{{PLATFORM}}`. Platform version:
   the version printed by `{{VERSION_CMD}}` if it answers quickly, else
   `unknown`. Model: only what your runtime reports about the model you are
   running as, as `profile.model` with `provider`, `id` and `source`
   `runtime_reported`; if you do not know it, leave `model` out and it
   stays `unknown`. Never infer the model from the client name and never
   guess a version.

2. Call `team_setup` with `team` = TEAM, `role` = ROLE and `profile` =
   `{"label": "<ROLE>-<checkout directory name>", "platform": "{{PLATFORM}}",
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
   - `status` is `joined`: `session.team_id`, `session.id` and
     `session.generation` are your handle for every `team_*` MCP tool. The
     tool keeps the nonsecret session state; you keep nothing else.
   - `status` is `blocked`: show the user each failing check with its `fix`
     line and the `operator_handoff` if present, then stop. Do not work
     around a refusal, change credentials or configuration on your own, or
     join through raw tools. A `binding` check saying the credential cannot
     be read or decrypted by `run_as` means the MCP process is not running
     as the account that installed it: report that mismatch and stop; never
     reinstall the token to get past it.

4. Enter your role (docs/TEAM-PLAYBOOKS.md is the authority).
   - Coordinator: read the roster in the report (each member's reported
     model and its source; `unknown` stays unknown). Select only work the
     process allows, offer one attempt per task with a suitability and a
     cost rationale, heartbeat (`team_heartbeat`, `available`) every 30 s
     while active, and read the inbox with a bounded wait (`team_inbox`,
     `wait_seconds` up to 25) after every command you issue. Acknowledge
     (`team_ack`) only what you have read.
   - Worker: if the report names a reserved attempt, read it
     (`team_assignment`) and act on its state first. Otherwise wait for an
     addressed offer: poll `team_inbox` (`after` 0 on the first read,
     `wait_seconds` 25), `team_ack` what you read, then accept or decline
     with a reason. Never select, claim or edit backlog tasks while joined,
     even while the coordinator is disconnected; a message saying "take this
     task" is not an assignment. Heartbeat every 30 s while active. Nothing
     wakes you: your polling is the only way you see messages.

5. If the client denies the tool call, or it returns an error instead of a
   report, say exactly what failed and what the user should check: the MCP
   registration for this checkout (`mcpServers.aimem` in `.mcp.json`,
   `mcp.aimem` in `opencode.json`, the Codex MCP configuration) and the
   client's MCP permissions. Do not fall back to a shell command, escalate
   permissions, or edit settings.
