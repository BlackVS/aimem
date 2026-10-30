# Developing aimem

## Build and test as CI does

```sh
CGO_ENABLED=0 go build -o aimem ./cmd/aimem
go test ./...
gofmt -l .                       # must print nothing
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
sh scripts/check-process-stops.sh
sh scripts/check-doc-versions.sh
sh scripts/check-doc-examples.sh
node scripts/test-epic-details.mjs
```

The last line runs the tasks console's script (`internal/server/tasks.html`)
against fake page and hub objects. It needs Node and no packages; where
Node is absent, skip it locally and CI's lint job is its runner.

The documented usage scenarios run separately, under a build tag, against
the real binary started as a hub and as a workstation:
`go test -tags scenario -count=1 ./internal/scenario/...`. The suite is
where a usage path is tested as a whole: a documented path that is added
or changed gets a scenario card, or a change to the card that covers it,
in the same pull request. docs/scenarios/README.md is the card
convention and lists the cards.

## Which checks run where

A pull request runs the fast set, so a PR's CI takes about two minutes:

- `lint` (gofmt, the `scripts/check-*.sh` checks, the tasks console
  harness, staticcheck, govulncheck) and `build`;
- `test (ubuntu-latest)`: `go vet` and `go test ./...` on Linux;
- `test-windows-pr`: `go vet` and `go test` on Windows for the packages
  whose behaviour depends on the operating system: `internal/wiring`,
  `internal/process`, `internal/processctx`, `internal/childenv`,
  `internal/filelock`, `internal/privatefile`, `internal/taskcred`,
  `internal/upgrade`, `internal/adapter` and `internal/installer`
  (paths and 8.3 short names, git and its environment, file locks and
  permissions, hooks, the binary swap, the installers under Windows
  PowerShell);
- `scenario (ubuntu-latest)` and `install-hub-container`.

The slow checks run on every push to master, nightly, on a manual run
of the CI workflow (`workflow_dispatch`, for any branch), and on a
release prepare PR, whose branch is named `release/v<version>`:

- `test (windows-latest)` and `test (macos-latest)`, including the
  macOS launchd install check;
- `scenario (windows-latest)`;
- the identity test's 50 runs under the race detector on Linux.

On an ordinary pull request those legs keep their names and pass after
one step that says where they run, because the master ruleset requires
every check by name. `test-windows-pr` runs only on an ordinary pull
request; on master, nightly and a release/ pull request it is skipped,
since the full Windows leg runs there. A Windows failure outside its
packages (`cmd/aimem` and the rest of the suite, the Windows scenarios)
and any macOS failure therefore still surface on master. A change whose
risk is on one of those platforms is tested there before the push, or
given a manual run of the workflow on its branch.

## Release gate

A release's prepare pull request (the one that rolls the CHANGELOG's
`[Unreleased]` section into the version), on a branch named
`release/v<version>` so CI runs the full set, is marked ready only when
CI is green on its head, including the `scenario` job on Linux and
Windows.
That job runs the release-gate cards, among them the new-project path of
the quickstart, so every released quickstart has run against the release
candidate. When the repository marks the job as a required check, the
merge button enforces this as well.

## Toolchain

aimem needs Go 1.26 (`go 1.26.0` in `go.mod`). The floor moved from 1.25 for the standard library's `crypto/hpke`, which enrollment delivery (D1) will use.

- **CI and the release build on the runner's Go.** Each job installs Go 1.26 with `actions/setup-go` (`go-version: '1.26'` with `check-latest: true`, so the newest patch and its security fixes), which exports `GOTOOLCHAIN=local`. A step then checks that `go env GOTOOLCHAIN` is `local` and that `go version` names 1.26, so a build never uses a toolchain that `go.mod` downloaded.
- **Only the `@latest` tools may use another Go.** staticcheck and govulncheck run with an inline `GOTOOLCHAIN=auto`, because a tool's own `go.mod` may need a newer Go. Build, vet and test stay on the pinned 1.26.
- **Raising the floor is its own PR.** Change `go.mod`, every `go-version` in `.github/workflows/`, the toolchain checks, this section, the Go version in `README.md` and `docs/INSTALL-CLIENT.md`, and the CHANGELOG together. Do it only for a feature that needs it, and only to a Go minor release older than seven days (the supply-chain wait). Patch releases within the pinned minor are taken as they appear, because they carry security fixes.
- **A developer machine** needs Go 1.26 or newer. With an older Go, the default `GOTOOLCHAIN=auto` downloads 1.26 for this module, and `GOTOOLCHAIN=local` stops with an error that names the version.

## Version mentions in the documentation

A guide that names a release reads as stating the current minimum. So every
mention of an old release (v0.6 to v0.9) under `docs/` and in `README.md` is
one of three things:

- **The current minimum, with its reason.** Update the mention to the
  release the documented steps need now, and say why: the feature or
  command that arrived in it. A team guide and the Kanban guide can need
  different releases; each states its own once.
- **Marked historical.** A document written against a release, such as a
  design with a source baseline, keeps its versions and carries a line
  starting `**Historical versions.**` within its first ten lines. The line
  says the releases it names are its baseline, not the current minimum.
- **Archived.** A checklist or review of a past release moves to
  `docs/archive/` unchanged, with its links into `docs/` adjusted and the
  inbound links pointed at the new place.

`CHANGELOG.md` and `docs/ROADMAP.md` are records of the past and are not
checked.

**Enforcement.** `scripts/check-doc-versions.sh` runs in CI's lint job. It
refuses a `v0.6.x` to `v0.9.x` mention in `README.md` or a Markdown file
under `docs/`, outside `docs/archive/` and `docs/ROADMAP.md`, unless the
file's first ten lines carry the marker. It prints each offending line.

**Why.** On 2026-10-23 several guides still stated v0.7.x minimums, while
the team flow needed 0.10.7 or later and the hub 0.10.8.

## Examples in the documentation

A reader copies an example as printed, so every name in it must exist in
the current code. `TestDocExamples` (`docs/examples_test.go`) reads the
code spans of the operational documents, the fenced blocks and the inline
code, and checks four kinds of name:

- **Commands.** In an `aimem ...` invocation, the first word must be a case
  of `main()`'s command switch in `cmd/aimem`. A following word that looks
  like a subcommand must appear as a word in a string literal of the
  `cmd/aimem` source: usage text after "aimem ", a case label, a compared
  value or a map key.
- **Flags.** A `--flag` or `-f` in an invocation must be registered on a
  FlagSet in `cmd/aimem` or appear as a flag in its usage text. A fenced
  line that ends in `\` (sh) or a backtick (PowerShell) continues the
  invocation on the next line, and a failure names the physical line.
- **Routes.** A `/v1/...` path must match a path of
  `internal/server/openapi.json`, which the server's parity test pins to
  the route table. A `{param}` or `<placeholder>` stands for a segment, and
  a mention such as `/v1/sync` or `/v1/identity/` passes when it is the
  prefix of a real route.
- **MCP tools.** A name in inline code followed by the word "tool" (or a
  list of such names before "tools"), and a name after the `mcp__aimem__`
  prefix, must be one of `mcp.ToolNames()`, every tool the MCP facades
  serve.

A failure prints file:line, the name and the nearest current name.

**What it skips.**
- Designs, proposals and probe reports (`DESIGN*.md`, `*-PROPOSAL.md`,
  `*-PROBE.md`): they describe proposed names and the corrections made to
  them. `docs/archive/`, `docs/ROADMAP.md` and `CHANGELOG.md` are records of
  the past.
- Routes of other services: the model proxy's `/v1/models`, `/v1/chat/`
  and `/v1/embeddings`, and aicrew's `/v1/crew/`.
- A line ending in `<!-- doc-check: skip -->`, for prose that names
  something on purpose, such as a command that does not exist.

**Its limits.** The source text is the oracle, not the dispatch: a renamed
command is caught only once its old word is gone from the `cmd/aimem`
source, and a flag is checked against the package's flags as a whole, not
those of its own subcommand. Snake-case words in inline code are not taken
as tool names unless the text calls them tools, because most of them are
JSON fields and error codes. Sample outputs, the order of steps and the
meaning of arguments are not checked; that is a reader's pass.

**Enforcement.** `scripts/check-doc-examples.sh` runs the test in CI's lint
job; `go test ./...` runs it too.

## Processes and state in tests

A development machine usually runs an aimem installation of its own: a local service (on Windows the scheduled task `aimem-serve`), its state root, its socket, and one MCP server per open client. A test, a script or a hand-run evidence check shares that machine. It must never reach that installation.

**Rule 1: stop only what you started.** Stop a process through the handle you hold: the `exec.Cmd`, a child handle, or the PID your own code started. Never stop by name. That means no `pkill`, `killall`, `taskkill /IM`, `Stop-Process -Name` or `Get-Process … | Stop-Process`. A name matches the installed service as well as your instance.

**Rule 2: isolate the state root and the socket.** Run a test instance under its own `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, both in a temporary directory. The guards catch what slips through:
- In a test binary, the state root (`adapter.StateRoot`) and the local socket (`server.SocketPath`) must lie in the temporary directory, or the test panics (package `internal/isolation`).
- The packages whose tests reach the socket (`cmd/aimem`, `internal/adapter`, `internal/mcp`, `internal/tui`) clear an inherited `AIMEM_SOCKET`, `AIMEM_STATE_DIR` or `XDG_RUNTIME_DIR` that points elsewhere, in their `TestMain`.
- A test that spawns `aimem` passes `AIMEM_SOCKET=` (or its own temporary socket) to the child, instead of inheriting the developer's.

**Rule 3: keep evidence runs by hand to the same rules.** To show a command against a live service for a PR, start a throwaway `aimem serve` with its own `AIMEM_STATE_DIR`, `AIMEM_SOCKET` and `HOME`, keep its PID, and stop that PID. Never stop "every aimem".

**The installers follow the same rule.**
- `install.ps1`'s restart stops only the `aimem serve` processes of the installation it upgrades. It selects them by executable path, the binary or its parked `aimem.exe.old-*` copies, through `Select-ServeProcess`. `internal/installps` tests that against synthetic processes, including another installation's service under the same OS user.
- `install.sh` restarts its own systemd user unit.

**Enforcement.** `scripts/check-process-stops.sh` runs in CI's lint job. It refuses a stop by name in Go tests, `scripts/`, the workflows, and shell, PowerShell and JavaScript files.

**Why.** On 2026-10-04 a throwaway service started for a PR's CLI evidence was stopped with `taskkill /F /IM aimem.exe`, a hand-run command and not repository code. It also ended the developer's installed service and every MCP server that depended on it, and the operator had to restart them. The same audit then found tests that would have reached a real service: with an inherited `AIMEM_SOCKET`, or with the real `XDG_RUNTIME_DIR` on Linux, the local socket of several test packages resolved to the developer's own. The guards above close that path.
