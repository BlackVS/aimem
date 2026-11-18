# Developing aimem

## Build and test as CI does

```sh
CGO_ENABLED=0 go build -o aimem ./cmd/aimem
go test ./...
gofmt -l .                       # must print nothing
go tool staticcheck ./...
go tool govulncheck ./...        # on an older Go 1.26 patch: exit 3, standard-library findings only
sh scripts/check-process-stops.sh
sh scripts/check-doc-versions.sh
sh scripts/check-doc-examples.sh
node scripts/test-epic-details.mjs
node scripts/test-features-view.mjs
```

The last two lines run the tasks console's script (`internal/server/tasks.html`)
against fake page and hub objects: the epic details and refresh, and the
features view. It needs Node and no packages; where
Node is absent, skip it locally and CI's lint job is its runner.

The console pages' scripts are checked in `go test` as well
(`internal/server/console_scripts_test.go`):

- `TestConsoleScriptsSyntax` runs `node --check` on the inline script of
  each page (`admin.html`, `tasks.html`, `status.html`). It skips where
  Node is absent; CI's lint job runs it with `AIMEM_TEST_REQUIRE_NODE=1`,
  where it must run.
- `TestConsoleTemplateHoles` reads every `${…}` hole of a template that
  builds HTML. A hole passes when it is a call of one of the page's
  escaping helpers (`pageHelpers`), a number the hub assigns, a literal,
  or a branch, sum or `.map(…).join(…)` whose parts pass the same way.
  A number is a field the hub fills, read as a property (`t.revision`),
  never a bare local. A sum is split first, so text added to a number is
  read. Inside an inline event handler `esc` is no escape, because the
  browser decodes the entity before the handler runs, so a hole there
  passes only as a number or a literal. In an address attribute (`href`,
  `src`, `action`, `formaction`) `esc` does not stop a `javascript:`
  address, so only the page's own address helper (`taskURL`), a number or
  a literal passes. Every other hole is on the reviewed list
  (`console_holes_test.go`), one entry per distinct expression with the
  number of places it occurs and the reason it is safe; an inline or
  address entry also names its ground. When the check fails on a page change, escape
  the new hole with a helper. If it is safe as it is, read it and add or
  update its entry. An entry that matches nothing fails too: remove it.

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
  harness, `node --check` of the console pages' scripts, staticcheck,
  govulncheck) and `build`;
- `test (ubuntu-latest)`: `go vet` and `go test ./...` on Linux;
- `test-windows-pr`: `go vet` and `go test` on Windows for the packages
  whose behaviour depends on the operating system: `internal/wiring`,
  `internal/process`, `internal/processctx`, `internal/childenv`,
  `internal/filelock`, `internal/privatefile`, `internal/taskcred`,
  `internal/upgrade` and `internal/adapter` (paths and 8.3 short names,
  git and its environment, file locks and permissions, hooks, the
  binary swap), and the tests that run boot.ps1 itself from
  `internal/installer` (`go test -run '^TestBootPs1'`). A pull request
  that touches the installer surface (`boot.sh`, `boot.ps1`,
  `install.sh`, `install.ps1`, `install-hub.sh`, the OpenCode plugin,
  `.github/workflows/release.yml` or `internal/installer`) runs the
  whole `internal/installer` package there instead, with master's
  20-minute timeout. Otherwise the rest of the package, about 8 minutes
  on the runner, runs on master;
- `scenario (ubuntu-latest)` and `install-hub-container`.

The slow checks run on every push to master, nightly, on a manual run
of the CI workflow (`workflow_dispatch`, for any branch), and on a
release prepare PR, whose branch is named `release/v<version>`:

- `test (windows-latest)`;
- `scenario (windows-latest)`;
- the identity test's 50 runs under the race detector on Linux.

On an ordinary pull request those legs keep their names and pass after
one step that says where they run, because the master ruleset requires
every check by name. `test-windows-pr` runs only on an ordinary pull
request; on master, nightly and a release/ pull request it is skipped,
since the full Windows leg runs there. A Windows failure outside its
packages (`cmd/aimem` and the rest of the suite, the Windows scenarios)
therefore still surfaces on master.

The macOS tests run only in the release workflow, at the tag (the
client upgrade check below also runs on macOS before it): the `macos` job
runs `go vet`, `go test ./...`, a native build and the launchd install
check (`scripts/check-launchd-install.sh`) on `macos-latest`, and the
job that publishes the release waits for it. GitHub's macOS
runners can go unassigned for long stretches, and a starved job held
merges until someone re-ran it, so a Mac-only break now stops a
release, never a merge. When the job fails on a real Mac problem, the
fix goes through a pull request and the release takes the next
version; when the runner never started, re-run the job.

The client upgrade check runs a real client upgrade from the previous
release to the tree under test. `scripts/upgrade-versions.sh` derives
both versions, so no file moves after a release: the previous release
(FROM) is the newest `vX.Y.Z` tag reachable from the head, the head's own
tag excluded, whose release publishes a `SHA256SUMS`; the tree is built
as TO, the head's own tag at a tag, the version in a `release/vX.Y.Z`
branch's name, or otherwise FROM's next patch. With no previous release,
or a shallow checkout, the run fails and says why. Only a "not found"
for a tag's `SHA256SUMS` (a tag between its push and its publication)
moves the derivation to the tag before it; any other answer, or a request
that fails, is asked once more and then ends the run with the tag and the
answer, so a rate limit or an outage never quietly picks an older
release.

`scripts/check-client-upgrade.sh` (and `.ps1` on Windows) installs FROM
with its published one-liner. FROM's binary then serves a throwaway hub
on the runner, and FROM wires two checkouts to it and joins a team in
one of them (the join only on Linux and macOS, where the team path's
shell runs). The upgrade itself is FROM's own `aimem upgrade --release
TO` against an export of the tree (`AIMEM_BASE`): the tree's installers,
OpenCode plugin and build. So only the first install is a published
installer; the upgrade runs the installers the next release will ship.
It first upgrades with a stand-in binary that never serves, and checks
that the upgrade fails, says ROLLED BACK and leaves FROM serving. Then
the real upgrade: the binary and the plugin must be the tree's, the
Claude Code hooks still wired, and the service healthy at TO. Last, the
tree's MCP server is started in each checkout, as a client's next session
starts it: the joined checkout must list the team work tools, from the
record FROM wrote, and the other the standalone set. It never
downgrades, so it does not depend on a downgrade working: an older binary
refuses a database newer than it understands. The user-level installers
install no skills, so there are none to check.

It runs in CI's `client-upgrade` job on a release prepare pull request
from this repository's `release/` branches (Linux, Windows and macOS),
nightly (Linux and Windows) and on a manual run (all three); never on
another pull request or a push, since it installs into the runner's
account. The release workflow's `macos-client-upgrade` job runs it again
at the tag; the release does not wait for that one, and a failure there
is filed as a task.

The hub upgrade check is the hub's half of the same upgrade.
`scripts/check-hub-upgrade.sh` runs in the pinned Debian container the
hub installer's container check uses, as root and without systemd, in
the installer's install-only mode (`AIMEM_NO_SERVICE=1`). FROM's own
`install-hub.sh`, fetched from its tag as the published one-liner does,
installs FROM's published binary, and the hub starts from the line the
installer prints. That hub writes a project with tasks on, a task with a
comment, a memory, and a team with a member. The hub is stopped, as an
operator without a service manager would, and the tree's
`install-hub.sh` upgrades it from an export of the tree built as TO.
The tree's hub must read back everything FROM wrote. The installer keeps
one state copy (`<state root>.backup-<UTC time>`), which must hold FROM's
databases at FROM's schema and nothing written after the upgrade. Each
project's journal must then be at the tree's schema: when the tree's
schema is newer this is a real migration, and the run prints `migration:
schema N -> M`; when it is the same, it prints `no schema step`. The
service manager's part of an upgrade stays with `install-hub-container`.

It runs in CI's `hub-upgrade` job, where `client-upgrade` runs, on Linux
only (the hub installer is Linux-only). A manual run may name the release
to start from with the workflow's `from` input
(`AIMEM_UPGRADE_FROM`), and only a manual run: a release prepare pull
request and the nightly run always derive it. The run's output says when
the release was named by hand. The input exists to run the migration
branch from a release with an older schema before a release needs it; it
does not make an upgrade that skips a release a supported path.

A change whose risk is on Windows or macOS is tested there before the
push; a Windows change can also get a manual run of the workflow on its
branch.

## Release gate

A release's prepare pull request (the one that rolls the CHANGELOG's
`[Unreleased]` section into the version), on a branch named
`release/v<version>` so CI runs the full set, is marked ready only when
CI is green on its head: no check run at the head may be other than
success or skipped. That includes the `scenario` job on Linux and
Windows, which runs the release-gate cards, among them the new-project
path of the quickstart, so every released quickstart has run against the
release candidate; and the `client-upgrade` job on Linux, Windows and
macOS, and the `hub-upgrade` job on Linux, so the release's own upgrade
path, from the previous release to this tree, has run before the tag for
a client and for a hub. Making these jobs required checks, so
that the merge button enforces this as well, is a repository setting and
the owner's; until it is set, the person who marks the pull request ready
checks the head's runs.

At the release, the worker reads the result of the release run's
`macos-client-upgrade` job and records it on the release task.
`ROLLBACK_LINE_SINCE` in `scripts/client-upgrade.env` names the first
release whose `aimem upgrade` ends a failed run with the rollback line;
the release that first carries the line sets it to its own version if it
differs. Nothing else in that file moves after a release.

While the CHANGELOG's entry on the tool sets states the served-by-name
period of the team tools by position ("in this release and in the release
after it"), the prepare pull request of the release that carries the entry
writes that release's number into the sentence, and the next release's
prepare names itself as the last release of the period. The removal waits
for both tags (its board task's start condition).

## Toolchain

aimem needs Go 1.26 (`go 1.26.0` in `go.mod`). The floor moved from 1.25 for the standard library's `crypto/hpke`, which enrollment delivery (D1) will use.

- **CI and the release build on the runner's Go.** Each job installs Go 1.26 with `actions/setup-go` (`go-version: '1.26'` with `check-latest: true`, so the newest patch and its security fixes), which exports `GOTOOLCHAIN=local`. A step then checks that `go env GOTOOLCHAIN` is `local` and that `go version` names 1.26, so a build never uses a toolchain that `go.mod` downloaded.
- **The lint tools are pinned in `go.mod`.** staticcheck and govulncheck are tool directives, so `go tool staticcheck` and `go tool govulncheck` run the versions `go.sum` records, built on the same Go as everything else. Dependabot's `gomod` updates move them like any other requirement. govulncheck also scans the standard library of the Go that runs it, so on a Go older than the newest 1.26 patch it reports standard-library findings that CI, which installs that patch, does not.
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
