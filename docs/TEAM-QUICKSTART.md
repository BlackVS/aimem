# Start an agent team, step by step

The `/join_team` and `/resume_team` entry points call the `team_setup` and
`team_continue` tools of the checkout's local aimem MCP server, which the
client starts in the checkout as the account that installed the credential.
A shell the client sandboxes under another account (the Windows pilot's
case) cannot read that credential; the MCP process can, so joining and
resuming do not depend on the shell. Both sides need aimem at or after the
release that carries these tools; restart agent sessions after upgrading so
the MCP process restarts. Do not replace a valid token or grant broad
credential-directory access to work around a credential the shell cannot
read. See [checkout-bound onboarding over local MCP](DESIGN-team-onboarding-mcp.md).

Use this guide to start one coordinator and two workers in Claude Code,
Codex or OpenCode. Each member gets its own checkout and ordinary task
credential. The coordinator assigns work; workers wait for assignments.

Examples use project `my-project`, team `Builders`, and three new users.
Replace these names, the repository URL, paths and expiry with your own.
Commands are individual steps, not a script. Never paste tokens into an
agent conversation or commit them to Git.

## 1. Check prerequisites

Install aimem v0.7.1 or later on the hub and each participating machine.
Complete [client installation](INSTALL-CLIENT.md) and configure the target
hub. Run on each machine:

```sh
aimem version
aimem health
```

The health command checks the local service; check the hub on its host too.
Restart agent sessions after upgrading: an existing MCP process can still
be running the old binary.

Use the existing project's exact identity. On the hub, as its service user:

```sh
aimem tasks on -p my-project
```

This can create a missing project, so check the name before running it.
Configure the project's process and skills using the
[Kanban quickstart](KANBAN-QUICKSTART.md) before assigning work.

On Debian, from a root shell, switch with a login shell:

```sh
su - sessiond
export XDG_RUNTIME_DIR="/run/user/$(id -u)"
aimem health
```

Replace `sessiond` with the actual service user. If aimem is outside PATH,
use its installed absolute path, commonly `$HOME/.local/bin/aimem`.
The runtime directory must match the running service's socket location.

## 2. Create the team and credentials on the hub

Still as the hub service user, create a private delivery directory:

```sh
mkdir -p ~/.local/state/aimem/team-delivery
chmod 700 ~/.local/state/aimem/team-delivery
```

Choose a future expiry appropriate for the work; the date below is an example.

```sh
aimem teams provision create my-project --team Builders --coordinator team-coordinator --create-user --label token-team-coordinator --expiry 2026-12-31T00:00:00Z --secret-file ~/.local/state/aimem/team-delivery/coordinator.token
```

```sh
aimem teams provision add my-project --team Builders --member team-worker-1 --role worker --create-user --label token-team-worker-1 --expiry 2026-12-31T00:00:00Z --secret-file ~/.local/state/aimem/team-delivery/worker-1.token
```

```sh
aimem teams provision add my-project --team Builders --member team-worker-2 --role worker --create-user --label token-team-worker-2 --expiry 2026-12-31T00:00:00Z --secret-file ~/.local/state/aimem/team-delivery/worker-2.token
```

Each command creates or reuses a user, grants project access, enrolls the
user and issues a project-scoped token. Enrollment does not start an agent
session. Note the team ID from the output.

For an existing user with a valid credential, use `--no-token` instead of
the label, expiry and secret-file options. A rerun does not recover a lost
secret or silently replace a live token with the same label. Follow the
reported repair instructions; see [provisioning details](TEAM-SETUP.md#guided-provisioning).

## 3. Prepare one checkout per member

On the agent machine, clone the same repository three times under a team
directory. For example, from that directory:

```sh
git clone REPOSITORY_URL coordinator
git clone REPOSITORY_URL worker-1
git clone REPOSITORY_URL worker-2
```

Use the project's normal installer to wire each checkout. In each checkout,
ensure `.aimem.json` selects the same project and intended local hub name.
Preserve existing settings when editing it; the binding fields look like:

```json
{"project":"my-project","hub":"team"}
```

Deliver each token file securely to only its matching member. Keep temporary
files outside repositories. Do the following separately in each checkout.

PowerShell, using a securely delivered local file:

```powershell
cd C:\work\agent-team\worker-1
Get-Content -Raw C:\private\worker-1.token | aimem task-token set
aimem task-token show-source
aimem process show
aimem teams commands .
```

Linux, using a securely delivered local file:

```sh
cd ~/work/agent-team/worker-1
aimem task-token set < ~/private/worker-1.token
aimem task-token show-source
aimem process show
aimem teams commands .
```

Repeat for `worker-2` and `coordinator` with their own token files.
Expect `source: project-local`, the correct checkout, project and hub.
Verify the selected process is the one your project requires. Remove the
temporary delivery copies after successful installation.

`task-token set` reads stdin; `aimem task-token set TOKEN` is not supported.
The secret is stored outside Git in protected per-user storage on both
Windows and Linux. The checkout stores only the local-credential marker.

## 4. Start the coordinator

Open your chosen agent client in the coordinator checkout. Use its native
entry point inside the agent conversation, not in the operating-system shell:

| Client | First join | Continue after restart |
| --- | --- | --- |
| Claude Code | `/join_team Builders coordinator` | `/resume_team Builders` |
| Codex | `$join-team Builders coordinator` | `$resume-team Builders` |
| OpenCode | `/join_team Builders coordinator` | `/resume_team Builders` |

These commands call the `team_setup` tool with the platform and model
fields the agent declares; you should not normally have to construct a
call yourself. The `aimem teams setup` CLI command does the same from a
shell that can read the credential and shares the saved session state.

Ask the coordinator to verify its identity, team, role and reported model,
read the roster, and wait until you finish adding workers. A model supplied
by you is `operator_configured`; an unknown model must remain unknown.

## 5. Start the workers

Open a separate agent session in each worker checkout. Use the same client's
join command from the table, replacing `coordinator` with `worker`.
For example, in a Codex worker conversation:

```text
$join-team Builders worker
```

Have the coordinator check that both workers appear with the expected user,
platform, model and availability. Start with small independent tasks and
follow the [team playbooks](TEAM-PLAYBOOKS.md). Joining alone does not
authorize a worker to code: it waits for and accepts an addressed offer.
The repository's review, serial-PR and human-merge rules still apply.

## 6. Pause, restart or change clients

Starting the client alone does not resume team duties or poll the inbox.
Use the resume command from the table after restarting in the same checkout.
It calls `team_continue`, which reads the saved membership, reconciles
reserved work and unread messages, and restores the role's duties. It never
creates a new membership.

Closing a client does not leave the team. Missing heartbeats make the
session suspect; they do not free the coordinator slot. Before a planned
absence, tell the coordinator and arrange any accepted work. An occasional
third worker follows the same provisioning and resume steps; it need not
stay online, and the coordinator should assign only when it is available.

To switch Claude/Codex/OpenCode in the same checkout, stop the old agent
and its work processes first, then resume from the new client. Resume
preserves the saved profile: ask the agent to update its platform/model
through `team_profile` and have the coordinator verify it before new work.
Do not run two agents from the same checkout simultaneously.

To end membership, ask the agent to resolve its active work under the
playbook and leave through `team_leave`. A later return uses join, not
resume. Never force a new session just to hide an unresolved old one.

## The MCP process, the checkout and the model

The onboarding tools run inside the `aimem mcp` process the client
registers for the checkout (`mcpServers.aimem` in `.mcp.json`, `mcp.aimem`
in `opencode.json`, the Codex MCP configuration). That process is bound to
the directory the client started it in and to that account's state root:
it uses the checkout's own credential and cannot be pointed at another
checkout, token or command. Two things follow. The client must start the
MCP process as the account that ran `aimem task-token set` for that
checkout: the report's `run_as` names the account, and a `binding` check
saying the credential cannot be read or decrypted by it means a different
account started the process, not a bad token. And the process must be the
upgraded binary: an older one does not list `team_setup`, and the entry
point stops with an upgrade-and-restart message instead of falling back
to a shell command.

The reported model is a declaration, never an inference. The entry point
passes what the runtime reports about the running model as
`runtime_reported`; a model you configure for the agent is
`operator_configured`; anything else stays `unknown`, and the coordinator
reads it as such. Nothing derives a model from the client name.

## Checkout and token rules

| Question | Behavior |
| --- | --- |
| Do three checkouts on one host overwrite each other's tokens? | No. Local overrides are keyed to checkout paths under the operating-system user's state. |
| Can any agent client use a checkout's token? | Yes, if its aimem integration resolves that checkout. The token identifies the user, not the model or client. |
| What if two workers share a token? | They share one security principal, permissions and revocation. Distinct sessions do not provide separate user identities. Use separate users/tokens for this pilot. |
| Does copying or moving a checkout carry its token? | No. Configure the destination separately; clones and worktrees need their own local credential setup. |
| Does granting more projects expand this token? | A project token's writes stay scoped to its project. A user-scoped token follows live grants. Project scope is not private task-read isolation. |
| Does clearing a local token revoke it? | No. Local selection and hub revocation are separate operations. |
| Does join twice create two memberships? | Setup normally verifies the saved membership. Refused or uncertain handles require the reported recovery, not blind retries. |
| Will an idle agent wake when a message arrives? | No automatic wakeup is provided. An active agent performs bounded inbox reads; a restarted agent needs explicit resume. |

Keep the configured member checkout as the coordination root. An isolated
worktree for an assignment is a different path: do not accidentally join a
second membership there. Follow the playbook for worktree credentials and
run coordination against the original member context.

## Troubleshooting

| Symptom | Check or next step |
| --- | --- |
| `aimem: command not found` | Check PATH in the current account/login shell; try the known installed binary's absolute path. |
| Unix socket missing or permission denied | Check the service is running, the service user, and its runtime directory. A plain `su` can retain the root user's environment; use `su - SERVICE_USER`. |
| Token missing, expired, revoked or mismatched | Check `task-token show-source` from the configured root, binding, expiry and live grant. Install the correct replacement there. Required local credentials do not fall back to a broader token. |
| Not enrolled or not coordinator-eligible | Run the operator provisioning command printed by setup on the hub. A project grant alone is not team enrollment. |
| Join/resume command missing | Run `aimem teams commands .`, then restart the agent. Verify the client's invocation spelling in the table. |
| MCP team tools missing | Check the setup integration report and client MCP configuration; restart after repairing it. |
| `team_setup` or `team_continue` not listed by the aimem MCP server | The running `aimem mcp` process predates these tools: upgrade aimem on that machine and restart the client so the process restarts. Do not run the CLI command from a sandboxed shell instead. |
| `binding` check: credential cannot be read or decrypted by `run_as` | The MCP process runs as a different account than the one that installed the credential. Start the client as that account; do not reinstall the token to get past it. |
| Setup or continue prints old usage | Check binary version and PATH on that machine. Upgrade through the normal installer. |
| Session refused or coordinator slot occupied | Stop and read the report; verify who owns the existing session. Do not take it over or add `--new-session` blindly. |
| Resume shows a live session owned by your stopped process | Only after verifying the old process is gone, consider `aimem teams continue Builders --fence`; it invalidates the old generation. |
| Worker appears suspect or does not respond | Check whether the agent is running and polling/heartbeating; restart and explicitly resume if needed. |
| Codex says `helper_unknown_error: setup refresh had errors` before executing a command | The local command runner failed before aimem ran. Test a harmless command such as `Get-Location`, inspect Codex sandbox logs, and repair the runner before retrying join. This message alone does not identify the underlying cause. |

The Codex runner failure is a client problem and remains open; joining and
resuming no longer depend on it, because the entry points call the MCP
tools instead of a shell command. Do not change team credentials or weaken
the sandbox to compensate for a command that never started.

For deeper recovery and evidence collection, use the
[agent reference](TEAM-AGENT-QUICKSTART.md), [playbooks](TEAM-PLAYBOOKS.md)
and [audit export guide](TEAM-AUDIT-EXPORT.md).
