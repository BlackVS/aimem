# Quickstart: a new project on a fresh workstation

This is the path for one new project on a workstation that has never run
aimem, against a hub that already runs. At the end the project journals
to the hub, its agent reads the session handoff at every start, its
design documents are shared through the hub, and the agent reads and
writes the project's task board.

You need:
- the workstation (macOS, Linux or Windows) and the project directory on it;
- SSH access to the hub host as an administrator;
- the hub's URL, for example `https://hub.example.com:8440`.

To stand up a hub first, see [the general quickstart](QUICKSTART.md),
stage 2. The [Kanban quickstart](KANBAN-QUICKSTART.md) explains the task
access model behind step 2 in more depth.

## 1. Decide three names

Write them down before you start. Every later command uses them, and
changing one afterwards means redoing the steps that used it.

| Name | Example | Rules |
|---|---|---|
| Project ID | `my-project` | Letters, digits, `.`, `_` and `-`; not `user` and not starting with `group-`. The hub and every workstation know the project by this ID, so pick it once. |
| Local hub name | `team` | Lowercase letters, digits and `-`. It is the name the workstation registers the hub under, and `.aimem.json` binds the project to it. Every machine that checks out the project registers the hub under this same name, since `.aimem.json` travels with the project. |
| Access user | `my-agent` | The user the hub's task board knows. Reuse an existing user when the same person or agent already has one. |

The examples below use these three values, the hub
`https://hub.example.com:8440`, the installer's default service account
`sessiond`, and `my-mac` as the label of the workstation's credentials.
Substitute your own.

## 2. On the hub: two credentials and the project's board

Log in to the hub host and switch to the service account, so the CLI
talks to the running hub service:

```sh
ssh root@hub.example.com
su - sessiond
export PATH="$HOME/.local/bin:$PATH"
export XDG_RUNTIME_DIR="/run/user/$(id -u)"
aimem health
```

`aimem health` must answer before you continue. The `XDG_RUNTIME_DIR`
export makes the CLI use the same socket as the service; without it the
CLI can look in the wrong place.

**The writer token.** It lets the workstation journal, sync, recall and
share documents. It cannot change hub configuration, and it cannot write
tasks:

```sh
aimem token add my-mac
```

The secret is printed once. Copy it into the workstation's private file
in step 3.2 now, or keep this terminal open until you get there.

**The task board.** This creates the project on the hub if it is new and
switches its tasks on:

```sh
aimem tasks on -p my-project
aimem projects
```

Check that the ID in the list is exactly the one you decided: a typo
creates a second project.

**The user and the grant.** List the users first and skip `user-add` if
`my-agent` already exists:

```sh
aimem access list
aimem access user-add --user-name my-agent
aimem access grant add --project my-project --user-name my-agent
```

**The user task token.** It is the credential the agent's task tools
present. It writes to every project the user is granted, now and later,
so give it an expiry that suits that:

```sh
aimem access token-issue-user --user-name my-agent --label my-mac --expires 2027-01-01T00:00:00Z --output my-agent.token
cat my-agent.token
rm my-agent.token
```

The command writes the secret to a new file only the service account can
read and prints the token ID. Keep the ID; it is what you revoke. Copy
the secret out of `cat` into the workstation's private file in step 3.3,
then remove the hub's copy.

## 3. On the workstation

### 3.1 Install aimem and wire the project

Run the one-liner **inside the project directory**.

**macOS.** The installer needs `jq` (for example `brew install jq`).
macOS has no service manager the installer can use, so start the local
service yourself after the install, and again after each login:

```sh
cd ~/src/my-project
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.3/boot.sh | bash
export PATH="$HOME/.local/bin:$PATH"
nohup aimem serve >/dev/null 2>&1 &
```

Add the `PATH` line to `~/.zprofile` so new shells find `aimem`. Before
a later upgrade, stop that `aimem serve`: the installer refuses to
upgrade while a service it does not manage answers, and leaves starting
the new one to you.

**Linux.** The installer needs `jq` (`sudo apt install jq` on Debian or
Ubuntu) and runs the service as a systemd user unit:

```sh
cd ~/src/my-project
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.3/boot.sh | bash
```

**Windows (PowerShell).** The installer registers a logon task for the
service and adds `aimem` to your user `PATH`. Open a new PowerShell
window after it finishes:

```powershell
cd $HOME\src\my-project
powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aimem/v0.10.3/boot.ps1 | iex"
```

The install puts the hooks and the agent plugins in your user profile,
and wires the project: `docs/SESSION-STATE.md` (the handoff),
`AGENTS.md`, `CLAUDE.md`, the project's hook and MCP registration, and
`.aimem.json`. [INSTALL-CLIENT.md](INSTALL-CLIENT.md) lists every file.
Do not start an agent in the project yet: until step 3.4 pins the project
ID, a session would journal under an ID derived from the directory.

### 3.2 Register the hub with the writer token

Put the writer token from step 2 in a file only you can read, give the
file to `aimem hub add`, and delete it.

macOS and Linux:

```sh
(umask 077; cat > ~/aimem-writer.token)    # paste the token, Enter, then Ctrl-D
aimem hub add team https://hub.example.com:8440 --token-file ~/aimem-writer.token
rm ~/aimem-writer.token
```

Windows, with the token copied to the clipboard:

```powershell
Get-Clipboard | Set-Content -Encoding ascii -NoNewline "$HOME\aimem-writer.token"
aimem hub add team https://hub.example.com:8440 --token-file "$HOME\aimem-writer.token"
Remove-Item "$HOME\aimem-writer.token"
```

`aimem hub add` refuses a file other accounts can read and prints the
command that fixes it. On Windows, a file in your profile folder is
private already; elsewhere, restrict it with
`icacls FILE /inheritance:r /grant:r "${env:USERNAME}:F"`.

If the hub's certificate is self-signed or from a private CA, add
`--ca-file PATH` with a copy of the certificate (a self-signed hub keeps
it at `~/.config/aimem/tls/selfsigned.pem` in the service account's
home), or `--pin sha256-BASE64`. The certificate is public; only the
tokens are secret.

### 3.3 Give the task tools their credential

Do the same with the user task token from step 2:

```sh
(umask 077; cat > ~/aimem-task.token)      # paste the token, Enter, then Ctrl-D
aimem hub task-token team --token-file ~/aimem-task.token
rm ~/aimem-task.token
aimem hub credential team
```

```powershell
Get-Clipboard | Set-Content -Encoding ascii -NoNewline "$HOME\aimem-task.token"
aimem hub task-token team --token-file "$HOME\aimem-task.token"
Remove-Item "$HOME\aimem-task.token"
aimem hub credential team
```

`aimem hub credential` must report `individual credential set, active,
scope user`, with the user and token IDs. It never prints the secret.

### 3.4 Bind the project in `.aimem.json`

Replace the `.aimem.json` the installer created in the project root with
the project's identity, its hub, and the design documents to share:

```json
{
  "project": "my-project",
  "hub": "team",
  "groups": [],
  "docs": ["docs/DESIGN.md", "docs/ARCHITECTURE.md"]
}
```

- `project` pins the ID from step 1, so every clone on every machine
  journals into the same project.
- `hub` binds the project to the hub you registered as `team`.
- `docs` lists files, relative to the project root, that are shared
  through the hub as documents. Each is known on the hub by its file
  name without the extension (`DESIGN`, `ARCHITECTURE`). A listed file
  that does not exist yet is skipped without an error, so check the
  paths. The handoff, `docs/SESSION-STATE.md`, is shared without being
  listed.

On Windows, write the file without a byte order mark, which aimem cannot
read. `Set-Content -Encoding ascii` does that; an editor's "UTF-8 with
BOM" does not.

Commit `.aimem.json` and the other wired files if the project is tracked.

### 3.5 Verify, in this order

From the project directory:

```sh
aimem health
aimem project-id .
aimem session-start
aimem docs push
aimem docs list
```

- `aimem health` answers from the local service. On macOS, if it cannot
  connect, the `aimem serve` from step 3.1 is not running.
- `aimem project-id .` prints `my-project`, the pinned ID.
- `aimem session-start` prints the hook output an agent receives at
  start: JSON that carries `docs/SESSION-STATE.md`.
- `aimem docs push` publishes the bound documents now instead of at the
  next checkpoint, and `aimem docs list` shows them on the hub,
  `SESSION-STATE` and each file you listed in `docs`.

Then start Claude Code, OpenCode or Codex in the project directory, a
new session, since hooks and MCP servers are read at startup. Ask the
agent to create a test task on the aimem board, then to list the
project's tasks. The new task appearing in the list proves the whole
chain: the MCP registration, the task credential, the user's grant and
the board. The hub's task page,
`https://hub.example.com:8440/tasks`, shows the same task.

## The two secrets

| Secret | Made by | Stored on the workstation | Revoke on the hub |
|---|---|---|---|
| Writer token | `aimem token add my-mac` | in `hub.json`, the aimem state directory's owner-only hub file | `aimem token rm my-mac` |
| User task token | `aimem access token-issue-user` | in the same file, as the hub's task credential | `aimem access token-revoke --token-id ID` |

Both are shown once. Neither belongs in the project, in `.aimem.json`,
in a commit, in a chat with an agent, or in shell history; the
`--token-file` route above keeps them out of all of these. Move each one
from the hub to the workstation through a file only you can read, and
delete that file once `aimem` has stored the token.

Each machine gets its own writer token, so revoking one machine leaves
the others working. A lost secret cannot be recovered, only replaced:
revoke it and issue another. The hub's admin token, printed when the hub
was installed, is never used on a workstation.

## If something is not working

| Symptom | Check |
|---|---|
| `aimem health` cannot connect | macOS: start `aimem serve` (step 3.1). Linux: `systemctl --user status aimem`. Windows: the `aimem-serve` scheduled task. |
| `aimem hub add` refuses the token file | The file is readable by other accounts; the error names the command that restricts it. |
| `aimem project-id .` prints a hash-suffixed ID | `.aimem.json` has no `project` field, or is not valid JSON (on Windows, often a byte order mark). |
| `aimem docs list` says no shared documents | Run `aimem docs push`; check that the `docs` paths exist and that `hub` names the hub you added. |
| The agent has no task tools | Start a new session; check `aimem hub credential team` and that tasks are on for the project on the hub. |
| Task reads work but writes fail | The user's grant on `my-project`, and that the token is the user task token, not the writer token. |
