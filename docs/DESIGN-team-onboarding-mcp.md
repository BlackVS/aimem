# Checkout-bound onboarding over local MCP

Status: owner-approved direction, 2026-09-23; increments 1 (shared core)
and 2 (local MCP tools, entry points, quickstart) implemented; live
platform validation tracked by the pilot task.

Team authentication remains unchanged. The agent's `/join_team` and
`/resume_team` entry points should invoke structured local MCP tools instead
of requiring credential access from a shell. This is the common path for
Claude Code, Codex and OpenCode on Windows, Linux and macOS.

## Observed problem

The Windows pilot exposed two independent failures. A Codex sandbox setup
regression prevents command launch in some versions. Separately, a working
sandbox runs shell commands as another Windows user. aimem's project-local
credentials use current-user DPAPI; an owner-installed token is not usable
by that sandbox user. The existing local stdio MCP processes were observed
running as the credential owner. Unix owner-only credential files create a
similar access boundary when a client uses another account or container.

Changing tokens or recommending token installation for every access error
does not solve the execution-context mismatch.

## Decision

Add local stdio MCP `team_setup` and `team_continue`, backed by the same
onboarding logic as `aimem teams setup` and `aimem teams continue`. Extract
that logic once; do not maintain a second state machine or wrap a general
shell command. The CLI remains available for operators.

The MCP server is started by the client under the credential owner's account
and bound to the configured checkout. Tools accept team, role, declared
profile and explicit recovery choices; they do not accept a token, arbitrary
checkout, executable or command string. No hub/remote MCP local-filesystem
onboarding endpoint is added. Provisioning stays an operator operation.

Preserve checkout/project/hub/token binding, saved handles, retry keys,
duplicate suppression, resume generations, role entry and explicit leave.
Continue never silently joins. Reports distinguish missing credentials,
filesystem denial, decryption failure and hub rejection without exposing
secrets or falling back to broader authority.

Before exposing the existing CLI workflow in an owner-context MCP process,
inspect Git/client probes and integration repairs: repository-selected
executables must not become an arbitrary execution path outside the coding
sandbox. Keep the tool constrained to its configured checkout and intended
operations. Missing integration must produce an actionable report.

Existing encryption and file permissions remain. No new daemon, credential
broker, machine-wide encryption, plaintext token copy, shell environment
secret, broad ACL grant or automatic privilege escalation is needed.

## Delivery and validation

1. Shared setup/continue core and accurate credential diagnostics: P1/M,
   task `01a0ce9d-d595-7000-a0f2-0b9ebedaa4f5`.
2. Local MCP tools, generated client commands and quickstart: P1/M,
   task `01a0ce9e-5654-7000-8cf2-1f48f1cc43e3`, after the first merge.
3. Existing pilot validation task `01a0cc7a-b690-7000-a5d5-217843806e00`
   verifies real Windows, Linux and macOS join/repeat/restart/resume/leave.

Automated tests cover refusal paths, wrong checkout, retries, duplicate
suppression, stale handles and CLI/MCP state parity. Real-client results
record the OS, client, MCP process identity, checkout and model provenance.
Unrun platforms remain pending. The owner starts pilot sessions manually;
this design does not authorize joining them during implementation.

## Increment 1: shared core and inspection record

`internal/teamsetup` holds the one onboarding state machine: `Run` (setup)
and `Continue`. `aimem teams setup` and `aimem teams continue` are shells
over it that parse flags and print the report. The host supplies an `Env`:
the checkout, the state root, the client version, the hub session caller
bound to that checkout's credential, the process-context probe and the git
probe. Nothing else crosses the boundary: no token, command string,
executable or other checkout. A probe the host does not supply is reported
as unchecked (`warn`), never substituted or failed. Reports carry `run_as`,
the OS account the run held the credential as, so pilot evidence records
the process identity.

`taskcred.Resolve` failures carry a class: missing, denied, decrypt,
malformed, rebound, config. Only malformed and rebound are told outright to
reinstall the token. A binding failure names the state root and the account
the process runs as, and a missing credential or hub entry leads with the
other-account case: another account's state root is invisible from here
and looks exactly like nothing installed. Denied (the file exists, this process may not read
it) and decrypt (DPAPI refuses the blob: another account's, or bytes it
never wrote) name the account and route to the owner-context path; a
reinstall is mentioned only as the deliberate choice to give this account
its own credential. On Windows the decrypt class cannot tell a foreign
account's blob from corrupt bytes, and the message says so.

What an owner-context host executes or mutates when it runs this core,
inspected before increment 2 exposes it over MCP:

- Git probe (`Env.Git`): `git -C <checkout> rev-parse HEAD` and `git -C
  <checkout> status --porcelain`, `git` resolved from the process PATH
  (Go refuses a PATH result relative to the working directory, so a binary
  dropped into the checkout is never run). `git status` consults the
  checkout's own `.git/config`, where `core.fsmonitor` may name a program;
  that file is writable by whoever edits the checkout, which under the
  sandbox is not the owner. The stdio facade already runs `git remote
  get-url` and `git rev-list` in the checkout for the project identity,
  neither of which consults fsmonitor. Increment 2 therefore either
  leaves `Env.Git` nil (base commit reported as not probed) or runs the
  probe with repository-config programs neutralized; it does not run the
  shell's probe as is.
- Client probe (`wiring`): `exec.LookPath` for `claude`, `codex` and
  `opencode` on the process PATH only; `<client> --version` runs only on
  the explicit `--client-versions` option and only on the resolved
  absolute path. Nothing from the checkout is looked up or run.
- Integration repairs (`wiring.Options.Repair`): add constant, aimem-owned
  entries only (the `aimem session-start` SessionStart hook, the
  `aimem mcp` registrations, the handoff template, the rendered
  `/join_team` and `/resume_team` assets in the checkout and, when
  `~/.codex` exists, under the process user's home). Content never comes
  from the checkout; an entry that differs is left as is; a symlink at a
  target path is replaced by a file, never followed for the write. From an
  owner-context process these writes land as the owner's files in the
  working tree the sandbox agent edits, so increment 2 exposes repair as an
  explicit argument (report-only by default), never as a side effect of a
  join.
- Process context (`Env.Process`): the CLI's session-start bootstrap
  fetches the hub-selected process repository into the state root with
  git. The selection is administered on the hub, not sourced from the
  checkout. The stdio facade does not run it today; increment 2 decides
  whether to supply it or report the process as not checked.
- State: nonsecret session state under the state root, keyed by the
  canonical checkout path and bound to project, hub, URL and token ID; the
  credential is read from the state root only. No new write location.

Tests: the CLI suite runs unchanged against the shared core; the package
suite drives `Run` and `Continue` through an in-memory session caller
(proving the seam), checks that a nil probe is reported as unchecked,
produces the denied class (Unix, file mode 0) and the decrypt class
(Windows, bytes DPAPI never protected) and asserts the fix names the
account and the owner-context path and never opens with a token reinstall.

## Increment 2: local MCP tools, entry points, quickstart

`internal/mcp` gains `team_setup` and `team_continue`, listed and served
only when the facade is bound to a checkout (`localCheckout`: the directory
the client started `aimem mcp` in, and this process's state root). The hub
endpoint constructs no such binding, so it neither lists nor serves them,
for a full principal and for a tasks-only one alike; a call by name is
refused. Arguments are decoded strictly: `team`, `role`, an optional
`profile` (label, platform and platform version required once given;
model rules shared with the CLI through `teamsetup.CheckProfile`),
`resume`, `new_session`, `repair_integration`, `allow_project_stop_hooks`
for setup; `team` and `fence` for continue. An unknown field, a token, a
checkout or a command is a decode error. The result is the report as
JSON, the CLI's `--json` shape; a blocked report is a result with its
checks and fixes, not a tool error.

The host environment the facade supplies resolves the inspection record
of increment 1:

- Git: `teamsetup.GitHeadOnly`, which runs `rev-parse HEAD` and reports
  everything else as not probed (`ErrNotProbed`); the core prints
  "(working tree not probed)" and the entry point tells the agent to check
  `git status` itself. No `git status` runs under the owner's identity.
- Integration repairs: `repair_integration` defaults to false, so a join
  reports the wiring and writes nothing; the installers and `aimem teams
  commands` remain the writers.
- Process context: `processctx.Bootstrap`, the session-start bootstrap
  moved out of the CLI into its own package (the process package sits
  below adapter in the import graph), for the facade's own state root, so
  the process check is the same on both paths.
- State and credential: the facade's state root, the same files.

`team_leave` through the facade clears the saved membership for the bound
checkout (it used the process working directory before). The generated
`/join_team` and `/resume_team` entry points call the tools, stop with an
upgrade-and-restart message when the server does not list them, never fall
back to a shell command or a broader permission, and read a credential the
process cannot open as an account mismatch. The Claude Code skills allow
`mcp__aimem__team_setup`, `mcp__aimem__team_list` and
`mcp__aimem__team_continue`.

Tests (`internal/mcp/onboard_test.go`, over the fake hub now shared in
`internal/teamsetup/teamsetuptest`): listing on the facade; join, repeat
without a second join, saved declaration reuse, refusal of a role or team
change in place, continue, resume of a suspect session with a new
generation, wrong team, argument strictness; the CLI core reading the
session the facade created; explicit leave ending it for both; a lost join
reply replayed with its saved key, continue replaying a pending join and
never a fresh one, a refused handle reported with nothing taken over,
`new_session` as the explicit way out; a second checkout under the same
state root seeing no membership and no credential; the denied (Unix) and
decrypt (Windows) classes refused before any hub call; the hub facade
listing and serving neither tool; tools hidden with tasks off; schema
validity and the absence of token, checkout, command and executable
arguments.

Live validation: automated coverage above; real Windows, Linux and macOS
client runs (OS, client, MCP process identity from `run_as`, checkout,
model provenance) are recorded by the pilot task and remain pending until
run.

