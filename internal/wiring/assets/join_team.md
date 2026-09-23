Join an aimem team from this checkout with verified onboarding. If this
checkout already holds a membership (a restart, a compaction), `/resume_team`
is the command; the setup below recognizes a saved session and never joins
twice, but `/resume_team` also restores the duties.

Arguments: `$ARGUMENTS` = TEAM [worker|coordinator]. TEAM is the team's
readable name (quote it if it has spaces) or its ID. If TEAM is missing, run
`aimem teams mine <project>` (the project from `.aimem.json`, or as reported
by `aimem task-token show-source`): it lists the teams enrolling this
checkout's credential with whether the coordinator role is open to it; show
them and ask the user which one (on a hub that answers "not authorized for
this endpoint", say the hub predates the listing and ask for the team name).
If the role is missing, ask the user one question: worker or coordinator?
Never assume coordinator.

1. Declare yourself honestly. Platform: `{{PLATFORM}}`. Platform version:
   the version printed by `{{VERSION_CMD}}` if it answers quickly, else
   `unknown`. Model: only what your runtime reports about the model you are
   running as, passed as `--model-provider <provider> --model-id <id>
   --model-source runtime_reported`; if you do not know it, pass no model
   flags and it stays `unknown`. Never infer the model from the client name
   and never guess a version.

2. Run, from the checkout root (the directory holding `.aimem.json`):

   aimem teams setup "<TEAM>" <ROLE> --platform {{PLATFORM}} --platform-version <version> [--model-provider <provider> --model-id <id> --model-source runtime_reported] --label <ROLE>-<checkout directory name> --json

   It checks the project binding, the client wiring (adding only what aimem
   owns), the credential's identity, the hub, the selected process and its
   skills, then joins the team or recognizes the session this checkout
   already holds, and prints a JSON report. It never prints a secret; never
   paste a token into the chat or into a file.

3. Read the report.
   - `status` is `joined`: `session.team_id`, `session.id` and
     `session.generation` are your handle for every `team_*` MCP tool. The
     command keeps the nonsecret session state; you keep nothing else.
   - `status` is `blocked`: show the user each failing check with its `fix`
     line and the `operator_handoff` if present, then stop. Do not work
     around a refusal, change credentials or configuration on your own, or
     join through raw tools.

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

5. If `aimem` is not on PATH, the command is denied, or no report is
   produced, say exactly what failed and what the user should run. If the
   installed `aimem` answers `teams setup` with a usage text instead of a
   report, it predates this command: tell the user to upgrade aimem to a
   release that has `aimem teams setup` (the one-line installer, or the hub
   installer on a hub host) and stop. Do not build, download or substitute
   a binary yourself, escalate permissions, or edit settings.
