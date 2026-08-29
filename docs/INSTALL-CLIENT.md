# Installing aimem on a workstation

A workstation runs the local service that journals your coding sessions.
It works completely on its own — a hub is optional, and everything below
except section 4 applies whether or not you ever set one up.

## 1. One-liner

Run it **inside a project directory**. The first run installs the
user-level pieces; every run wires the project you are standing in.

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh | bash
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/BlackVS/aimem/master/boot.ps1 | iex
```

Then **restart any running Claude Code or OpenCode session** — hooks and
plugins are read at startup.

### What the user-level install puts on the machine

| Path | What |
|---|---|
| `~/.local/bin/aimem` (`%LOCALAPPDATA%\aimem\bin\aimem.exe`) | the single static binary: CLI, service, hub, MCP server, curator |
| `~/.claude/settings.json` | `Stop`, `StopFailure`, `PreCompact` hooks running `aimem submit-claude` |
| `~/.config/opencode/plugins/aimem.ts` | the OpenCode plugin |
| `~/.config/systemd/user/aimem.service` | the local service (Windows: an `aimem-serve` logon scheduled task) |
| `~/.local/state/aimem/` | journals and memories, one SQLite database per project |

Checkpoint hooks are installed at **user level only**. Never add
`Stop`/`StopFailure`/`PreCompact` to a project's `.claude/settings.json`:
registering them twice journals every turn twice.

### What wiring a project adds to it

| Path | What |
|---|---|
| `docs/SESSION-STATE.md` | the handoff file, re-injected at session start |
| `AGENTS.md` | the handoff protocol (edit its project-context section) |
| `CLAUDE.md` | a one-line stub importing `AGENTS.md` |
| `.claude/settings.json` | a `SessionStart` hook running `aimem session-start` |
| `.mcp.json`, `opencode.json` | MCP registration for recall |
| `.aimem.json` | project identity, knowledge groups, hub binding |

Commit these if the project is tracked. Re-running the installer in
another directory only wires that directory.

## 2. Options

Set these before running the one-liner:

| Variable | Effect |
|---|---|
| `AIMEM_HUB_URL` + `AIMEM_HUB_TOKEN` | register a hub, so checkpoints push in real time |
| `AIMEM_GROUPS=a,b` | pre-declare shared knowledge groups in `.aimem.json` |
| `AIMEM_REINSTALL=1` | refresh the binary and hooks even if aimem is installed |
| `AIMEM_VERSION=vX.Y.Z` | pin a release instead of taking the latest |
| `AIMEM_REPO=owner/name` | install from a fork |

## 3. Verify

```sh
aimem version
aimem health          # service reachable, state root, spool depth
aimem project-id .    # the identity this directory journals under
```

Run a turn in Claude Code or OpenCode, then:

```sh
aimem timeline        # the turn you just did should be the last row
aimem tui             # operator dashboard: projects, groups, AI, hub
```

If nothing appears, see *Troubleshooting* in
[ADMIN-MANUAL.md](ADMIN-MANUAL.md).

## 4. Connect to a hub (optional)

A hub merges journals and curated memory across machines. With one
running (see [INSTALL-HUB.md](INSTALL-HUB.md)):

```sh
aimem hub https://hub.example.com:8440 "<token>"
```

Every checkpoint now pushes there as well as landing locally. If the hub
is unreachable the event spools locally and flushes on the next contact —
capture never depends on the network.

### Several hubs on one machine

Different projects can live on different hubs, so — for example — work
and personal projects never share a server:

```sh
aimem hub add work https://hub.example.com:8440  <token> --sync aimem@hub.example.com --default
aimem hub add home https://hub2.example.com:8440 <token> --sync aimem@hub2.example.com
aimem hub               # list; the default is marked *
aimem hub default home  # change the default
```

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
./install.sh enable-sync          # Linux: systemd timer, every ~10 min
```

On Windows the installer registers an `aimem-sync` scheduled task
automatically (same cadence). Passing an ssh destination
(`enable-sync aimem@hub.example.com`) keeps the legacy ssh transport
for hubs that predate the sync API.

## 5. Manual install and other modes

```sh
./install.sh user                 # user-level only (builds from source)
./install.sh project [dir]        # wire one project
./install.sh bootstrap [dir]      # what the one-liner runs
./install.sh enable-sync <ssh>    # periodic anti-entropy sync timer
./install.sh uninstall-user       # remove everything `user` installed
```

Building from source needs Go 1.25+. The release binaries are static
(`CGO_ENABLED=0`), so a machine that installs from a release needs no
toolchain at all.

## 6. Windows notes

Windows support is best-effort and tested less than Linux.

- The service runs as a logon scheduled task named `aimem-serve`, and
  periodic sync as one named `aimem-sync` — rather than systemd units.
- Windows 10 1803 or newer is required: the service listens on an
  `AF_UNIX` socket.
- The installer parks a replaced `aimem.exe` under a timestamped name,
  because Windows will not overwrite a running executable.

## 7. Uninstall

```sh
./install.sh uninstall-user       # binary, hooks, plugin, service
```

Journals and memories under `~/.local/state/aimem/` are left alone —
delete that directory yourself if you want the data gone.
