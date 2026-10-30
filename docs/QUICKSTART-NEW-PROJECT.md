# Quickstart: a new project on a fresh workstation

This is the path for one new project on a workstation that has never run
aimem, against a hub that already runs. At the end the project journals
to the hub, its agent reads the session handoff at every start, its
design documents are shared through the hub, and the agent works from
the project's task board under a selected process.

You need:
- the workstation (macOS, Linux or Windows) and the project directory on it;
- SSH access to the hub host as an administrator;
- the hub's URL, for example `https://hub.example.com:8440`.

To stand up a hub first, see [the general quickstart](QUICKSTART.md),
stage 2. The [Kanban quickstart](KANBAN-QUICKSTART.md) explains the task
access model behind step 2 in more depth.

Sections 2 and 3 are also written as numbered steps with the expected
output of each, in the scenario card
[01-new-project](scenarios/01-new-project.md). CI runs that card against
every change, so its commands and output lines match this release; use it
to compare what you see.

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

## 2. On the hub: two credentials, then the project in one command

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
in step 3.3 now, or keep this terminal open until you get there.

**The process.** A project's board is not ready for work until it has a
process. Every session then starts with the notice that no process
reference is selected, and an agent can read that as "the board is not
ready" and leave it alone. The process is the handbook, the checklists
that gate READY and DONE, and the task templates the agents follow. The
process lives in a Git repository, which is separate from the hub: the
hub stores the selection and a copy of the files and serves them to
members. aimem's own repository carries a ready set at
`processes/aimem/manifest.json`. It is aimem's own process and requires
the `oh-code-review` skill from step 3.2. Read its
[handbook](https://github.com/BlackVS/aimem/blob/master/processes/aimem/handbook.md)
before you select it; a project with other rules copies the
set into a repository of its own and selects that instead.

**The skills, before the process.** Members install the skills a
process requires from the hub, so the hub must hold them. Import the
skills release on the hub host before you select the process, from the
`aiskills-X.Y.Z.tar.gz` release archive with its `SHA256SUMS` beside it:

```sh
aimem skills import --from-archive aiskills-X.Y.Z.tar.gz
```

The [admin manual](ADMIN-MANUAL.md#importing-the-skills-sets) covers the
import and the instance set for skills of your own. A process that
requires a skill the hub does not hold still selects, but
`aimem process select` and `aimem project show` warn
`the hub lacks required skills of the project's process`, with one line
per missing skill and the import command for it, and
`aimem project list` marks the project `not ready for work`. Every
member's `aimem skills install` refuses those skills until you import
them.

Set the hub's default process once, by a release tag of the repository.
The hub host resolves the tag to the commit it holds now, reads the
files at that commit and checks them; the manifest path defaults to
`processes/aimem/manifest.json`:

```sh
aimem process default set https://github.com/BlackVS/aimem.git TAG
aimem process default show
```

Replace `TAG` with a release tag of aimem, such as `v0.10.14`. Every
new project gets this process, pinned to that commit, from the onboard
command below, so the agents read exactly what you reviewed. A project
that already has a process keeps it.

**The user and the user task token.** The task board knows the agent
by a user. Skip `user-add` when `my-agent` already exists:
`aimem access list --user my-agent` shows it, with its tokens.

```sh
aimem access user-add --user-name my-agent
aimem access token-issue-user --user-name my-agent --label my-mac --expires +90d --output my-agent.token
cat my-agent.token
rm my-agent.token
```

The user task token is the credential the agent's task tools present.
It writes to every project the user is granted, now and later, so give
it an expiry that suits that (`--expires` takes a date such as
`2027-01-01` or a duration such as `+90d`). Like any ordinary token, it
can read the tasks of every project on the hub, not only the granted
ones: grants and project scope limit writes. Do not put anything in a
task that the hub's other users must not see. The command writes the
secret to a new file only the service account can read and prints the
token ID. Keep the ID; it is what you revoke. Copy the secret out of
`cat` into the workstation's private file in step 3.4, then remove the
hub's copy.

**The project, in one command.** This creates the project on the hub if
it is new, switches its tasks on, selects the hub's default process,
and grants `my-agent` the project:

```sh
aimem project onboard my-project --user my-agent
aimem projects
```

It prints one line per step and ends with the project's readiness
checklist, which reads `readiness: ready for work` when the board can
take an agent's writes. Since `my-agent` already holds a user task
token, onboard issues none. For a user without one, it issues a token
for this project only, and you name where its secret goes with
`--output FILE`, as above. Running it again changes nothing and reports
what exists. Check that the ID in `aimem projects` is exactly the one
you decided: a typo creates a second project.

A project that needs another process than the default names it, by a
tag of its repository:

```sh
aimem project onboard my-project --user my-agent --process https://github.com/BlackVS/aimem.git TAG
```

With neither `--process` nor a hub default, onboard leaves the process
step open and the checklist names the `aimem process select` command.
Onboard never replaces a project's process: to move a project to a
newer release later, select its tag with `--expect` and the tag selected
now; changing the default never re-selects an existing project. See
[the administrator manual](ADMIN-MANUAL.md#selecting-a-projects-process-documents).
The workstation checks the token it was given with
`aimem hub credential team` in step 3.4.

**The repository.** Bind the project to the one repository its work
happens in. The host of the URL names the forge credential a member
needs:

```sh
aimem project repo set --project my-project --kind github --url https://github.com/you/my-project.git
aimem project show --project my-project
```

`--kind` is `github`, `gitea` or `gitlab`. `aimem project show` prints
the repository, the process, when the hub lacks a required skill the
warning above, and the readiness checklist.

## 3. On the workstation

### 3.1 Install aimem and wire the project

Run the one-liner **inside the project directory**. When aimem is
already installed on this machine, wire the project with the installed
binary instead, which downloads nothing, and skip to step 3.2:

```sh
aimem wire --project my-project --hub team
```

**macOS.** The installer needs `jq` (for example `brew install jq`).
Run it in a login session on the Mac. It registers the local service and
the periodic sync as launchd user agents, which start at each login:

```sh
cd ~/src/my-project
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.20/boot.sh | bash
export PATH="$HOME/.local/bin:$PATH"
launchctl list | grep io.github.blackvs.aimem
```

The last line shows the serve and sync agents. System Settings >
General > Login Items lists them as two entries both named "aimem",
which is expected: the labels `io.github.blackvs.aimem` (serve) and
`io.github.blackvs.aimem.sync` (sync) in that `launchctl list` output
tell them apart. Add the `PATH` line to
`~/.zprofile` so new shells find `aimem`. To stop the service, and how
the agents work, see
[INSTALL-CLIENT.md, macOS notes](INSTALL-CLIENT.md#6-macos-notes).
Releases up to v0.10.3 registered no agent and had you start
`aimem serve` by hand; stop such a serve before upgrading, since the
installer refuses while it answers.

**Linux.** The installer needs `jq` (`sudo apt install jq` on Debian or
Ubuntu) and runs the service as a systemd user unit:

```sh
cd ~/src/my-project
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.20/boot.sh | bash
```

**Windows.** The installer registers a logon task for the
service and adds `aimem` to your user `PATH`. Open a new PowerShell
window after it finishes. In a PowerShell window (from cmd, type
`powershell` first), go to the project, then download the release's
boot script, check it against the release's `SHA256SUMS` and run it:

```powershell
cd $HOME\src\my-project
& {
  $ErrorActionPreference = 'Stop'
  $d = New-Item -ItemType Directory -Force (Join-Path $env:TEMP 'aimem-boot')
  $f = Join-Path $d 'boot.ps1'; $s = Join-Path $d 'SHA256SUMS'
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.20/boot.ps1 -OutFile $f -UseBasicParsing
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.20/SHA256SUMS -OutFile $s -UseBasicParsing
  $want = ((Select-String -LiteralPath $s -Pattern '\sboot\.ps1$').Line -split '\s+')[0]
  if ((Get-FileHash -LiteralPath $f).Hash -ne $want) { throw 'boot.ps1 does not match SHA256SUMS' }
  Unblock-File -LiteralPath $f
  powershell -NoProfile -ExecutionPolicy RemoteSigned -File $f
}
```

The install puts the hooks and the agent plugins in your user profile,
and wires the project: `docs/SESSION-STATE.md` (the handoff),
`AGENTS.md`, `CLAUDE.md`, the project's hook and MCP registration, and
`.aimem.json`. [INSTALL-CLIENT.md](INSTALL-CLIENT.md) lists every file.
Do not start an agent in the project yet: until step 3.5 pins the project
ID, a session would journal under an ID derived from the directory.

The one-liner is for the first install on a workstation. Every later
release installs with `aimem upgrade`, from any directory
([INSTALL-CLIENT.md, Upgrading](INSTALL-CLIENT.md#upgrading)).

### 3.2 Install the agent skills (optional)

Skip this step for plain journaling. A project that uses the process
from step 2, the review gates or a team process needs the skills the
process names (the review skill `oh-code-review` among them). They come
from the hub. The skills import of step 2
(`aimem skills import --from-archive aiskills-X.Y.Z.tar.gz` on the hub
host, verified against the `SHA256SUMS` beside it; see the
[admin manual](ADMIN-MANUAL.md#importing-the-skills-sets)) is the
hub-side prerequisite of the process selection: until it is done, the
hub warns `the hub lacks required skills of the project's process` and
this step refuses each missing skill with the command that imports it.
Each member installs them in the checkout after step 3.5, once the
checkout is bound to the hub:

```sh
aimem skills install
```

It installs the skills the process requires, at the versions the manifest
pins, into the skills directories of the agent clients found on this
machine (`--tool claude,codex` names them; `--user` installs for your
user instead of the checkout), and records each one's version and source
beside them. `aimem skills list` shows the hub's effective set and what
this machine has. The admin's connected machine may still use the aiskills
installers to obtain the release archive and the prompts; a member needs
neither Git nor internet access for its skills.

### 3.3 Register the hub with the writer token

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

### 3.4 Give the task tools their credential

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
scope user`, with the user's name and ID and the token's label, ID and
expiry. It never prints the secret. If the hub later refuses the token, for
example once it expires, the same command names the token by its label
and gives the step that replaces it.

### 3.5 Bind the project in `.aimem.json`

Set the project's identity and its hub in the `.aimem.json` the
installer created, from the project root:

```sh
aimem wire --project my-project --hub team
```

- `--project` pins the ID from step 1, so every clone on every machine
  journals into the same project.
- `--hub` binds the project to the hub you registered as `team`.
- `aimem wire` sets only the keys it is given and keeps the rest of
  `.aimem.json`. `aimem wire --check` shows what is set.

To share design documents through the hub, add a `docs` list of paths
relative to the project root to `.aimem.json`, for example
`"docs": ["docs/DESIGN.md", "docs/ARCHITECTURE.md"]`. Each is known on
the hub by its file name without the extension (`DESIGN`,
`ARCHITECTURE`). A listed file that does not exist yet is skipped
without an error, so check the paths. The handoff,
`docs/SESSION-STATE.md`, is shared without being listed. On Windows,
save the file as UTF-8, with or without a byte order mark. Do not write
it with `Out-File` or `>` in Windows PowerShell 5.1: they write UTF-16,
which aimem cannot read. It then warns that `.aimem.json` is unreadable
and treats the file as absent.

Commit `.aimem.json` and the other wired files if the project is tracked.

### 3.6 Verify, in this order

This step needs the hub registered and the project bound: do steps 3.3
to 3.5 first. From the project directory:

```sh
aimem health
aimem project-id .
aimem session-start
aimem docs push
aimem docs list
aimem process show
```

- `aimem health` answers from the local service. On macOS, if it cannot
  connect, check the serve agent from step 3.1 with `launchctl list`.
- `aimem project-id .` prints `my-project`, the pinned ID.
- `aimem session-start` prints the hook output an agent receives at
  start: JSON that carries `docs/SESSION-STATE.md`.
- `aimem docs push` publishes the bound documents now instead of at the
  next checkpoint, and `aimem docs list` shows them on the hub,
  `SESSION-STATE` and each file you listed in `docs`.
- `aimem process show` prints what a session receives about the
  process. Its second line starts with `Process set: aimem @` and the
  first 12 characters of the commit you selected, then names the
  manifest, `processes/aimem/manifest.json` (the repository URL is not
  part of what a session sees; `--full` prints it). Then come
  `Required skills: oh-code-review (installed 1.34.0 from the hub (release))`
  (with a version 2 manifest,
  `oh-code-review 1.34.0 (release; installed 1.34.0 from the hub (release))`;
  a copy another installer put there reads `installed, not from the hub`,
  and a missing or outdated skill reads `run aimem skills install`), the
  handbook and the READY and DONE checklists. A notice in their place (no process
  selected, access denied, unavailable) names its cause. The command
  succeeds either way, so read the output.

Then start Claude Code, OpenCode or Codex in the project directory, a
new session, since hooks and MCP servers are read at startup. Ask the
agent to create a test task on the aimem board, then to list the
project's tasks. The new task appearing in the list proves the whole
chain: the MCP registration, the task credential, the user's grant and
the board. The hub's task page,
`https://hub.example.com:8440/tasks`, shows the same task.

## 4. The first day on the board

Steps 1 to 3 give a board that works; this step makes the agent work
from it. This guide gives the order and the first commands. What an
epic stands for in your project, how large a task may be and who moves
it to READY or DONE belong in the process handbook.

### 4.1 Make the board authoritative

Add one line to the project's `AGENTS.md`, which the installer created
in step 3.1, and commit it:

```markdown
The aimem task board is authoritative for task state: a stage plan is an epic and a pull request is a task. Pick up, claim and move tasks as the process handbook says, without waiting for permission to use the board.
```

Without such a line an agent can treat the board as optional and ask
before every task change.

### 4.2 The first epic and the first tasks

Start a new session in the project and ask the agent to create:
- the first epic: the current stage or milestone, with its objective
  and target;
- one or two tasks under it, each a concrete outcome with acceptance
  criteria, filled in from the process's task template.

`aimem process show --template task` prints that template: the title,
the objective, the acceptance criteria, the non-goals and a next action
that records priority and size. Every placeholder is replaced before a
task is created. A task moves to READY when it meets the READY
checklist that `aimem process show` printed. The task page shows the
epic and its tasks.

### 4.3 Where your decisions go

A decision given only in a chat is gone when that session ends. Put it
where a later session finds it:
- A decision about one task goes in a comment on that task, which an
  agent reads with the task: use **add comment** on the task page, or
  ask the agent to add one.
- A decision for the whole project goes into the project's memory: ask
  the agent to remember it as a decision, or run
  `aimem remember -p my-project --kind decision "..."`. An agent finds it
  with `recall_memory`. It reaches a session's start context only when
  `.aimem.json` sets `session_facts`, a token budget for recalled
  facts, which is off by default.

### 4.4 Check that a new session sees it all

Start one more new session in the project; hooks, the MCP server and
`AGENTS.md` are read at startup. Then check:
- `aimem process show` still prints the handbook and the checklists,
  not a notice;
- the session's start context has the same process block, so ask the
  agent which process set and commit it follows;
- the agent lists the project's tasks and names the epic from step 4.2
  without being told where to look.

### 4.5 Saving tokens

Everything an agent reads is English by default: task fields and
comments, pull request reviews, handoffs, messages between sessions and
`AGENTS.md`. The same content in another script costs two to three times
the tokens, and an agent reads it many times per session. The operator's
own chat with an agent may be in any language. Three more habits:
- the handoff stays near 50 lines, and `aimem handoff compact` moves its
  history out ([INSTALL-CLIENT](INSTALL-CLIENT.md#what-wiring-a-project-adds-to-it));
- `session_facts` is a token budget that every session start spends, off
  by default ([4.3](#43-where-your-decisions-go));
- `aimem process show --full` is needed only when the start context says
  the process unit was over its budget and not injected
  ([ADMIN-MANUAL](ADMIN-MANUAL.md#selecting-a-projects-process-documents)).

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
| `aimem health` cannot connect | macOS: `launchctl print gui/$(id -u)/io.github.blackvs.aimem`, and its log in `~/Library/Logs/aimem/serve.log`. Linux: `systemctl --user status aimem`. Windows: the `aimem-serve` scheduled task. |
| `aimem hub add` refuses the token file | The file is readable by other accounts; the error names the command that restricts it. |
| `aimem project-id .` prints a hash-suffixed ID | `.aimem.json` has no `project` field, or aimem warns that it is unreadable: the JSON is not valid, or the file is not UTF-8 (on Windows, often UTF-16 from `Out-File` or `>`). |
| `aimem docs push` or `aimem docs list` fails with `no hub configured (aimem hub add ...)` | The hub was never registered on this workstation, for example after a pause right after the one-liner. Do steps 3.3 to 3.5, then rerun step 3.6. |
| `aimem docs list` says no shared documents | Run `aimem docs push`; check that the `docs` paths exist and that `hub` names the hub you added. |
| The session starts with "no process reference is selected" | Tasks are on and no process is selected: do the process part of step 2, then start a new session. |
| `aimem process show` says access denied or unavailable | Denied: this workstation's task token has no current grant on the project, or expired; check `aimem hub credential` and the grant. Unavailable: the hub is unreachable, or stores no set for the selection (an older hub, or a selection made before the upgrade): re-select the process on the hub host, which imports the files. |
| The agent has no task tools | Start a new session; check `aimem hub credential team` and that tasks are on for the project on the hub. |
| Task reads work but writes fail | The user's grant on `my-project`, and that the token is the user task token, not the writer token. |
