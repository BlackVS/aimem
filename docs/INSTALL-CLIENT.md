# Installing aimem on a workstation

A workstation runs the local service that journals your coding sessions.
It works completely on its own — a hub is optional, and everything below
except section 4 applies whether or not you ever set one up.

## 1. One-liner

Run it **inside a project directory**. The first run installs the
user-level pieces and wires the project you are standing in. Later runs
upgrade aimem from any directory and wire only a directory that already
holds `.aimem.json`; to wire another project, run `aimem wire` inside it
(see "Wiring another project" below). The home directory is never wired: Claude Code reads a
`CLAUDE.md` in every directory above a session, so wiring the home would
reach every project under it.

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.16/boot.sh | bash
```

**Windows**, in a PowerShell window. From cmd, or from the Run dialog
(Win+R) for an upgrade, type `powershell` first and paste the block there
(the Run dialog starts in your home directory, which is never wired):

```powershell
& {
  $ErrorActionPreference = 'Stop'
  $d = New-Item -ItemType Directory -Force (Join-Path $env:TEMP 'aimem-boot')
  $f = Join-Path $d 'boot.ps1'; $s = Join-Path $d 'SHA256SUMS'
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.16/boot.ps1 -OutFile $f -UseBasicParsing
  Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.16/SHA256SUMS -OutFile $s -UseBasicParsing
  $want = ((Select-String -LiteralPath $s -Pattern '\sboot\.ps1$').Line -split '\s+')[0]
  if ((Get-FileHash -LiteralPath $f).Hash -ne $want) { throw 'boot.ps1 does not match SHA256SUMS' }
  Unblock-File -LiteralPath $f
  powershell -NoProfile -ExecutionPolicy RemoteSigned -File $f
}
```

The block downloads `boot.ps1` and `SHA256SUMS` from the release and
refuses a `boot.ps1` whose hash the release does not list; `boot.ps1`
then checks the binary against the same `SHA256SUMS`. It runs `boot.ps1`
as a file in a child PowerShell, which matters on a fresh machine:
Windows clients default to the `Restricted` execution policy, which
refuses every script file. `-ExecutionPolicy RemoteSigned` lifts that
for the child only and changes no machine state; `Unblock-File` comes
after the hash check, because PowerShell 7 marks a downloaded file as
coming from the internet and `RemoteSigned` would refuse it. A Group
Policy-enforced policy (`Get-ExecutionPolicy -List` showing
MachinePolicy/UserPolicy) overrides the flag, and no installer can or
should work around it. There is no one-line cmd form: the check and the
run in one line would be too long to read before running it. Releases up
to 0.10.13 publish no `boot.ps1` asset; they were installed with
`irm …/boot.ps1 | iex`, as their own README shows.

Then **restart any running Claude Code, OpenCode, or Codex session** —
hooks and plugins are read at startup. Codex additionally asks once, in
its own UI, to review-and-trust the new hooks before running them.

If the Windows block stops with `boot.ps1 does not match SHA256SUMS`,
the download was cut short or changed by the CDN or a network filter,
and nothing ran. Retry, or install from a checkout (section 5), which
never touches the network for the script itself.

### What the user-level install puts on the machine

| Path | What |
|---|---|
| `~/.local/bin/aimem` (`%LOCALAPPDATA%\aimem\bin\aimem.exe`) | the single static binary: CLI, service, hub, MCP server, curator |
| `~/.claude/settings.json` | `Stop`, `StopFailure`, `PreCompact` hooks running `aimem submit-claude`, and a `PostToolUseFailure` hook for `Bash` running `aimem failure-recall`: when a command fails, it recalls the project's and the user's memories with the error's distinctive words (redacted; paths, numbers and hashes left out) and gives the agent at most 3 that share enough of them (2 words, or 3 for an error of 8 words or more), at most 1500 bytes. It says nothing when none qualifies, when the local service is not running, or in a team conversation. |
| `~/.config/opencode/plugins/aimem.ts` | the OpenCode plugin (1.18+ and 2.x). It journals turns, injects the handoff on 2.x, and, when a shell call fails, runs `aimem failure-recall` and gives the model at most 3 matching memories, as the Claude Code hook does |
| `~/.codex/hooks.json` | `Stop`, `PreCompact` hooks running `aimem submit-codex` |
| `~/.codex/config.toml` | `[mcp_servers.aimem]` MCP registration (via `codex mcp add` when codex is on PATH) |
| `~/.config/systemd/user/aimem.service` | the local service (macOS: launchd user agents, see section 6; Windows: an `aimem-serve` logon scheduled task) |
| `~/.local/state/aimem/` | journals and memories, one SQLite database per project |

Checkpoint hooks are installed at **user level only**. Never add
`Stop`/`StopFailure`/`PreCompact` to a project's `.claude/settings.json`
or `.codex/hooks.json`: registering them twice journals every turn twice.

### What wiring a project adds to it

| Path | What |
|---|---|
| `docs/SESSION-STATE.md` | the handoff file, re-injected at session start |
| `AGENTS.md` | the handoff protocol (edit its project-context section) |
| `CLAUDE.md` | a one-line stub importing `AGENTS.md` |
| `.claude/settings.json` | a `SessionStart` hook running `aimem session-start`, which injects `docs/SESSION-STATE.md` at the start of a session and after each compaction: at most its first 8 KiB, followed by a one-line note when the file is longer. `docs/SESSION-STATE-ARCHIVE.md`, where history goes, is never injected. `aimem handoff compact` moves the history there: everything below the line `<!-- aimem: history below -->`, or without that line everything past the head that fits 50 lines and 8 KiB, keeping a closing section of up to 10 lines headed "Pick up here", "Pick up", "Exact next action", "Next action", "Next" or "Next steps" (any case). `--summarize` adds a short summary of what moved, written by the curation backend (`aimem curate`'s), and `--dry-run` only reports |
| `.codex/hooks.json` | the same `SessionStart` hook for Codex (runs once the project is trusted in Codex). Codex runs hook commands without a shell, so this one cannot guard on aimem's presence — a collaborator without aimem sees a one-line hook failure per session start, nothing worse |
| `.mcp.json`, `opencode.json` | MCP registration for recall. `opencode.json` also lists `docs/SESSION-STATE.md` in `instructions`. OpenCode 1.x loads that file whole into every session, with no size limit aimem can apply, so the handoff's 50-line rule and `aimem handoff compact`, which moves history to `docs/SESSION-STATE-ARCHIVE.md`, are what keep it small there. OpenCode 2.x ignores `instructions`; the aimem plugin injects the handoff instead, with the same 8 KiB limit and note as `aimem session-start`. |
| `.claude/skills/join_team/SKILL.md`, `.opencode/commands/join_team.md`, `.agents/skills/join-team/SKILL.md` | the `/join_team` team-onboarding entry points, rendered by `aimem teams commands` (managed files; `~/.codex/prompts/join_team.md` too when a Codex home exists) |
| `.claude/commands/aimem-analyze.md`, `.opencode/command/aimem-analyze.md` | the `/aimem-analyze` command, which builds the shared knowledge base subsystem by subsystem (managed files) |
| `.aimem.json` | project identity, knowledge groups, hub binding |

Commit these if the project is tracked. An existing `AGENTS.md`,
`CLAUDE.md` or handoff is kept as it is, and a managed file is refreshed.
Re-running the installer in another directory only wires that directory. `aimem teams setup` checks
this wiring in a checkout before joining a team and adds what is missing
in the same shapes (`--no-repair` to report only); see
[TEAM-AGENT-QUICKSTART.md](TEAM-AGENT-QUICKSTART.md#one-command-onboarding).

### Wiring another project

Once aimem is installed, `aimem wire` wires a project from the installed
binary: run it in the project directory, or name the directory. It writes
everything in the table above, and both installers run it for their
project part. Its flags set those keys in `.aimem.json` and keep every
other key; without them a new `.aimem.json` has no groups and a derived
project id:

```sh
aimem wire --project my-project --hub team --groups shared
aimem wire --check        # report what is wired and what is missing; write nothing
```

`--check` exits non-zero while something is missing. `aimem wire`
refuses the home directory, the aimem state root and a directory inside
a checkout whose `.aimem.json` already governs it. Registering the hub
(`aimem hub add`) and the task credential (`aimem task-token`) stay
separate steps (section 4).

## 2. Options

Set these before running the one-liner:

| Variable | Effect |
|---|---|
| `AIMEM_HUB_URL` + `AIMEM_HUB_TOKEN` | register a hub, so checkpoints push in real time |
| `AIMEM_GROUPS=a,b` | pre-declare shared knowledge groups in `.aimem.json` |
| `AIMEM_USER_ONLY=1` | install or upgrade the user level only and wire no project |
| `AIMEM_REINSTALL=1` | refresh the binary and hooks even if the installed aimem is already current (an older install is upgraded without it) |
| `AIMEM_VERSION=vX.Y.Z` | install another release than the one in the URL |
| `AIMEM_UPGRADE_WAIT=30` | seconds an upgrade waits for health at the new version before rolling back |
| `AIMEM_REPO=owner/name` | install from a fork |
| `AIMEM_NO_SERVICE=1` | register no service (no systemd unit, no launchd agents): run `aimem serve` yourself. `AIMEM_NO_SYSTEMD=1` does the same |

## 3. Verify

```sh
aimem version
aimem health          # service reachable, state root, spool depth
aimem project-id .    # the identity this directory journals under
```

Run a turn in Claude Code or OpenCode, then:

```sh
aimem sessions -p "$(aimem project-id .)"                  # the session you just used
aimem timeline -p "$(aimem project-id .)" -s <session-id>  # its turns; the one you just did is the last
aimem tui             # operator dashboard: projects, groups, AI, hub
```

If nothing appears, see *Troubleshooting* in
[ADMIN-MANUAL.md](ADMIN-MANUAL.md).

## 4. Connect to a hub (optional)

A hub merges journals and curated memory across machines. With one
running (see [INSTALL-HUB.md](INSTALL-HUB.md)), mint this machine its
own **writer token** on the hub host — `aimem token add <machine>`
(secret printed once; covers events, sync, recall, and shared
documents, but not hub administration, and revokes alone) — then:

```sh
aimem hub add home https://hub.example.com:8440 "<writer-token>"
```

Never give a client the hub's env token (`AIMEM_HTTP_TOKEN`): it is the
hub's admin credential, and a machine holding it acts as hub admin.
`hub add` asks the hub once who the token is and warns when it is the
env token or another admin token. It also warns about a URL that is not
https and about `--insecure`, and when the hub cannot be reached to
check. The warnings never stop the entry from being stored.

If the hub's certificate is not in the system roots (a private CA or a
self-signed certificate), record its trust with the entry: add
`--ca-file /path/to/hub-ca.pem` or `--pin sha256-BASE64` (the SHA-256 of
the certificate's public key; the hub installer prints it for its
self-signed certificate). Every aimem path to that hub uses it; a later
`hub add` of the same hub keeps it unless you give a new one. To drop a
pin or CA file once the hub has a certificate the system roots trust,
run `hub add` again with `--system-roots`: it clears the recorded trust
and keeps the entry's task credential and default mark.
`--insecure` skips verification instead; use it only as a last resort.
An entry with `--insecure`, or with a plain `http://` URL, still carries
checkpoints, but aimem refuses to send an individual credential to it:
the MCP task tools, team setup and team mode, and a checkout's
project-local credential need verified https.

To keep the token off the command line and out of shell history, give it
with `--token-file PATH` (a file only you can read) or `--token-file -`
(standard input) instead of the third argument; `aimem hub task-token`
takes the same flag.

Every checkpoint now pushes there as well as landing locally. If the hub
is unreachable the event spools locally and flushes on the next contact —
capture never depends on the network. A per-machine token is
individually revocable and cannot touch hub configuration.

### Several hubs on one machine

Different projects can live on different hubs, so — for example — work
and personal projects never share a server. Mint a token per machine on
**each** hub (they are not interchangeable):

```sh
aimem hub add work https://hub.example.com:8440  <work-writer-token> --default
aimem hub add home https://hub2.example.com:8440 <home-writer-token>
aimem hub               # list; the default is marked *
aimem hub default home  # change the default
```

Task tools (the Kanban work, `create_task` and friends in the MCP facade)
use a separate per-user credential, never the writer token above: ask the
hub admin for an ordinary token issued to your user for your project
(`aimem access` on the hub) and store it with

```sh
aimem hub task-token home <aimem_user_...token>
```

Without it the task tools answer with exactly this instruction; nothing
falls back to the writer token or to the local service.

The task tools appear only for a project whose tasks the hub admin has
switched on (`aimem tasks on -p <project>` on the hub host, or the
console). The facade asks the hub once when the session starts: off hides
the tools; on lists them; a hub that cannot be reached lists them too,
with the hub refusing writes if the project turns out to be off. A change
of the setting takes a session restart to show.

When tasks are on and the hub admin has selected a process reference,
the session-start hook also injects the project's process documents —
the handbook and the READY/DONE checklists — fetched from the named Git
commit with this machine's own Git access and cached per commit. `aimem
process show` prints exactly what a session receives, including the
notice when something is unavailable (no selection, no access to the
repository, the hub unreachable, the unit over its budget). The first
session after a selection pays one bounded fetch; later ones none.

(`--sync <ssh-dest>` on `hub add` is only for hubs old enough to lack
the sync API; current hubs sync over HTTPS with the same token. Hub
NAMES are machine-local, but use the same names on every machine — a
project's `.aimem.json` binding travels with the repo, and a name that
no machine-local hub matches syncs nowhere; `aimem sync` and
`aimem logs` both warn when that happens.)

Bind a project to one of them in its `.aimem.json`:

```json
{"hub": "home", "groups": ["webstack"]}
```

An unbound project goes to the default hub. On a machine that talks to
more than one hub, leaving a project unbound is genuinely ambiguous —
bind them.

### Periodic sync

Real-time push covers the normal case; a periodic anti-entropy pass
reconciles anything missed while a machine was offline — and it is the
leg that PULLS curated knowledge down to this machine. It rides the
hub's HTTPS API with the same token as everything else:

```sh
bash ./install.sh enable-sync          # Linux: systemd timer, every ~10 min
```

On macOS and Windows the installer registers the periodic sync
automatically, at the same cadence: a launchd agent on macOS, an
`aimem-sync` scheduled task on Windows. On macOS, `enable-sync` repoints
that agent, and a later install keeps it. Passing an ssh destination
(`enable-sync aimem@hub.example.com`) keeps the legacy ssh transport
for hubs that predate the sync API.

## 5. Manual install and other modes

`install.sh` is committed without the executable bit (`boot.sh` invokes
it through `bash` for the same reason), so call it via `bash`:

```sh
bash ./install.sh user                 # user-level only (builds from source)
bash ./install.sh project [dir]        # wire one project (runs `aimem wire`)
bash ./install.sh bootstrap [dir]      # what the one-liner runs
bash ./install.sh enable-sync <ssh>    # periodic anti-entropy sync timer
bash ./install.sh uninstall-user       # remove everything `user` installed
```

The Windows installer has the same shape, run from a checkout with the
process-scoped policy bypass the one-liner also uses:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\install.ps1 -Target C:\path\to\project   # user install if needed + wire project
powershell -NoProfile -ExecutionPolicy Bypass -File .\install.ps1 -UserOnly                        # user-level only
powershell -NoProfile -ExecutionPolicy Bypass -File .\install.ps1 -UninstallUser
```

Run this way, `install.ps1` leaves an aimem that is already on `PATH`
alone (only the one-liner knows the release version to compare against)
and just wires the project; `AIMEM_REINSTALL=1` forces a user-level
refresh, which builds from the checkout's source and therefore needs Go.
`install.sh project` never touches the user-level install at all.

Building from source needs Go 1.26+. The release binaries are static
(`CGO_ENABLED=0`), so a machine that installs from a release needs no
toolchain at all.

## 6. macOS notes

The installer needs `jq` (for example `brew install jq`). It registers
two launchd user agents in `~/Library/LaunchAgents`, loaded into your
login session:

| Agent | What it runs |
|---|---|
| `io.github.blackvs.aimem` | `aimem serve`, started at login and restarted if it fails |
| `io.github.blackvs.aimem.sync` | `aimem sync --all-hubs` every 10 minutes |

System Settings > General > Login Items shows them as two entries, both
named "aimem". That is expected: both agents run the same binary, and
macOS shows the executable's name for an agent without an app bundle.
`launchctl list | grep io.github.blackvs.aimem` tells them apart by label.

Their output goes to `~/Library/Logs/aimem/`. An upgrade stops the agents,
swaps the binary and starts them again; uninstall unloads and removes
both.

```sh
launchctl list | grep io.github.blackvs.aimem                  # both agents, with the serve agent's PID
launchctl print gui/$(id -u)/io.github.blackvs.aimem            # state, last exit status
launchctl bootout gui/$(id -u)/io.github.blackvs.aimem          # stop the service until the next login
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.blackvs.aimem.plist   # start it again
```

- Run the one-liner in a login session. Over ssh with nobody logged in
  at the Mac, the installer writes the agents and they load at the next
  login.
- If an `aimem serve` you started by hand is running, as releases up to
  v0.10.3 asked on macOS, stop it first. The installer refuses while it
  answers and stops nothing itself.
- With `AIMEM_NO_SERVICE=1` the installer registers no agent and you run
  `aimem serve` yourself, as before.

## 7. Windows notes

Windows support is best-effort and tested less than Linux.

- The service runs as a logon scheduled task named `aimem-serve`, and
  periodic sync as one named `aimem-sync` — rather than systemd units.
- Windows 10 1803 or newer is required: the service listens on an
  `AF_UNIX` socket.
- The installer parks a replaced `aimem.exe` under a timestamped name,
  because Windows will not overwrite a running executable.

## 8. Uninstall

```sh
bash ./install.sh uninstall-user       # binary, hooks, plugin, service
```

Journals and memories under `~/.local/state/aimem/` are left alone —
delete that directory yourself if you want the data gone.
