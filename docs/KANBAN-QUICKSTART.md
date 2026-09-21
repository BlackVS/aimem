# Kanban quickstart

Release preparation (2026-09-21): this guide describes the v0.6.0 token
model, merged in PR #56 and #57. Further project onboarding remains on
hold until the release is deployed and the two-project workflow verified.
See [the roadmap](ROADMAP.md) for the required delivery sequence.

Enable a project's task board, give a person or agent access, and connect
the agent's MCP tools. Use aimem v0.6.0 or a compatible newer release on
both the hub and agent machines. For a first installation, follow
[the general quickstart](QUICKSTART.md) and [client setup](INSTALL-CLIENT.md).

Examples use `hub.example.com`, local hub name `team`, project `my-project`,
and the installer's default service account `sessiond`. Substitute your
actual values. Keep credentials out of project files and Git.

## 1. Identify the project and open the operator shell

On the agent machine, inside the project directory:

```sh
aimem version
aimem project-id .
```

Use that exact project ID throughout. If it does not exist on the intended
hub yet, the enablement command in step 2 creates it. Do not change an
established project ID just to match this example.

For the following hub-side commands, log in with an authorized SSH key
and switch to the service account:

```sh
ssh root@hub.example.com
su - sessiond
export PATH="$HOME/.local/bin:$PATH"
export XDG_RUNTIME_DIR="/run/user/$(id -u)"
aimem version
aimem health
aimem projects
```

`su - sessiond` above runs from the root SSH shell and needs no `sudo`
package (minimal Debian installations may not include it). The runtime
directory export makes the CLI use the same Unix socket as the systemd
user service: `su` may leave this variable unset, causing the CLI to look
under the state directory instead. Keep these exports in the shell where
you run the remaining hub commands. If you use a
different SSH account, use your configured administrative elevation to
become the service user. SSH keys authorize host login; they are not aimem API tokens.
The local aimem service must be running. Running these commands as root
without switching users can address the wrong state directory or socket.

## 2. Enable Kanban for the project

In the hub-side service-user shell:

```sh
aimem tasks on -p my-project
aimem projects
```

This creates the project's store if absent and enables tasks. There is no
separate `aimem create-project` command. Creation through task enablement
is a temporary workaround; explicit CLI and console project creation are
backlogged, with Kanban enablement kept separate. Double-check the ID
first: a typo can create an unintended project.

For a project already listed on the hub, alternatively open
`https://hub.example.com:8440/admin` with an admin
credential, open the project's menu, and select **tasks (Kanban): switch
on**. Enablement is per project; upgrading the hub does not enable new
projects automatically. Existing task-bearing projects are enabled by the
upgrade migration where no explicit setting exists.

The task-page project picker shows Kanban-enabled projects, including empty
boards. Disabling a board removes it from that picker; a direct task/project
link can still read existing tasks.

## 3. Create a user, grant access, and issue a task token

Still on the hub as the service user:

```sh
aimem access list
aimem access user-add my-agent
```

If the intended user already exists, reuse its ID instead of creating
another. Copy the returned user's `id` into `USER_ID` below:

```sh
aimem access grant add my-project user USER_ID
aimem access token-issue-user USER_ID token-my-agent-laptop EXPIRY_RFC3339
```

Replace `EXPIRY_RFC3339` with a future UTC timestamp such as
`2027-01-01T00:00:00Z`, choosing a lifetime appropriate to your access policy.
This user-scoped token can write to every currently granted project, including
projects granted later; a long lifetime keeps that evolving authority usable
longer. The `token-` label identifies the credential, not a second user.
`USER_ID` is the user's immutable ID; readable-name selectors are backlogged.
The result includes the token ID and its secret, shown **once**. Save the
secret privately and retain the ID for revocation. If the secret is lost,
revoke that token and issue another; `access list` cannot recover it.

A write needs all of these: tasks enabled, an enabled user, a current
user or group grant for the current project instance, and a valid unrevoked
token whose scope permits the write. A token alone does not grant write access.
Adding another project grant works with the same user token; no reissue is
needed. Removing access takes effect on subsequent writes.

For an agent that must write only to this project, issue a project token
instead and configure the checkout-local override in step 4:

```sh
aimem access token-issue USER_ID token-my-project-laptop my-project EXPIRY_RFC3339
```

To grant through a group instead:

```sh
aimem access group-add developers
aimem access member add GROUP_ID USER_ID
aimem access grant add my-project group GROUP_ID
```

Tokens are still issued to users, not groups. Use `-` instead of the
project argument in `token-issue` to issue a read-only ordinary token. Ordinary tokens
can read tasks across ordinary projects; project scope restricts writes,
not task-read visibility. Do not treat projects as private task tenants.

| Credential | Purpose |
|---|---|
| SSH key | Host administration; not an API credential |
| Admin token | Hub administration and project enablement |
| Writer/checkpoint token (`aimem token add`) | Agent journaling, sync and memory access; cannot write tasks |
| User-scoped ordinary token (`token-issue-user`) | Writes follow the user's live grants; normally configured per OS user and hub |
| Project-scoped ordinary token (`token-issue`) | Writes restricted to one project instance plus live grants; use a checkout-local override |
| Read-only ordinary token (`token-issue` with `-`) | Reads only; never task writes |

Create users, grants and ordinary tokens through this CLI or
the admin API. The access-management console is still planned; the
console's legacy token controls do not replace these commands.

## 4. Connect the agent machine

Return to the agent machine. If the hub is not already configured, first
register it with its separate writer/checkpoint credential:

```sh
aimem hub add team https://hub.example.com:8440 "<writer-token>"
```

Merge the binding into the project's existing `.aimem.json`, preserving
its actual identity, groups and other settings:

```json
{"project": "my-project", "hub": "team"}
```

For the user-scoped token, configure the OS user's credential for this hub:

```sh
aimem hub task-token team "<user-scoped-token>"
```

Paste the real token locally, not into a chat or committed script. The
task credential is separate from the writer credential. It is stored
**per local hub name**, not per project: replacing it changes the token
used by projects bound to that name unless they require a local override.
Two projects can use this same user-scoped token concurrently. Enable tasks
and grant the user access to each; there is no token switching between them.

For a project-scoped token, run from the configured project root:

```sh
printf '%s' "$PROJECT_TOKEN" | aimem task-token set
aimem task-token show-source
```

Here `PROJECT_TOKEN` holds the secret issued for this project. In PowerShell,
use `$ProjectToken | aimem task-token set`. `set` reads the secret from stdin
and validates its project scope with the hub. Only a nonsecret
`"task_credential":"local"` marker enters `.aimem.json`; the secret stays
outside the checkout in protected per-user storage. `show-source` prints
safe metadata and should report `project-local` for this checkout and
`user-hub` in one using the user-level credential.

A missing, expired, revoked or mismatched local credential fails closed;
it never falls back to the broader user token. Configure each clone and
worktree separately. Committing the marker makes setup a requirement for
teammates too. To deliberately restore per-hub selection in a checkout:

```sh
aimem task-token clear
```

Clearing does not revoke the token on the hub. A project-scoped token stays
restricted when its user gains access to more projects; user-scoped tokens
follow those grants automatically. See [Task credentials](TASK-CREDENTIALS.md)
for storage, binding and compatibility details. Upgrade every participating
client before relying on overrides: older clients ignore the marker.

Restart Claude Code, Codex or OpenCode so its MCP process refreshes the
credential and available tools. Ensure aimem's project integration is
installed; merely editing `.aimem.json` does not register an MCP server.

## 5. Verify browser and agent access

Open `https://hub.example.com:8440/tasks`, enter the ordinary token, and
select the project. Ask the agent to list tasks for the same project.
The tools include `list_tasks`, `get_task`, `create_task`, `update_task`,
comments/history and epic tools. With an explicitly agreed test task,
verify creation and read-back through the agent before assigning real work.

Enabling tools does not install a scheduler. Your process must say which
READY task to pick, how to check advisory dependencies, who claims it,
and what evidence is required for DONE. Updates replace the full task
content and require the revision just read; agents must preserve fields
they are not changing and re-read after conflicts.

## 6. Select process instructions for agents

Kanban works without a selected process, but agents then receive a notice
that process context is unavailable. To inject your handbook, readiness
and completion checklists, templates and required-skill names, prepare a
reviewed manifest in a Git repository each agent machine can read.

On the hub as the service user:

```sh
aimem process select https://example.com/team/process.git FULL_40_HEX_COMMIT process/manifest.json --ref main -p my-project
```

Replace the commit placeholder with the reviewed immutable commit. The
optional `--ref` must resolve to that commit. To replace an existing
selection, also pass `--expect CURRENT_40_HEX_COMMIT`.

On the agent machine, inside the project:

```sh
aimem process show -p my-project
```

Verify the source commit, complete handbook/checklists and required-skill
availability, then restart the session. Git access is machine-local;
the hub stores only the selection. See the [manifest and cache
contract](ADMIN-MANUAL.md#selecting-a-projects-process-documents).

## Disable, revoke, and troubleshoot

Run these on the hub as the service user when needed:

```sh
aimem tasks off -p my-project
aimem access token-revoke TOKEN_ID
aimem access grant rm my-project user USER_ID
```

These are independent actions: disabling tasks stops every task write
while retaining reads; revocation invalidates that token; removing a
direct grant removes that grant, but a group grant may still allow writes.

| Symptom | Check |
|---|---|
| Task tools absent | Enablement on the bound hub, client version, MCP registration; restart the session |
| Task credential missing | Use `task-token show-source` from the configured root; configure the per-hub user token or run local `task-token set` as selected |
| Required local credential fails | Check project/hub binding, expiry, revocation and grant; run `set` with a valid project token, or deliberately `clear` to restore per-hub selection |
| Token rejected | Correct hub, ordinary token, expiry, revocation and enabled user |
| Reads work, writes fail | Tasks enabled, enabled user, current user/group grant and token scope; a recreated project needs a new grant and, for project-scoped tokens, a new token |
| Process context unavailable | Selected manifest/commit, each machine's Git access, required files and bootstrap budget |
| CLI cannot reach service | Run as the configured service user; export `XDG_RUNTIME_DIR="/run/user/$(id -u)"` and check `aimem health`. If that runtime directory/socket is absent too, inspect `systemctl --user status aimem` |

For deployment and API details, see [the administrator manual](ADMIN-MANUAL.md)
and [the access model](DESIGN-access-control.md). Operators upgrading from
v0.5.0 should follow the [token-model rollout checklist](TOKEN-MODEL-ROLLOUT.md)
before enabling more projects.
