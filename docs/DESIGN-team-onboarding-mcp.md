# Checkout-bound onboarding over local MCP

Status: owner-approved direction, 2026-09-23; increment 1 (shared core)
implemented, increment 2 (MCP tools) pending.

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
malformed, rebound, config. Only missing, malformed and rebound are told to
(re)install the token. Denied (the file exists, this process may not read
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

