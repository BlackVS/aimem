# Changelog

Notable changes to aimem. Versions are release tags on the repository;
because releases are cut per-deployment, entries are grouped by the change
that matters rather than one heading per tag. Fixes that could lose or
misplace data are called out under **Data integrity** — read those before
upgrading a fleet.

The format follows [Keep a Changelog](https://keepachangelog.com/1.1.0/);
this project does not yet promise semantic versioning. The on-disk schema
version is tracked separately (`currentSchema` in `internal/store/store.go`,
currently 25); a binary refuses a database newer than it understands.

## [Unreleased]

### Changed

- **A project granted only to a team has its writer.** The readiness
  checklist (`aimem tasks on`, `aimem project show`, `aimem project
  onboard`) no longer reads a project granted only to an enabled team
  profile as missing a token. The team writes with its peer credential,
  not a hub token, so the item reads done and names the team or teams.
  When a user is granted, alone or beside a team, the item still needs
  that user's live token, as before.

### Security

- **Curation's model CLI and the process import's git run without the
  hub's secrets (audit A-22).** A hub folds its env file into its own
  environment, and both children inherited all of it: the admin token
  `AIMEM_HTTP_TOKEN` and the provider keys. The `claude` CLI also ran
  with its default tools, over journal text that any writer or syncing
  peer authored, so a prompt-injected curation run could have read the
  env file and returned the token as a fact.
  - Both children now get a minimal environment: the search path, home,
    temp dirs, locale, proxies, CA bundles and the Windows system
    variables. No `AIMEM_*` variable passes.
  - The CLI also keeps its own credential and configuration
    (`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`,
    `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDE_CONFIG_DIR`,
    `CLAUDE_CODE_GIT_BASH_PATH`).
    It runs with `--tools ""` and `--strict-mcp-config`, so it has no
    built-in tools and no MCP servers, and extraction is a pure
    completion.
  - git also keeps `SSH_AUTH_SOCK` and git's own `GIT_*` settings, so a
    private process source is still reached the way the machine already
    reaches it.

## [0.10.18] — 2026-10-29

`aimem project onboard` does the hub side of a new project in one
command. A refused task credential names itself: its label, scope and
expiry, where it is bound, and the step that replaces it.
`aimem process pin` works for a team member, under the team's grant. The
on-disk schema stays at 25 and the access schema at 9. Includes PR #323,
#325 and #326.

### Upgrade notes

- **A team member's pin read needs the upgraded hub.** In a team
  conversation, `aimem process pin` reads `GET /v1/projects/{p}/process`
  in team mode, which only an upgraded hub serves; an older hub refuses
  it (`team_operation_unsupported`). Upgrade the hub before aicrew's
  coordinator relies on the pin. Outside a team conversation the command
  reads as before.
- **Token labels are recorded against an upgraded hub.** The identity
  answer reports the caller's token label and expiry from this release
  on. `aimem task-token set` and `aimem hub task-token` record them when
  they store a token; against an older hub, `hub task-token` notes that
  the label was not recorded. A credential stored before keeps working,
  and its refusal says the label is unknown. To name it later, store it
  again after the hub upgrade (`aimem hub task-token HUB --token-file
  FILE`, or `aimem task-token set` in the checkout).
- **`aimem task-token show-source` now asks the hub.** It makes one
  identity request (bounded at 5 s) to report the token's `state`; it was
  offline before.
- **Clients and hubs can otherwise be upgraded in either order.**
  `aimem project onboard` composes admin routes an older hub serves.
- **The on-disk schema stays at 25 and the access schema at 9.** A hub
  or a client goes back to 0.10.17 by putting its binary back.

### Added

- **A refused task credential names itself.** When the hub refuses a task
  credential (HTTP 401), the error now says which token it is (label,
  scope and expiry), where it is bound (this checkout's project-local
  override, or the hub's task credential) and the step that replaces it,
  in the MCP task tools, the session-start process context and
  `aimem hub credential`. A token the hub refuses cannot be asked about
  itself, so `aimem task-token set` and `aimem hub task-token` record its
  label, scope and expiry beside the binding when they store it. The
  identity answer (`GET /v1/access/identity`) reports `label` and
  `expires_at` for the caller's own ordinary token. `aimem hub credential`
  also prints the user's name and the token's label and expiry, and
  `aimem task-token show-source` adds the token's label and expiry and a
  live `state`. A credential stored by an older release keeps working; its
  refusal says the label is unknown.
- **`aimem project onboard` does the hub side of a new project in one
  command.** `aimem project onboard PROJECT --user NAME [--process REPO
  TAG] [--output FILE|-]` turns tasks on, selects the process (the one
  named, else the hub's default, else it leaves the step open), grants
  the user the project and, when the user holds no live token that can
  write to it, issues a project-scoped token whose secret goes to
  `--output` as `aimem access token-issue` writes it. It checks every
  input before the first change, never replaces a project's process,
  issues nothing on a second run unless `--new-token`, and ends with
  the readiness checklist. The new-project quickstart's step 2 and the
  scenario card 01 now use it.
- **The readiness checklist counts a project granted to a team
  profile.** The grant line names such a project's enabled team
  profiles (`team NAME`) beside its users and groups.

### Fixed

- **`aimem process pin` works for a team member.** Run in a team
  conversation (`AIMEM_TEAM_SESSION`), it now reads the process
  reference and the stored set under the team's grant, as the member's
  MCP tools do, instead of being refused because the member's user
  holds no project grant of its own. The hub serves
  `GET /v1/projects/{p}/process` in team mode under the team profile's
  grant, without `selected_by`, as it already served the set. The JSON
  and the refusal shape are unchanged, and nothing falls back to the
  checkout's credential.

## [0.10.17] — 2026-10-29

The process reference on the hub is now read under the project grant,
and a selection may not carry a credential in its URL (audit A-4).
`aimem process pin` prints a project's process pin as aicrew's offer
names it. A new project's readiness checklist ends `tasks on` and
`project show` with what is left and the command for each item;
`aimem access list` prints tables, and `aimem access audit` reads the
administration audit. The hub keeps its default process's files, so
`tasks on` applies the default without Git, and `default set` takes an
archive. Pull requests run the fast CI set. The on-disk schema stays at
25 and the access schema at 9. Includes PR #318 to #322.

### Upgrade notes

- **Before upgrading a hub, check that no selection names a URL with a
  credential.** The upgraded hub refuses a selection or default whose
  https URL carries a user name or password, or whose ssh URL carries a
  password, and clients refuse such a stored reference. Re-select it
  with a clean URL and give Git on the hub host its access through a
  credential helper or an ssh agent (docs/ADMIN-MANUAL.md).
- **A token without a grant on a project no longer reads its process
  reference.** After the hub upgrade, `GET /v1/projects/{p}/process`
  answers 403 to a user token with no current grant on the project, and
  `?history=1` is an admin read. A member's checkout reads its process as
  before through its grant; one without a grant gets the denial and a
  message that says what to check.
- **A default set before this release is applied from the repository
  as before.** Its files are not kept on the hub; `aimem process default
  show` says so, and setting the default again keeps them.
- **Clients and hubs can otherwise be upgraded in either order.**
  `aimem process pin` and the `access list` tables read routes an older
  hub already serves. `aimem access audit` needs an upgraded hub (the
  route is new), as does keeping the default's files; an upgraded
  client's `tasks on` against an older hub reads the default's set from
  the repository as before.
- **The on-disk schema stays at 25 and the access schema at 9.** A hub
  or a client goes back to 0.10.16 by putting its binary back.

### Added

- **`aimem process pin` prints the project's current process pin.**
  aicrew's coordinator fills an offer's process pin and instruction
  digest from it. `aimem process pin [-p PROJECT] [--json]` runs with the
  checkout's own credential, which needs the project grant, and prints
  the repository, the full commit, the manifest, the set's digest, and
  `instruction_digest`: `sha256:` and the SHA-256 of the manifest file's
  bytes at that commit. It reads the hub's stored set, checks it against
  the selection's digest, needs no Git, and caches nothing. A project
  with no selection, a selection from before stored sets, a set that
  does not match, or a refused credential is an error.

- **A project's readiness checklist, `access list` as a table, and
  `access audit`.** The first steps on a new project no longer stop at
  `tasks on` without saying what is left.
  - `aimem tasks on -p ID` and `aimem project show -p ID` end with the
    project's readiness checklist: tasks on, the process selected, a user
    or group granted (named), and a live token that can write (label and
    expiry). Each open item carries the command that closes it, with the
    project filled in, and the granted user's name where there is one.
    The output before the checklist is unchanged.
  - `aimem access list` prints tables instead of the hub's JSON: per
    project, who is granted and which live tokens can write to it, then
    the users with their groups and tokens. `--project` and `--user`
    narrow it. Disabled users and revoked or expired tokens appear only
    with `--all`. `--json` prints the document exactly as before, for
    scripts.
  - `aimem access audit [--since TIME|DATE|Nd] [--actor A] [--action X]
    [--limit N] [--json]` reads the administration audit, newest first.
    It is served by a new admin route, `GET /v1/access/audit`, bounded to
    1000 events; the access snapshot still carries the newest 100.
  - `--expires` on `access token-issue` and `token-issue-user` also takes
    a date (`2027-10-01`, midnight UTC) and a number of days from now
    (`+90d`). The issued token shows the resolved time. RFC 3339 keeps
    working.

- **The hub keeps its default process's set, and `default set` takes an
  archive.** `aimem process default set` sends the set's files with the
  default, and the hub keeps them in `process-default-set.json` in its
  state root, so `aimem tasks on` applies the default without Git or the
  forge, still checked against the default's digest. `default set <repo>
  <tag> --from-archive FILE [--sums FILE]` reads nothing over the
  network: the set comes from the archive its `SHA256SUMS` vouches for,
  and the commit from the archive itself. `aimem process export` now
  records the commit in the archive, as `git archive` and a forge's
  source tarball do. `default show` says whether the files are kept, and
  `default clear` removes them.

### Changed

- **CI on a pull request runs the fast set; the slow checks run on
  master.** A PR runs lint, build, the Linux tests and scenarios and the
  installer container, in about two minutes. The Windows and macOS tests,
  the Windows scenarios and the race runs, which made a PR's run about
  six minutes, run on every push to master, nightly, on a manual run, and
  on a release prepare PR (branch `release/v<version>`).
  docs/DEVELOPMENT.md names the split. AGENTS.md gains the PR economy
  rules: one task, one PR, one squash commit; gates by size and surface;
  the architect session merges an ordinary PR and the operator a
  sensitive or release PR; the worker takes the next task right after
  the merge.

### Security

- **The process reference needs a grant on the project, and its URL may
  not carry a credential (audit A-4).** `GET /v1/projects/{p}/process`
  checked only that the project exists, so any user token read the
  repository URL, who selected it and, with `?history=1`, every earlier
  selection and the kept sets. A selection also accepted
  `https://user:token@host/...`, which would have published the token to
  every token holder.
  - The route now applies the set route's rule: a user token with a
    current grant reads the current selection without `selected_by`; a
    token without one gets 403, and an unknown project reads the same.
  - `?history=1` is an admin read. Admin credentials and the hub host's
    socket (the `aimem process` commands) are unchanged.
  - A selection or default whose https URL carries a user name or
    password, or whose ssh URL carries a password, is refused with 400.
    Clients refuse such a stored reference too. Give Git on the hub host
    its access through a credential helper or an ssh agent
    (docs/ADMIN-MANUAL.md).

## [0.10.16] — 2026-10-28

A hub can hold a default process that `aimem tasks on` applies to a
project with none. aicrew can read one task's text through a new peer
operation, `task.read`, with its own credential. `install-hub.sh` writes
`AIMEM_INTROSPECTION_TOKEN_FILE` into the hub's env file. A revoked
project grant now also reaches a process selection without a digest. On
the tasks page, a long epic title no longer squeezes the task title in
the list view. The access schema moves to 9 (one-way); the on-disk
schema stays at 25. Includes PR #284 and #311 to #316.

### Upgrade notes

- **Upgrade the hub before aicrew's sixth credential is issued.**
  `task.read` exists only on an upgraded hub. After the hub upgrade,
  rerun `aimem identity peer provision` with the same flags; it issues
  only the new `aimem-task-read.token` and keeps the other five.
- **Access schema 9 is one-way.** The hub rebuilds its peer credential
  table to accept `task.read` and keeps every credential. A hub on
  schema 9 does not start under 0.10.15 or earlier; to roll back,
  restore the state backup the installer took before the upgrade.
- **`process select` takes a bare commit only with `--commit`.** A
  selection names a tag of the process repository. A script that selects
  by a full 40-character commit fails with a message naming `--commit`
  until the flag is added. The `tasks on` hint now offers processes in
  use by their tags only.
- **Clients and hubs can otherwise be upgraded in either order.** An
  upgraded client's `aimem tasks on` against an older hub finds no
  default process and behaves as before; `aimem process default` itself
  needs an upgraded hub.
- **The on-disk schema stays at 25.** Only the access schema changes, so
  going back to 0.10.15 on a hub that reached access schema 9 needs the
  installer's state backup (see above); a client goes back by putting
  the 0.10.15 binary back.
- **`AIMEM_INTROSPECTION_TOKEN_FILE` is a new optional installer
  setting.** Without it, the install and upgrade are unchanged.

### Added

- **A hub default process that `tasks on` applies.** On the hub host,
  `aimem process default set <repo> <tag> [<manifest>]` resolves the tag
  and checks the set as `process select` does, and stores the default
  in `process-default.json` in the state root. `show` prints it and
  `clear` removes it.
  - `aimem tasks on -p ID` for a project with no process now selects the
    default's commit with its tag, and prints
    `applied the hub's default process: <set> (tag, commit)`.
  - A project with a process keeps it, and `--no-process` skips the
    step. With no default, the output is unchanged.
  - Changing the default never re-selects an existing project, and the
    admin console's tasks switch does not apply it.

- **`install-hub.sh` writes `AIMEM_INTROSPECTION_TOKEN_FILE` into the env
  file.** A hub for an identity peer needs no hand edit after the install:
  pass the setting (an absolute path) to the installer, on a fresh install
  or on an upgrade.
  - A line the env file already holds is kept and never duplicated; a
    different value is reported, not rewritten.
  - An invalid path is refused before anything changes.
  - Unset, the env file is as before.

- **`task.read`: aicrew reads one task's text.** A new peer operation,
  `GET /v1/identity/peers/{service_id}/task-reads/{project}/{task_id}`,
  answers a task's title, objective, acceptance criteria, non-goals,
  state and revision, for aicrewd to freeze on an attempt when it offers
  the task. Nothing else of the task is returned.
  - Its scope is `board.read`'s: the projects granted to the peer's
    enabled team profiles. An ungranted, reserved or unknown project and
    an unknown task all get the same `task_not_found` (404); the audit
    keeps the reason.
  - It needs its own credential (`--operation task.read`). After the
    upgrade, rerun `aimem identity peer provision` with the same flags;
    it issues only `aimem-task-read.token`.

### Changed

- **modernc.org/sqlite v1.60.1 to v1.61.0** (SQLite 3.54.1, libc
  v1.79.0); on-disk schema unchanged.

### Security

- **A revoked project grant now also reaches a process selection without
  a digest (audit A-5, follow-up).** A selection that carries no digest
  comes from an older hub, from before stored sets, or from a pinned
  version accepted before records carried the digest. A checkout that
  presents the hub entry's own credential kept serving such a selection
  as ready from this machine's Git cache after its grant on the project
  was revoked. The set route is not asked for such a selection, because
  an older hub refuses a route it does not know with 403.
  - The hub's identity answer, which every load already reads, is now
    the grant check. Its `task_write` applies the same grant rule as the
    set route. When it is `false`, the selection is `denied`, the cache
    and Git are not read, and the selection is not recorded as last
    observed.
  - When the field is absent (an admin token, or a hub before project
    grants), when it is `true`, or when the hub is unreachable, the load
    behaves as before. A project-local credential was already refused on
    the same field before any read.

### Fixed

- **Tasks page: a long epic title no longer squeezes the task title in the
  list view.** The row's meta never wraps, and it printed the epic's full
  title, so an epic title of about 120 characters left the task title a
  column one or two words wide (or no width at all). The list row's epic
  label is now capped at about 28 characters with an ellipsis, and the full
  title shows on hover. The board cards and rows without an epic are
  unchanged.

## [0.10.15] — 2026-10-28

`install-hub.sh` now installs a hub without systemd (`AIMEM_NO_SERVICE=1`),
for a container or a host with its own supervisor. It can also install a
verified binary from a local directory (`AIMEM_PREBUILT_DIR`). Task and
comment ids may be given by a unique prefix. `aimem task-token set` reads
the project token from `--token-file`. A token passed where a token file
belongs is refused without being repeated. The documented usage scenarios
run as release-gate cards against the real binary. The Windows install
block checks `boot.ps1` against the release's `SHA256SUMS` before it runs
it. No schema change. Includes PR #292 to #309.

### Upgrade notes

- **Hubs and clients can be upgraded in either order.** A short task or
  comment id works only against an upgraded hub: until the hub is
  upgraded, use the full id.
- **No schema change.** The schema stays at 25, so going back is putting
  the 0.10.14 binary back; no state copy is needed for that.
- **The hub installer has two new optional settings.**
  - `AIMEM_PREBUILT_DIR` installs from a directory instead of downloading.
  - `AIMEM_NO_SERVICE=1` installs without systemd and prints the command
    that starts the hub. Its upgrade refuses while that hub answers:
    stop the hub, run the installer, then start the hub again.
  - Without either setting, the install and upgrade are unchanged.
- **A token-shaped value of `--token-file` or `--admin-token-file` is
  refused when it names no file.** A token's shape is an `aimem_user_` or
  `aimem_peer_` credential, or a word of 32 or more token characters. A
  script that names its token file by such a bare word needs a path with
  a directory or an extension, such as `./<name>`.

### Added

- **install-hub.sh installs a hub without systemd (`AIMEM_NO_SERVICE=1`).**
  For a container or a host with its own supervisor, the installer writes
  the binary, the env file, the certificate and the curation script as
  usual, but enables no linger, writes no systemd unit, starts nothing and
  calls neither `systemctl` nor `loginctl`. Its last line is the command
  that starts the hub. An upgrade in this mode refuses while a hub answers
  on the configured address, since it cannot stop or start one. A CI job
  runs the installer this way in a Debian container: install, start with
  the printed command, refusal, upgrade. See ADMIN-MANUAL, "Installing
  without systemd".

- **The new-project quickstart runs end to end as two scenario cards.**
  Card `01-new-project` follows QUICKSTART-NEW-PROJECT, sections 2 and 3,
  with a user task token. On the hub it switches tasks on, imports the
  skills, selects the process by tag, binds the repository, adds the
  user, grants the project and issues the token. On the workstation it
  wires the project, adds the hub, stores the task credential and runs
  the checks of step 3.6. It then creates and lists a task through the
  hub's API with that token, and shows that a second project needs only
  a grant. Card `02-new-project-project-token` takes the project-local
  path of TASK-CREDENTIALS.md: `token-issue --project`, `task-token set
  --token-file`, a task created with that token, and the token refused
  for another project the user is granted. Both are release-gate cards:
  DEVELOPMENT.md's new "Release gate" section makes the green `scenario`
  job a condition for marking a release's prepare pull request ready.
  The card runner gains an `http` step, a request to the hub's API with
  the bearer read from a token file, and workstation commands now run in
  the project's directory. docs/scenarios/README.md documents both, with
  the curl form of an `http` step for a person following a card by hand.
- **`aimem task-token set --token-file FILE` reads the project token
  from a file.** The project-local task credential could only be piped
  in on standard input. It can now come from a file only its owner can
  read, such as the one `aimem access token-issue --output` writes. The
  file is read and refused exactly as `aimem hub task-token --token-file`
  does: a file other accounts can read is refused with the command that
  restricts it, and no error quotes the content. Standard input stays the
  default without the flag, and `--token-file -` is refused because it
  would add nothing. See TASK-CREDENTIALS.md, "Project-local override".
- **The documented usage scenarios run against the real binary.** A
  scenario card in `docs/scenarios` is a usage path written as numbered
  steps, each with its command, the reason for it and the expected output
  lines. A person can follow it by hand, and the new suite in
  `internal/scenario` (build tag `scenario`) runs it. The suite starts the
  built binary as a hub, with its HTTPS listener on loopback and a test
  CA's certificate, and as a separate workstation, each with its own state
  root and home. It runs the card's steps on the side each names and
  fails on the first expected line the output lacks, naming the card
  line. The runner takes each command as an argument list, with no shell.
  The first card checks the fixture itself: both services answer, a skills
  release is imported, a process set is selected by tag from a local Git
  repository standing in for the public URL, and the workstation adds the
  hub and reads its skills over TLS. A new `scenario` CI job runs the
  suite on Linux and Windows. docs/scenarios/README.md is the card
  convention.
- **The hub installer installs from a local directory.**
  `AIMEM_PREBUILT_DIR=<dir> install-hub.sh` installs the binary from a
  directory holding `aimem-linux-<arch>` and `SHA256SUMS` under the
  release's names, instead of downloading it, and checks it against the
  sums exactly as a download is checked: a missing binary, sums file or
  line, or a wrong sum stops the installer before anything is installed.
  The version installed is the binary's own, and the rest of the install
  or upgrade is unchanged. It lets a closed network, or a container hub
  that installs a build that has not been released, use the real
  installer with a verified binary. `AIMEM_PREBUILT` (a file installed
  as given) is unchanged; setting both is refused. See ADMIN-MANUAL,
  "Installing from a local directory".
- **The new-project quickstart has a "Saving tokens" note.** Step 4.5
  of `docs/QUICKSTART-NEW-PROJECT.md` lists the habits that keep an
  agent's token use down, the language rule first: everything an agent
  reads is English by default, because the same content in another
  script costs two to three times the tokens and is read many times per
  session, while the operator's own chat may be in any language. The
  other items point at the sections that explain them: the handoff's
  50-line rule and `aimem handoff compact`, the `session_facts` budget,
  and `aimem process show --full` for a process unit over its budget.
  The Kanban quickstart and the README link to it.

- **Task and comment ids may be given by a unique prefix.** The task
  tools, the reservation tools and the HTTP task routes accept the short
  form people write, the first two groups such as `01a17718-4ca9` (at
  least 13 characters, lowercase as written), wherever they take a task or
  comment id. Before, every tool wanted the full 36-character id and
  answered "task or comment not found" for the short form. The hub
  resolves a prefix across the hub, the scope a full id has; the
  reservation routes resolve it inside the route's project. A comment
  prefix resolves among the task's own comments. The full id replaces the
  prefix before anything reads it, so retries, revision checks,
  coordination proofs and receipts see only full ids, and a retry by
  prefix and a retry by full id are one request. A prefix that starts two
  tasks is refused with 409 and the matching ids in the projects the
  caller may read (a team session is told how many more there are
  elsewhere); a shorter one is refused with 400 and the rule.

### Changed

- **The tasks console refreshes the epics too, and refreshes itself when
  its tab comes back.** The ↻ refresh button reloads the epic list and the
  open epic panel before the list or the board, so a renamed or new epic
  shows without F5. A selected epic that still exists stays selected; one
  that is gone falls back to "all epics" with a note. When the tab becomes
  visible again, or the window regains focus, at least 30 seconds after
  the last load, the list or the board and the epics reload once. A task
  page never reloads by itself, since a form there may be open, and
  nothing runs while the tab is hidden. CI's lint job now runs the
  console's harness, `node scripts/test-epic-details.mjs`.

- **A required skill the hub lacks is reported with the command that
  imports it.** A hub with no skills imported reported a ready project,
  `aimem process show` called a skill another installer had put on the
  machine installed, and `aimem skills install` refused with no fix. Now
  `aimem skills install` names, beside each missing skill, the hub-host
  command that imports it: `aimem skills import --from-archive
  aiskills-<version>.tar.gz` for a skill of the skills release, the
  instance set for any other, with the administrator manual's section.
  `aimem process select` and `aimem project show` warn `the hub lacks
  required skills of the project's process` with the same lines, and
  `aimem project list` marks such a project not ready for work. The
  bootstrap and the session-start notice say where an installed skill
  came from: `installed 1.34.0 from the hub (release)` or `installed,
  not from the hub`. QUICKSTART-NEW-PROJECT step 2 now imports the
  skills before the process selection and binds the project's
  repository.
- **The documented Windows install no longer pipes a download into
  `iex` or bypasses the execution policy.** README, ADMIN-MANUAL,
  INSTALL-CLIENT and both quickstarts now show one PowerShell block. It
  downloads the release's `boot.ps1` asset and `SHA256SUMS`, refuses a
  `boot.ps1` whose hash the release does not list, and runs it as a file
  in a child PowerShell under `RemoteSigned` for that process only. The
  old `irm | iex` chain drew a Windows Defender behavioral warning on an
  upgrade. From cmd or the Run dialog, start `powershell` first: a
  one-line cmd form of the check and the run would be too long to read.
  The knob form sets `AIMEM_USER_ONLY` just before the run and removes it
  in a `finally`. `boot.ps1` itself is unchanged apart from its header
  comment, and `irm | iex` still works. Releases up to 0.10.13 publish
  no `boot.ps1` asset. A scenario test runs every documented block
  against a stub release. It checks that the block runs from the
  caller's directory, keeps the knob out of the window, refuses a
  tampered `boot.ps1`, and needs its `Unblock-File` for a file marked as
  downloaded.

### Fixed

- **A token passed to `--token-file` or `--admin-token-file` is no longer
  repeated in the error.** `hub add`, `hub task-token` and
  `task-token set` take `--token-file`, and `identity`,
  `identity enroll` and `reservation recover` take `--admin-token-file`.
  Both flags take the path of a file that holds the token. When the token
  itself was given instead, the error that the file cannot be read quoted
  it, and so did any log that captured the error. Now, a value that names no file and has a token's shape is
  refused with a fixed message that does not repeat it. A token's shape
  is an `aimem_user_` or `aimem_peer_` credential, or a word of 32 or
  more token characters such as a writer token. A missing path still
  gets the message that names it.

- **`aimem wire` refuses an empty `.aimem.json`.** An empty or
  whitespace-only file (a byte order mark alone counts as empty) was
  taken as an empty config and reported as present, or rewritten with
  the keys the run named, while a session cannot read it and runs with
  no project pin, hub binding or groups. `aimem wire` and
  `aimem wire --check` now report it as a failure, write nothing to it,
  and say to write `{}` into it or remove it. A `{}` file is still
  accepted.
- **A token piped from Windows PowerShell 5.1 is accepted.** Under a
  UTF-8 console, PowerShell 5.1 prepends a byte order mark to what it
  pipes into a native command, and `Out-File` writes one at the start of
  a file, so `$token | aimem task-token set` and the documented
  `Get-Content -Raw FILE | aimem task-token set` refused a valid token.
  `aimem task-token set`, `aimem hub add` and `aimem hub task-token` with
  `--token-file` (a file or `-`), `--admin-token-file` of the identity
  commands, `aimem enroll redeem`'s record on standard input, and the
  hub's introspection credential file now drop one leading UTF-8 byte
  order mark. A second one, or one inside the token, is still refused.
- **The fix a Windows token-file refusal prints runs in PowerShell.** The
  refusal of a token file other accounts can read printed
  `icacls ... /grant:r "%USERNAME%:F"`, which only cmd expands; in
  PowerShell it failed with "No mapping between account names and
  security IDs was done". It now names the account itself, as
  `DOMAIN\user`, so the same line runs in both shells, and icacls's
  `*SID` form when the name cannot be resolved.

### Docs

- **Two commands in the docs corrected.** The admin manual said that a
  process unit over the manifest's budget is read with
  `aimem process show`, which prints only the bootstrap; it now names
  `aimem process show --full`, as the notice does, and `aimem process`
  usage says that `--full` prints the whole unit. The pilot hub runbook's
  `icacls` line used `%USERNAME%`, which only cmd expands; it now gives
  the PowerShell form `${env:USERNAME}`, with the cmd form beside it.

## [0.10.14] — 2026-10-26

The hub now refuses task text that is not readable prose (HTTP 422,
`unreadable_text`), with an optional model second stage that stays off
until the operator sets it. `aimem wire` wires a project from the
installed binary, and the installers use it. A revoked project grant now
reaches a member's cached process set (audit A-5). Provider base URLs
must be public https endpoints, and a provider's stored token no longer
follows a new base URL. A write during a hub upgrade's state backup no
longer fails the upgrade. The release attaches `boot.ps1` and lists it in
`SHA256SUMS`. No schema change. Includes PR #271 to #290.

### Upgrade notes

- **Hubs and clients can be upgraded in either order.** The text check
  and the provider rules are hub-side. The client's conditional read of
  the process set (`If-None-Match`) is answered by 0.10.13 hubs as well.
- **No schema change.** The schema stays at 25, so going back is putting
  the 0.10.13 binary back; no state copy is needed for that.
- **The hub refuses shorthand task text.** From this release a task
  write with compressed shorthand is refused with HTTP 422 and has to be
  rewritten. After the upgrade, `aimem tasks text-check -p
  <project>` lists the stored text the filter would refuse, without
  changing anything. If the filter refuses readable text,
  `AIMEM_TEXT_CHECK=off` in the hub's env file turns the check off until
  a fixed release is installed.
- **A hub with a local or LAN model server needs one setting to change
  it.** A provider save, the model list and the test button now refuse a
  plain `http://` URL and a private, loopback or link-local address.
  Registries already on disk keep working for curation. To re-save such
  a provider or use its model list or test button, set
  `AIMEM_PROVIDER_ALLOW_PRIVATE=1` in the hub's env file and restart.
- **The hub is down for its state backup during the upgrade.** The
  output prints the size of the state and the copy's time, about a
  second for 128 MiB on a local disk. Do not run `aimem` commands as the
  hub's user during the upgrade: a copy that keeps changing is refused
  after three tries, and nothing is changed.
- **A cached process set needs the hub's set route at session start.**
  A member with a cached set now asks the hub once per load. When that
  read fails, the set is last observed instead of ready: the agent still
  reads its rules, and the set does not authorize a task write.
- **New settings, all optional:** `AIMEM_TEXT_CHECK` (the check's off
  switch), `AIMEM_TEXT_CHECK_MODEL` and `AIMEM_TEXT_CHECK_DAILY_CALLS`
  (the second stage, off until a model is set), and
  `AIMEM_PROVIDER_ALLOW_PRIVATE`.
- **The hand-written `.aimem.json` step is gone.** Run `aimem wire` in a
  new project directory instead.

### Added

- **The hub refuses task text that is not readable prose.** A task
  comment, and the title, objective, acceptance criteria, non-goals,
  next action and blocker on task create, update and triage and on a
  team coordinator's edit of a managed task (`team_edit`), are checked
  against the writing rule (docs/WRITING-PERSISTED-TEXT.md). A filter
  cuts out code, paths, ids, hashes, versions and URLs, then flags words
  glued to numbers, text with almost no spaces, and sentences run
  together. A flagged write answers HTTP 422 with code `unreadable_text`,
  the field, the flagged span, the rule's section and a one-sentence fix,
  and the MCP task tools pass the refusal through unchanged. An update,
  triage or team edit checks only the fields it changes, so text stored earlier never
  blocks an unrelated change. An optional second stage asks a small model
  about flagged text only, and it can only relax the filter. It has its
  own model key, `AIMEM_TEXT_CHECK_MODEL`, off until the operator sets
  it, resolved through the provider registry, and its own daily call cap, `AIMEM_TEXT_CHECK_DAILY_CALLS`
  (default 200). Both are set in the console beside the curation and
  embedding models, and `aimem health` shows the stage under
  `text_check`. `aimem tasks text-check -p <project> [--since <date>]`
  reports the stored text the filter would refuse, without changing
  anything. ADMIN-MANUAL section 3e covers the setting, free, local and
  paid model options, and what any external model means for the
  project's data. Saving a provider whose endpoint is outside this
  machine now returns a one-line pointer to that section, shown in the
  console. **Upgrade note:** from this release, an agent that writes
  compressed shorthand into a task comment or field gets a refusal and
  has to rewrite the text; run `aimem tasks text-check` first to see how
  much stored text the filter would flag. If the filter refuses readable
  text, `AIMEM_TEXT_CHECK=off` in the hub's env file turns the whole check
  off until a fixed release is installed. The second stage stays off
  after the upgrade, so no task text goes to a model provider until
  `AIMEM_TEXT_CHECK_MODEL` is set.
- **`aimem hub add --system-roots` drops a recorded pin or CA file.** A
  re-add of the same hub keeps its recorded `--pin` or `--ca-file`, so
  once a real certificate replaced the self-signed one, the only way to
  drop the pin was `aimem hub rm` and a fresh `hub add`. That also lost
  the entry's task credential and could move the default hub. The new
  flag clears the recorded trust, so the entry verifies against the
  system roots, and keeps the task credential and the default mark. It
  is refused together with `--pin`, `--ca-file` or `--insecure`. The hub
  installer's hint, INSTALL-HUB, INSTALL-CLIENT and QUICKSTART now name
  it in place of `hub rm`.
- **`aimem wire` wires a project from the installed binary.**
  `aimem wire [DIR] [--project ID] [--hub NAME] [--groups a,b]` writes
  what the installers' project part wrote:
  - the handoff template;
  - the SessionStart hooks for Claude Code and Codex;
  - the MCP registrations and the OpenCode instructions;
  - the `/join_team`, `/resume_team` and `/aimem-analyze` commands;
  - `AGENTS.md`, `CLAUDE.md` and `.aimem.json`.

  A file with the user's content is kept and a managed file is
  refreshed. The flags set only those keys in `.aimem.json` and keep
  every other key, written as UTF-8 without a BOM. `--check` reports
  what is wired and what is missing without writing, and exits non-zero
  while something is missing. It refuses the home directory, the state
  root and a directory inside a checkout whose `.aimem.json` already
  governs it. `install.sh project` and `install.ps1 -Target` now run it
  for their project part, so there is one implementation. As a result,
  Windows projects also get `/aimem-analyze`, which only `install.sh`
  wrote before. The Claude Code SessionStart hook keeps each
  installer's form: guarded on Linux and macOS, so a collaborator
  without aimem sees no hook error, and the bare `aimem session-start`
  on Windows. Files `aimem wire` writes keep `&`, `<` and `>` as
  themselves rather than JSON escapes. **Upgrade note:** the hand-written
  `.aimem.json` step is gone. On a machine where aimem is installed, run
  `aimem wire` in the new project directory. The one-liner's skip
  message, README, INSTALL-CLIENT, QUICKSTART-NEW-PROJECT (3.1 and 3.5)
  and TEAM-QUICKSTART now say so.

### Changed

- **The README names aimem's four ways of working.** A new section,
  "Four ways to run it", sets them out as steps that each add a layer:
  local only, with a hub, a hub with tasks, and a hub in a team. Each
  row says what you get, what it needs, the command that turns it on
  and the document to read. Curation is shown as optional at every step.
  Each feature section says which step it belongs to, and the
  documentation table is grouped the same way, with the task and team
  guides now listed in it.
- **Stale hold notices removed.** KANBAN-QUICKSTART opened with a
  2026-09-21 notice that onboarding was on hold for v0.6.0; it now states
  what the guide covers and the releases its steps need (hub 0.10.8,
  agent machines 0.6.0), with the 0.10.13 client rule for task
  credentials. TASK-CREDENTIALS' matching
  note now says when the feature arrived. The README's curation note names
  curation and embeddings as separate layers: semantic recall needs an
  embed model, not a curation run.

- **The reference process set lives in the aimem repository.** The
  handbook, checklists and task templates that aimem projects select are
  now at `processes/aimem/` in this repository, copied byte for byte from
  the ai-skills repository at commit 89bf57d2, the set the hubs serve
  today. The default manifest path, `processes/aimem/manifest.json`,
  therefore names this repository's own layout, and
  `aimem process select https://github.com/BlackVS/aimem.git <release tag> -p <project>`
  selects it. KANBAN-QUICKSTART, ADMIN-MANUAL, QUICKSTART-NEW-PROJECT,
  PILOT-HUB-RUNBOOK and the README say that the process repository is
  separate from the hub, and keep `example.com` only for your own
  repository. On a release build, `aimem tasks on` and `aimem project show`
  print this command at the build's own tag when the hub uses no process
  yet. Existing selections are unchanged: a hub serves its stored copy, and
  moving a project to this repository is a re-selection with `--expect`.
- **The documentation's examples are checked against the code.**
  `scripts/check-doc-examples.sh`, a new step in CI's lint job, runs
  `TestDocExamples`. It reads the fenced blocks and inline code of the
  operational documents and refuses an `aimem` command word or flag, a
  `/v1/` route or an MCP tool name that the code does not have: the
  `cmd/aimem` source, `internal/server/openapi.json` and the new
  `mcp.ToolNames()` are its sources. It prints file:line and the nearest
  current name. Designs, proposals, probe reports, the archive, the
  roadmap and other services' routes are skipped, and a line can opt out
  with `<!-- doc-check: skip -->`. DEVELOPMENT.md documents the rule and
  its limits. On master the operational documents had no stale name; the
  one hit, KANBAN-QUICKSTART's sentence that there is no
  `aimem create-project`, now carries the opt-out. DESIGN-structured-docs
  names the records route as it was built,
  `GET /v1/projects/{p}/collections/{c}/records`.
- **The quickstarts' examples run as printed.** A manual pass ran every
  example of the six guides (QUICKSTART, KANBAN-QUICKSTART,
  TEAM-QUICKSTART, QUICKSTART-NEW-PROJECT, INSTALL-CLIENT, INSTALL-HUB)
  in guide order, against the current build on a throwaway hub. The
  forms that did not run are corrected:
  - `aimem timeline` needs `-p` and `-s`, so QUICKSTART, INSTALL-CLIENT
    and the README now list the sessions with `aimem sessions -p` and
    read one with `aimem timeline -p ... -s`;
  - `aimem search` and `aimem recall` take the query with `-q`, and
    the recall on the hub names its project with `-p`;
  - KANBAN-QUICKSTART's table names `--read-only` for the read-only
    token;
  - TEAM-QUICKSTART quotes `show-source`'s JSON as it is printed.
- **Version mentions in the documentation are current, marked or
  archived.** TEAM-QUICKSTART stated v0.7.1 for the hub and v0.7.3 for
  agent machines. It now states the hub 0.10.8 or later (selecting the
  process by tag, importing the skills archive) and agent machines 0.10.7
  or later (installing the required skills from the hub), and the 0.10.13
  rule that a task credential goes only to a hub entry verified over
  https. PILOT-HUB-RUNBOOK's socket sentence states what every supported
  release does. DESIGN-AIFORGE, DESIGN-access-control and
  DESIGN-portable-team-context carry a `**Historical versions.**` line:
  the releases they name are their baseline, not the current minimum. The
  past release checklists TOKEN-MODEL-ROLLOUT and TEAM-ROLLOUT-0.7.3 and
  the review TEAM-MULTIPROJECT-REVIEW move unchanged to `docs/archive/`,
  with a README there and the inbound links updated.
  `scripts/check-doc-versions.sh`, a new step in CI's lint job, refuses an
  unmarked v0.6 to v0.9 mention outside CHANGELOG, ROADMAP and the
  archive; DEVELOPMENT.md documents the rule.
- **The reference handbook takes the two later ai-skills changes.** The
  ai-skills commits b029cd3 and 4be41c1 are ported into
  `processes/aimem/handbook.md`. The external reviewer is the one deployed on
  the project's own forge, and a `review-this:<profile>` variant is used only
  where the site offers one. Where no external reviewer is deployed, the local
  high review is the pre-merge gate, and the external gate is reported as
  absent, never as passed. Watching for the review and checking every gate at
  the head before marking a PR ready are stated as steps. The forge-gates
  skill's `forge_gates.py wait-review` and `gates` are named as an optional
  way to do them, since that skill ships with ai-skills, not with aimem. The
  handbook's opening lines record the port and that choice. The manifest is
  unchanged, and forge-gates is not a required skill.
- **The release publishes boot.ps1, and boot.ps1 no longer bypasses the
  execution policy.** Each release attaches `boot.ps1` and lists it in
  `SHA256SUMS`, so a Windows install can check the script against its
  release before running it as a file; the documented form that does
  this follows once a release carries the asset. boot.ps1 now runs
  `install.ps1` with `-ExecutionPolicy RemoteSigned` instead of `Bypass`.
  The installer it extracts from the release archive carries no Mark of
  the Web, because Windows' `tar` does not copy the mark from the archive
  to the files it extracts, so the install runs as before on a machine
  with the default Restricted policy. A policy set by Group Policy still
  wins over both values.

### Security

- **A revoked project grant now reaches a cached process set (audit A-5).**
  A member that had cached the project's process set kept serving it as
  ready after its grant on the project was revoked, because a cache hit
  returned before any grant-checked call. Each load with a cache hit now
  asks the hub's set route conditionally (`If-None-Match` with the set's
  digest), and the hub checks the grant before it answers. A refusal
  (401 or 403) is `denied`, and the cached set is not used; that is the fix
  for the finding. A 304 serves the cache as ready, as before.
  This changes the offline behaviour. A session start with a cache hit
  now contacts the hub's set route once. When that read fails (it cannot
  reach the hub, the hub no longer keeps the set, or it answers with an
  error), the cached set is last observed instead of ready: the agent
  still reads its rules, and the set does not authorize a task write. A
  hub that is unreachable altogether already gave last observed.

- **A provider's stored token no longer follows a new base URL.** In the
  console's provider registry, saving a provider with a blank token kept
  the stored token even when the base URL changed. The model list and
  the test button then sent that token to the new URL, so an admin
  bearer could collect any provider key. Such a save is now refused
  (HTTP 409, "send the token again"); a save to the same endpoint still
  keeps the stored token.
- **Provider base URLs must be public https endpoints.** A save is
  refused for a plain `http://` URL and for a host that is, or resolves
  to, a loopback, private, link-local or multicast address, or one in
  0.0.0.0/8.
  The model list and the test button also refuse such an address when
  they connect, so a name re-pointed after the save gains nothing. A hub
  that uses a local or LAN model server (Ollama, LM Studio, a LAN
  gateway), or reaches providers through a proxy on a private address,
  sets `AIMEM_PROVIDER_ALLOW_PRIVATE=1` in its environment file and
  restarts. Only the host operator can set this; the admin API cannot.
  Registries already on disk are not changed, and curation keeps using
  them as before; re-saving such a provider, or using its model list or
  test button, needs the setting. The `AIMEM_OPENAI_*` pair in the
  environment is the operator's own and is not restricted.
- **Provider changes are logged with the old URL.** The "admin provider
  set" log line now carries `old_base_url` and whether the token was new,
  replaced, kept or absent. A refused save logs its reason, and no line
  carries a token value.

### Fixed

- **A write during a hub upgrade's state backup no longer fails the
  upgrade.** The installer already stopped the service before copying
  the state root, but another process of the service's user (an `aimem`
  command, a curation run started by hand) could still write it, and
  tar's "file changed as we read it" ended the upgrade with "could not
  copy the state root". install-hub.sh and install.sh now take the copy
  again from scratch, up to three times, and never keep a copy that
  changed while it was read, because a database copied mid-write can be
  torn. After the third try the upgrade stops with tar's message and a
  hint to find the writer with `lsof +D`, starts the service again and
  changes nothing. The upgrade also stops before
  the copy when the service still answers after its stop, and it prints
  the size of the state it copies and how long the copy took, which is
  the main part of the downtime (about a second for 128 MiB on a local
  disk).

- **A successful answer that mentions Cloudflare is no longer read as a
  block.** Curation's completions and the embeddings client classified
  every response with the block-page check, a 200 included, and that
  check matches "cloudflare", "attention required" or "<html" anywhere
  in the body. A completion about a project's Cloudflare proxy was
  therefore penalised as an upstream block and retried until the batch
  failed, and the call spacing widened for every later call. The check
  now reads a response's body only when the status is not 2xx or the
  body is not the expected JSON. A 429, a 5xx, an HTML page where JSON
  belongs and a proxy error carrying a block page are still blocks.
- **A Windows one-liner knob no longer outlives its run.** The documented
  forms set `AIMEM_USER_ONLY=1` in the caller's own window (`$env:` in
  PowerShell, `set` in cmd), so every later one-liner in that window ran
  user-only and wired nothing. ADMIN-MANUAL now sets the knob inside the
  child PowerShell's `-Command`, which scopes it to the run, and says why
  a script block cannot. boot.ps1 itself, run as `irm | iex` in an open
  window, left `AIMEM_TARGET_VERSION` and `AIMEM_PREBUILT` set in that
  window; it now puts both back as they were when it ends, a value the
  user had set included, even when the run fails.

## [0.10.13] — 2026-10-23

Individual credentials now go only to a verified https hub. Every request
that carries one, from the MCP task tools, process context with a
project-local credential, team setup and the task-credential check, uses
one strict client. That client refuses a plain `http://` entry and an
`--insecure` one before any request is sent. This is the breaking change
0.10.12 announced. `aimem hub task-token` now warns when it stores a
credential that will not be sent, and `aimem hub credential` reports such
an entry as `untrusted`. No schema change. Includes PR #268 and #269.

### Upgrade notes

- **Move every hub entry that holds an individual credential to verified
  https before upgrading a client.** Run `aimem hub list` on each client.
  An entry whose URL starts with `http://`, or that shows
  `[insecure: self-signed]`, is refused for task credentials after this
  upgrade. That covers an entry with an `aimem hub task-token`, and one
  that a checkout's project-local credential (`aimem task-token set`) is
  bound to. Re-add it with its pin or CA: `aimem hub add NAME
  https://HOST:PORT TOKEN --pin sha256-BASE64` (the hub installer prints
  the pin) or `--ca-file PATH`. A re-add over the same URL with `--pin`
  or `--ca-file` drops `--insecure` on its own, so no `hub rm` is needed
  here; `hub rm` first is only for dropping a recorded pin or CA, as the
  0.10.12 note on a replaced certificate says. An entry moved from
  `http://` to `https://` is a new URL: run `aimem hub task-token` and
  `aimem task-token set` for it again.
- **Client-side only.** The change is in the client. No hub route,
  hub setting or wire format changes, so hubs and clients can be
  upgraded in either order.
- **No schema change.** The schema stays at 25, so going back is putting
  the 0.10.12 binary back; no state copy is needed for that.
- **One new `--json` value.** `aimem hub credential --json` can now
  report `state: "untrusted"`; a script that matches every state needs
  that case.
- **No setting changes.** No new environment setting.

### Security

- **Individual credentials go only to a verified https hub.** Every
  request that carries an individual credential now uses one strict
  client. It accepts an https URL only, verified against the entry's
  `--ca-file`, its `--pin` or the system roots. It refuses an entry
  configured with `--insecure`, even when a pin is also recorded, and a
  plain `http://` entry, loopback included. It uses no proxy from the
  environment and follows no redirect. The refusal is made before any
  request is sent. For an http or `--insecure` entry it names
  `aimem hub list` and the `hub add` that fixes it; an entry whose
  `--ca-file` or `--pin` cannot be loaded is refused with that error. The
  paths it covers:
  - the MCP task tools (the user-scoped `aimem hub task-token` and a
    checkout's project-local credential alike);
  - process context with a project-local credential;
  - team setup's identity and status reads;
  - the task-credential check behind `aimem task-token set`.

  Team mode, `aimem hub credential` and enrollment already
  worked this way; team mode now shares the same client and wording. One
  path is deliberately left as it was: process context for a checkout
  without a project-local credential reads with the hub's shared
  checkpoint (writer) token, not an individual credential, and keeps the
  entry's own trust, as checkpoint pushes do. This is part 3 of 3 of
  audit finding A-3.
- **`aimem hub task-token` warns when the credential will not be sent.**
  Storing a task credential on an entry the strict client refuses (an
  `http://` URL, `--insecure`, or a `--ca-file` or `--pin` that cannot be
  loaded) still succeeds, but now prints `WARNING: hub NAME: the task
  credential is stored, but aimem will not send it to this entry`,
  followed by the reason and the fix. The check is local: nothing is sent
  to the hub.
- **`aimem hub credential` reports such an entry as `untrusted`.** It
  used to show it as `unreachable`. The new state, also in `--json`, says
  the credential was not sent and how to re-add the entry; `unreachable`
  now means only that the hub did not answer or answered wrongly, and
  `refused` still means the hub rejected the credential. Scripts that
  read `aimem hub credential --json` see one new `state` value.

## [0.10.12] — 2026-10-22

Clients no longer get the hub's admin token by default. The hub installer
stops printing the env token for clients. Instead it prints `aimem token
add <machine>`, which gives each machine its own writer token, and a
`--pin` for its self-signed certificate. `aimem hub add` now warns about
an admin token, a URL that is not https and `--insecure`. A team
conversation's task list carries the board's counts, and a NUL byte in a
search query is refused as an invalid query. No schema change. Includes
PR #263, #264, #265 and #266.

### Upgrade notes

- **Hubs first, then workstations.** A 0.10.11 hub refuses the task-counts
  read in a team conversation, so a newer client's team `list_tasks` page
  comes back without `counts` until the hub is upgraded. Upgrade the hub
  with its one-liner before the clients.
- **No schema change.** The schema stays at 25, so going back is putting
  the 0.10.11 binary back; no state copy is needed for that.
- **The env token belongs on no client.** `AIMEM_HTTP_TOKEN` in the hub's
  env file is the hub's admin credential. A client set up from an older
  installer's output holds it and acts as hub admin. `aimem hub add` on
  0.10.12 says so when it is given that token. To move such a client to
  its own writer token, and then to replace the env token, follow
  ADMIN-MANUAL "Clients that hold the env token".
- **The installer prints `--pin`.** On a hub with its self-signed
  certificate, every run of the hub one-liner, an upgrade included,
  prints the certificate's pin. A client added with
  `--pin sha256-...` verifies the hub instead of skipping verification
  with `--insecure`.
- **A replaced certificate needs `hub rm`, then `hub add`.** A later `hub
  add` keeps a recorded pin, so once a real certificate replaces the
  self-signed one, run `aimem hub rm <name>` on each pinned client, then
  `hub add` without `--pin`, and `aimem hub task-token` again if the entry
  had a task credential.
- **The next release refuses what this one warns about.** In the release
  after 0.10.12, the individual-credential paths (the MCP task tools,
  process context, team setup and the task-credential check) will refuse
  an entry that is not https or that skips verification with
  `--insecure`. `aimem hub list` shows each entry's URL and its
  `[insecure: self-signed]`, `ca-file:` or `pin:` trust; move every
  entry to https with `--pin` or `--ca-file` before that release.
- **No setting changes.** No new environment setting, and no route
  changes beyond the team-mode counts read.

### Added

- **Task counts in a team conversation.** `GET /v1/projects/{p}/tasks/counts`
  is now a team-mode route: a verified team session reads it under the
  linked team access profile's live grant, with the same refusals as the
  task list. A team conversation's first `list_tasks` page now carries
  `counts`, as an ordinary one's has since 0.10.11. Upgrade the hub
  first: an older hub refuses the counts read in team mode, the page then
  comes back without `counts`, and the next tool verifies the context
  again.

### Security

- **The hub installer no longer hands clients its admin token.** A fresh
  `install-hub.sh` printed the env token (`AIMEM_HTTP_TOKEN`, an implicit
  admin) as the credential to configure clients with, and added
  `--insecure` for its self-signed certificate. It now prints two steps
  instead: `aimem token add <machine>` on the hub, which gives that
  machine its own writer token, then `aimem hub add` with that token. For
  a self-signed certificate it adds `--pin sha256-...`, the certificate's
  key pin, computed on every run, so the connection is verified.
  `--insecure` is mentioned only as a warned last resort. The env token
  is never printed, on a fresh install or a re-run. A hub with no TLS
  gets a warning that bearer tokens cross the network in clear.
  INSTALL-HUB, INSTALL-CLIENT, QUICKSTART and ADMIN-MANUAL say the env
  token belongs on no client, and ADMIN-MANUAL has the steps for moving a
  client that holds it to its own writer token. This is part 1 of 3 of
  audit finding A-3.
- **`aimem hub add` warns about a risky hub entry.** After storing the
  entry, it asks the hub once who the token is (`GET
  /v1/access/identity`, through the entry's own trust, following no
  redirect). It warns when the token authenticates as role admin. When
  the token is the hub's env token, the warning says so and names
  `aimem token add <machine>` as the way to give the machine its own
  writer token. It also warns when the URL is not https, and when
  `--insecure` is given. A hub that cannot be reached, refuses the
  token or answers something else gets a warning that the role could
  not be checked. These are warnings only: the entry is stored as
  before, and `hub add` still succeeds. This is part 2 of 3 of audit
  finding A-3.

### Fixed

- **A NUL byte in a search query is refused as an invalid query.** A
  journal search or memory recall whose terms held a NUL reached SQLite,
  which ends a string there, and the 400 carried SQLite's own "SQL logic
  error: unterminated string". The journal search, memory recall and task
  search now run one shared check before the query reaches SQLite and
  answer 400 "invalid query: search terms must not contain NUL". Queries
  without a NUL are unchanged.

## [0.10.11] — 2026-10-21

Tasks are easier to find and to read at a glance. The `/tasks` list sorts
newest first and refreshes in place, a task can be found by words in its
content or its comments, and the state chips show how many tasks are in
each state. Agents get the same over MCP: `search_tasks`, the newest
comments first, what changed since a time, their own tasks with
`mine: true`, and the board's counts on the first page of `list_tasks`.
A refused reservation request now says which input was wrong. Schema 25
adds the task search index. Includes PR #254, #255, #256, #257, #258,
#259, #260 and #261.

### Upgrade notes

- **Hubs first, then workstations.** The new task arguments are read by
  the hub: a 0.10.10 hub ignores `q`, `order` and `updated_since` on the
  task list, so a newer client's `search_tasks` or `updated_since` against
  it gets an unfiltered list rather than an error. Upgrade the hub with
  its one-liner before the clients.
- **Schema 25, and the way back.** The first 0.10.11 open of a project
  store adds the task search index and fills it from the existing tasks
  and comments, once. An older binary refuses the newer store ("database
  schema v25 is newer than this binary"), so the way back is the state
  copy the hub one-liner makes before it swaps the binary,
  `<state root>.backup-<UTC time>`, whose path it prints. If the new
  release does not come up healthy, the one-liner puts the previous binary
  and that copy back itself. To go back later, stop the hub, put the
  0.10.10 binary back and replace the state root with the copy; writes
  made since the upgrade are not in it. The client one-liners make no
  such copy of a workstation's state.
- **No setting changes.** No new environment setting, and every route
  and tool keeps its previous defaults: the list's order, the comments'
  order, and every refusal code.

### Added

- **The task list sorts newest first, and refreshes.** The `/tasks` list
  was in creation order, 50 at a time, so on a large project a task
  created a minute ago sat on the last page (on a 305-task project, at
  positions 301 to 305), and a list left open never showed it. The list
  now sorts by update time (newest first, the default), title, state or
  creation, a choice kept per browser. The board shows the newest tasks
  first in each column, so past its 500-card cap it is the oldest that are
  left out. A **refresh** control reloads the list or the board without a
  page load, and the view shows when it was last loaded.
- **`order` on the task list route.** `GET /v1/projects/{p}/tasks` takes
  `order`: `id` (the default, unchanged), `updated_desc`, `updated_asc`,
  `title` (case-insensitive) or `state` (lifecycle order, newest updated
  first within a state). Every order breaks ties by id and pages exactly
  with `after`. Under `id` the cursor is still the last task id; under the
  others it is an opaque token that only its own order accepts. The MCP
  `list_tasks` tool is unchanged.
- **Task search.** A task can be found by words: the `/tasks` list has a
  search field, `GET /v1/projects/{p}/tasks` takes `q`, and agents get a
  `search_tasks` MCP tool (also in team mode, as a read). Every term must
  appear, literally and case-insensitively, in the task's title,
  objective, acceptance criteria or non-goals, or in one of its comments.
  Matches come best first, and each row says whether the best hit was in
  the task or in a comment and shows it in context. The state, assignee,
  epic and archived filters still apply; a search takes no `order` (the
  console's sort is off while one is active, and refresh still works).
  Schema 25 adds the search index, kept in step by triggers on every task
  and comment write; the upgrade indexes the existing tasks and comments
  once, when a project is first opened, and `aimem fts-rebuild` rebuilds
  it with the other indexes.
- **Task comments, newest first.** Reading the latest comment of a long
  discussion meant paging to its end. `GET /v1/tasks/{id}/comments` takes
  `order=desc` (the default `asc` is unchanged): the first page starts at
  the newest comment and `after` continues toward older ones. The MCP
  `list_task_comments` tool takes `last` (1 to 100): the N newest
  comments, newest first, with a cursor for the ones before them.
- **What changed, and my tasks, in one call.** `GET /v1/projects/{p}/tasks`
  takes `updated_since` (an RFC 3339 time: only the tasks updated at or
  after it), with any order or a search. The MCP `list_tasks` tool takes
  `updated_since`, the list's `order` (the default stays creation order)
  and `mine: true`, the tasks assigned to the caller's own user identity
  (not its groups; refused with `assignee`, and for a token without a user
  identity). The hub's `/mcp` endpoint now lets task tools read the
  caller's own identity, which `mine` needs.
- **A board's shape in one call.** `GET /v1/projects/{p}/tasks/counts`
  returns the active tasks per state (every state, zero included), the
  archived ones and the total, under the list's `assignee` and `epic`
  filters. The first page of the MCP `list_tasks` tool (no `after`)
  carries the same `counts`, so an agent sees how many tasks are in each
  state without listing them all. The `/tasks` list shows the numbers on
  its state chips.

### Fixed

- **`aimem process show` on the hub host.** Run as the service user in a
  directory with no hub binding, right after `aimem process select`, it
  printed an empty line and exited 0. In a directory with no hub binding,
  on a machine with no hub configured, it now reads the project's
  selection and stored set from this host's service, as `list` and
  `select` do, and prints the unit a session receives. Whatever `show`
  cannot print is an error that names why, never an empty success.
- **A refused reservation request says what was wrong.** A reservation
  write the hub refused as `invalid_request` answered only "The request is
  not a valid reservation request.", and the reason (such as a commit
  reference that is a bare hash rather than a URL) was in the hub log,
  where an agent cannot read it. `invalid_request` and
  `unsupported_version` now carry a `detail` field naming the input that
  failed, and the MCP reservation tools pass it through. Conflict refusals
  are unchanged: their detail may describe another holder and stays in the
  log.

### Docs

- **The Windows one-liner in both forms.** The docs show the direct
  `irm .../boot.ps1 | iex` for a PowerShell window that is already open,
  and the `powershell -NoProfile -ExecutionPolicy Bypass -Command` wrapper
  for cmd (or the Run dialog, for an upgrade). On one host the wrapper
  typed in an open window was refused (`Program 'powershell.exe' failed
  to run: Access is denied`) while the direct form ran.

## [0.10.10] — 2026-10-20

A hub host without `git` now selects a process by its release tag, and
reads a private process repository with a forge token from the hub
user's env file: one token for every host (`AIMEM_PROCESS_FORGE_TOKEN`),
or one per host (`AIMEM_PROCESS_FORGE_TOKENS`) when the hub's projects
use more than one forge. No schema or API change. Includes PR #249, #250, #251 and
#252.

### Upgrade notes

- **Workstations: the client one-liner is optional.** The new settings
  are read by `aimem process select` and `aimem process refresh` on the
  hub host; nothing changes on a workstation.
- **Hub hosts: a token only for a private repository.** A hub whose
  process repositories are public needs no setting. For a private
  repository on a hub host without `git` (or where Git cannot read it),
  put `AIMEM_PROCESS_FORGE_TOKEN`, or `AIMEM_PROCESS_FORGE_TOKENS` with
  one `host=token` entry per forge, in the hub user's
  `~/.config/aimem/env` before the hub one-liner's refresh (ADMIN-MANUAL
  names both). A malformed entry fails the forge read with its position
  only. Selecting by tag no longer needs `git`.

### Added

- **A forge token for process repositories, and tags without Git.** On a
  hub host without `git` (or where Git cannot read the repository),
  `aimem process select` and `aimem process refresh` now read a private
  repository's commit tarball with `AIMEM_PROCESS_FORGE_TOKEN` from the
  hub user's `~/.config/aimem/env`, and `select <repo> <tag>` resolves the
  tag through the forge's ref advertisement
  (`<repository>/info/refs?service=git-upload-pack`) to the commit `git
  ls-remote` gives. The token is sent as `Authorization: token <token>`
  to the repository's own host only, dropped on a redirect to another
  host, and never printed or stored.
- **A forge token per host.** A hub whose projects use more than one
  forge gives each host its own token in `AIMEM_PROCESS_FORGE_TOKENS`
  (`host=token` entries separated by commas, matched without
  case and with the port, `:443` implied), so each forge receives only
  its own. `AIMEM_PROCESS_FORGE_TOKEN` stays the token for any other
  host.

### Fixed

- **A flaky enrollment test.** The first-run expiry test issued an
  enrollment that expired three seconds later by the wall clock, so on a
  loaded runner the hub could refuse it as already in the past. The
  client now reads a first run's expiry through a clock the test
  advances, and the test uses the default expiry. Behaviour is
  unchanged.

### Docs

- **Agent instructions hold in every mode.** The repository's
  `AGENTS.md` and `CLAUDE.md` now hold both in an operator's checkout and
  in an agent team member's worktree: where a workflow rule differs by
  mode, the agent home's `START.md` and the project's process set
  decide. Every rule about the code is unchanged.

## [0.10.9] — 2026-10-19

The refresh that the hub one-liner runs after an upgrade now stores the
process sets of a public repository on a hub host without `git`, with no
manual step. `aimem process select`, `refresh` and `export` read the
forge's commit tarball over https when Git is absent or cannot read the
commit, and the hub one-liner warns at its start when `git` is missing. No schema or API change. Includes PR #247.

### Upgrade notes

- **Workstations: the client one-liner is optional.** The change is in
  the admin commands on the hub host; on a workstation it only lets
  `aimem process export` read a public repository without `git`.
- **Hub hosts: `git`, or https to the forge.** The hub service needs
  neither, but `aimem process select` and `aimem process refresh` (which
  the hub one-liner runs after an upgrade) read a process set on the hub
  host. With `git` they use it; without it they now read the forge's
  commit tarball over https, so a hub host without `git` refreshes the
  sets of a public repository on its own. A private or ssh-only
  repository without `git` still needs the archive import, and selecting
  by tag needs `git`. The hub one-liner warns at its start when `git` is
  missing.

### Changed

- **Process sets are read from the forge's tarball when Git cannot read
  them.** `aimem process select`, `refresh` and `export` try Git first
  when the host has it, then fetch `<repository>/archive/<commit>.tar.gz`
  over https (the GitHub and Gitea form; ssh and scp-like repositories
  have none). The tarball must record the selected commit in its pax
  header, as `git archive` writes it on both forges, and must hold the
  manifest; its files are read as an imported archive's are, so the set
  and its digest are those Git gives. The report says whether a set came
  from Git, the forge tarball or an archive, and a set read from neither
  is reported with both reasons and the export command.

## [0.10.8] — 2026-10-19

People select a project's process by its release tag, and upgrading a
hub stores the process sets of older selections without a manual step.
`aimem process select <repo> <tag>` resolves the tag on the hub host and
pins the commit it points at; the manifest path has a default. The hub
one-liner now runs `aimem process refresh`, which replaces the manual
re-select step of the 0.10.7 notes. The next step that `aimem tasks on` prints
shows the tag form. No schema or API change. Includes PR #242, #243,
#244 and #245.

### Upgrade notes

- **Hubs: the process sets are stored on upgrade.** The hub one-liner
  now runs `aimem process refresh` after the upgraded hub answers its
  health check. For each selection made before 0.10.7, it stores the set
  from the recorded repository, commit, manifest and ref, and prints a
  line per project. This replaces the manual re-select step of the 0.10.7
  notes. On a hub upgraded by hand, run `aimem process refresh` as the
  service user on the hub host. A project whose set the host cannot read
  from Git is reported with the `aimem process export` command to run on
  a machine that has access, and `aimem process refresh -p <project>
  --from-archive FILE` then imports the archive. The refresh never fails
  the upgrade.
- **Workstations: the client one-liner is optional.** It brings the tag
  into the session's `Process set:` line and the tag form into a
  member's not-ready message. Nothing else changes on a workstation.

### Added

- **A process is selected by its tag.** `aimem process select <repo>
  <tag> -p <project>` takes a tag, or a branch, where the commit went.
  The command on the hub host resolves it with `git ls-remote` (an
  annotated tag to the commit it points at), and the selection stores
  both: the tag for people, the commit as the pin, so a session still
  sees exactly the reviewed commit. The manifest path defaults to
  `processes/aimem/manifest.json`. `--expect` takes the current
  selection's tag or commit, and re-selecting the commit and manifest already
  selected needs no `--expect`. A full commit, with `--ref` as the fetch
  hint, works as before. The session's `Process set:` line names the tag
  beside the commit. No schema or API change: the selection's existing
  `ref` field holds the tag. The next step that `aimem tasks on`,
  `aimem project show` and the admin API print for a project with no
  process, and a member's not-ready message, now show the tag form. A
  process the hub already uses is still offered by its exact commit, with
  the tag it was selected by as `--ref`, so the new project follows the
  same version as the others.
- **`aimem process refresh` and `aimem process list`.** Both run on the
  hub host. `list` prints every project with a selection: its commit, its
  ref, its manifest, and whether its set is stored. `refresh` stores the
  set of every selection that has none, from Git at the recorded commit or
  from a verified archive (`-p` with `--from-archive`; the archive's file
  name must carry the commit, as `aimem process export` names it). It uses the compare-and-swap of a re-select
  and leaves a selection that has its set unchanged. It reports each
  project as refreshed, already stored, or needing an archive, and a
  project it cannot refresh does not fail the run. The export and refresh
  commands it prints for a set it cannot read quote each value for a POSIX
  shell where needed, so a manifest path holding `$`, `;` or a quote is
  pasted as it is.

## [0.10.7] — 2026-10-19

A security release. It addresses three findings of an independent security
review of aimem, done with Claude Mythos 5.1 (see Security below), and
ships the hub-served process and skills sets. Once the admin re-selects
a project's process on the upgraded hub (see Upgrade notes), its members
no longer need Git or access to the process repository: the hub stores
the selected process files and the imported skills, and `aimem skills install` installs the
skills a project requires from the hub. `aimem tasks on` names the
missing process with the values to run. No schema change. Includes PR
#235, #236, #237, #238, #239 and #240.

### Upgrade notes

- **Upgrade the hubs and the workstations.** The A-1 fix is in the admin
  console and the hub's own checks, so it takes effect when the hub host
  runs this release (the hub one-liner). The A-2 fix and the A-10 tool
  descriptions are in the client (`aimem mcp` and the `aimem` commands on
  a workstation), so they take effect with the client one-liner and a
  restart of the agent client.

- **Re-select each project's process on each hub after the hub host runs
  this release.** A selection recorded before it carries no set: the
  upgraded hub answers 404 on `GET /v1/projects/{p}/process/set` for it,
  members on the new client keep fetching that commit from Git with their
  own access as before, and a member without Git access is not ready for
  that project. The admin re-selects on the hub host with the current
  commit as `--expect` (`aimem process select <repo> <commit> <manifest>
  --ref <branch> --expect <commit> -p <project>`); that reads the files
  from Git on the hub host, or from an archive with `--from-archive`, and
  stores them with the selection. Once per project on each hub. No schema
  change: the sets live in the project's meta table.
- **The skills store is hub state.** `root/skills/` under the hub's state
  root (`index.json` and `sets/<digest>/`) holds the imported skills sets;
  a backup or a migration of the state root carries it. `aimem health`
  reports it under `skills`: `none` before any import, `ok` with the
  versions per source, or `fault` with the error when the index exists
  and cannot be read.

### Added

- **The hub serves the skills; a member installs them from the hub, never
  from a URL.** `aimem skills import --from-archive FILE [--sums FILE]
  [--source release|instance] [--version V]` on the hub host verifies a
  skills release archive (`skills/<name>/…` under one leading directory,
  with its `VERSION` file) or the instance's own skills against the
  `SHA256SUMS` beside it and stores the set on the hub with its digest,
  as the public `release` set or the `instance` set; the hub keeps the
  last five imports per source and never half-stores a set that does not
  validate (each skill a directory with `SKILL.md`, 1 MiB per file, 4 MiB
  per set). `aimem skills export` writes the archive pair back. Two routes
  on the ordinary surface under the process set's grant rule:
  `GET /v1/projects/{p}/skills` (the imports per source and the effective
  skills, an instance skill overriding a release skill of the same name,
  each with its version and source) and `GET /v1/projects/{p}/skills/set`
  (`?source=` the current import, `?digest=` a kept one; ETag by digest).
  `aimem skills install [--user] [--tool …] [--all]` in a checkout
  installs the skills the project's process requires, at the pinned
  versions (a kept earlier import serves a pin the current one no longer
  matches), into the skills directories of the agent clients found on the
  machine, the same the aiskills installers use, replacing a skill
  directory whole and recording each skill's version and source in the
  directory's `.ai-skills.json`; it reads nothing but the hub and works
  from its cache offline. `aimem skills list` prints the effective set
  with each skill's source and what this machine has of it; `--hub` prints
  the hub host's own index. The admin's import routes (`GET /v1/skills`,
  `GET|PUT /v1/skills/{source}`) are outside the ordinary surface: the API
  lists and reads, never imports.
  (docs/DESIGN-portable-team-context.md, increment 1e.)
- **The process manifest pins each required skill, version 2.** A
  version 2 manifest names each skill as `{"name", "version", "source"}`
  (source `release` or `instance`); a version 1 manifest with plain names
  is still accepted (any version present on the hub). An aimem from before
  this release refuses a version 2 manifest outright (its decoder rejects
  the skill objects, before it looks at the version), so it never misreads
  one; the process owner moves to version 2 once every member runs this
  release.
- **The hub serves the process files; a member needs no Git and no access
  to the process repository.** `aimem process select` on the hub host now
  reads the set at the selected commit (the manifest and every file it
  names) from Git, or with `--from-archive FILE` from a tar.gz verified
  against the `SHA256SUMS` beside it, and stores it on the hub with the
  selection, in the same compare-and-swap; `aimem process export` writes
  that archive pair on a machine with access, for a hub host without it.
  The hub keeps the sets of the last five selections, so an earlier one is
  re-selected without an import, and never runs Git itself. A new route on
  the ordinary surface, `GET /v1/projects/{p}/process/set`, serves the
  current selection's set, or a kept earlier one by commit and manifest,
  to a token or team session with a current grant on the project (the
  project repository's read rule); the member verifies the files against
  the selection's digest before caching them by it; a selection without a
  digest (an older hub's, or one made before the upgrade) is fetched from
  Git as before without asking the route. The selection records `digest`
  and `name`.
  (docs/DESIGN-portable-team-context.md, increment 1d, now the default
  path.)

### Changed

- **The required skills line names the version, the source and the
  command.** The session bootstrap, `process_context`, `aimem process
  show` and the `team_setup`/`team_continue` process check print each
  required skill as `oh-code-review 1.33.13 (release; installed)`, with
  `installed 1.33.12, run \`aimem skills install\`` for another version
  and `NOT FOUND, run \`aimem skills install\`` for a missing one (a
  version 1 manifest's skills keep the name-only form with the same
  command); the installed version comes from the installers' record
  beside the skill, `version unknown` without one. No text names a URL.
  `aimem teams setup`'s skills list gives the same command as the fix.
- **The session bootstrap names the process set, not the repository
  URL.** `Process set: <name> @ <commit> (manifest <path>)`, in the
  session-start hook, `process_context`, `team_setup` and `team_continue`
  and `aimem process show`; the name is the manifest's directory, else the
  repository's name. `aimem process show --full` prints the URL after the
  unit, for the admin. The availability notices name the set the same way,
  and Git's own messages are redacted of the repository URL and host
  before they reach one; a hub that refuses the set read is reported as
  denied and Git is not tried in its place. `PUT /v1/projects/{p}/process` refuses a selection
  that carries neither `files` nor the `digest` of a kept set, so an older
  `aimem process select` against a new hub is refused rather than storing
  a selection no member can read from the hub.
- **Turning tasks on names the missing process with values you can run.**
  A project with tasks on and no process selected is not ready for work:
  no member can take a task, and the first agent session there fails at
  its start with `project_process: not_selected`. `aimem tasks on`, and
  the admin API's answer to it, now say that at once and print the
  selection command with the values of the processes this hub already
  uses for other projects, most used first, quoted for a POSIX shell
  where a value needs it. When the hub uses none, the
  command comes with an example of each of its three values: the process
  repository's git URL, the full 40-hex commit and the manifest's path in
  that repository. Nothing is selected by turning tasks on. The admin API
  adds `ready_for_work`, `not_ready`, `processes` and `next_step` to the
  answer, and a new admin route, `GET /v1/processes`, lists the
  selections in use. The console's toggle says the project is not ready.
  `aimem project show` prints the same, and `aimem project list` now
  prints each project's tasks and process state and marks such a project
  as not ready, with the command; `aimem projects` still prints the raw
  id list.

### Security

An independent security review of the whole product was done on
2026-10-18 with Claude Mythos 5.1, with Defense Access under
Anthropic's Cyber Verification Program, at commit `2cb868e` (the
hub-served skills set, #237, landed after it and was not part of that
review). This release addresses three of its
findings, one line each:

- A-1: the admin console could run script from stored values (cross-site
  scripting); fixed.
- A-2: bound documents and other checkout files could be read or written
  through a link that leaves the checkout; fixed.
- A-10: the MCP task-read tools understated the scope of a task read;
  the hub-wide read is now stated honestly, with no behaviour change.

Findings fixed in later releases are listed with those releases. The
fixes in detail:

- **A-1: the admin console no longer places stored values inside inline
  event handlers.** HTML escaping cannot protect a value inside an
  inline handler's script, because the browser decodes the attribute
  before it runs it. Rendered controls now name an allowlisted handler
  and carry their arguments as JSON data attributes, and one delegated
  listener per event type dispatches them. The escape helper of every
  hub page encodes the apostrophe as well. On the hub, imported and
  synced memory ids must be 1 to 64 letters, digits, `-` or `_`, tags
  and sources drop control and markup characters, and chapter names
  with them are refused from the console and from group-config sync.
  A test fails on any new data-bearing inline handler in the console.
- **A-2: files in a checkout are never read or written through a link.**
  Bound shared documents (the checkpoint publisher and pull,
  `update_doc`, and the `aimem docs` commands), collection renders to a
  bound path, `aimem handoff compact`, the client files a setup repair
  writes, and project-level `aimem skills install` now refuse a path
  when any component below the checkout root is a symbolic link or
  junction, the file itself included. A link that points inside the
  checkout is refused too, and so is a path into the checkout spelled
  with a `..` element, which the system could resolve through a link. The checkout root itself may be a link, and
  user-level directories are written as before. On Windows, a reparse
  point that does not redirect, such as a cloud-sync placeholder, is not
  treated as a link. Writes go to a temporary file with an unpredictable
  name, which is then renamed into place. A second escape is closed too:
  a bare `..` in `.aimem.json`, as a docs binding or a collection render
  path, named the checkout's parent directory and is now dropped.
- **A-10: the hub-wide task read is stated where agents and operators
  read it.** An ordinary token reads the tasks, comments, history and
  epics of every project on the hub. A project-scoped token and a missing
  grant limit writes only. The MCP task and epic read tools said only that
  the project argument defaults to the current project, which reads as a
  limit. Their descriptions now state the hub-wide read and its team-mode
  exception. The identity route's API description explains `task_read`,
  the new-project quickstart warns that a task is visible to every user of
  the hub, and the access-control design marks this as the current
  contract until task reads come under the project grant. No behaviour
  changes.

## [0.10.6] — 2026-10-16

A new project set up by the quickstart now ends at a board its agent
works from. Before, a project could have tasks on and no process
selected; every session then started with the notice that no process
reference is selected, and the agent took that as "not ready" and never
used the board. The quickstart now selects and verifies the process and
walks through the first day on the board, and `aimem tasks on` and
`aimem project show` report tasks on with no process as an unfinished
setup, with the next step. The OpenCode plugin's context knobs in
`.aimem.json` now apply to a session started in a subdirectory or a
linked worktree, as the project id already did. No schema change and no
hub change. Includes PR #231, #232 and #233.

### Upgrade notes

- **No schema change and no hub change.** Hubs may stay on 0.10.5: the
  new `tasks on` and `project show` output reads routes a 0.10.5 hub
  already serves.
- **Hub hosts where projects are set up: a 0.10.6 `aimem` command for
  the new next step and warning.** `aimem tasks on` and `aimem project
  show` run on the hub host, so the next step and the `tasks on, process
  not selected` warning appear once that host's `aimem` command is
  0.10.6. An older command keeps working without them.
- **OpenCode workstations: the client one-liner, then restart OpenCode.**
  The installer copies the plugin, and the plugin reads its knobs when
  it loads, so a project's `ctx_limit`, `ctx_warn_fraction` and
  `auto_compact` apply in a subdirectory or a worktree only after the
  0.10.6 client one-liner and a restart of OpenCode.

### Changed

- **Tasks on with no process selected is reported as an unfinished
  setup.** Such a project's sessions start with the notice that no
  process reference is selected, and an agent can take that as "the
  board is not ready" and leave it alone. `aimem tasks on` now checks
  the project's process selection and, when there is none, prints the
  next step with the shape of `aimem process select`. `aimem project
  show` prints whether tasks are on, and for tasks on with no process
  it adds `warning:    tasks on, process not selected` and the same next
  step. A project whose tasks are off, one that only journals, gets
  neither.

### Fixed

- **OpenCode: the context knobs of `.aimem.json` apply in a
  subdirectory or a linked worktree.** The plugin read `ctx_limit`,
  `ctx_warn_fraction` and `auto_compact` from `.aimem.json` in the
  directory OpenCode was launched in only, so such a session got the
  host defaults instead of the project's overrides. It now finds the
  governing `.aimem.json` the way the project id is found: the nearest
  one up to the checkout's top, then a linked worktree's main
  checkout. Both OpenCode generations.

- **The new-project quickstart ends at a board the agent works from,
  not at "tasks on".** A project set up by the quickstart had tasks on
  and no process selected, so every session started with the notice
  that no process reference is selected, and the agent took that as
  "not ready" and never used the board. Step 2 now selects the process
  (the aiskills set at `processes/aimem/manifest.json`, at a full
  commit id), and step 3.6 verifies it with `aimem process show`. A new
  step 4 adds the line that makes the board authoritative in the
  project's `AGENTS.md`, the first epic and tasks from the process's
  task template, where a person's decisions go, and how to check that
  a new session sees all of it. The command check in
  `cmd/aimem/quickstart_doc_test.go` now covers `aimem process`.

## [0.10.5] — 2026-10-15

A session started in a linked git worktree or in a subdirectory of a
checkout now journals under the project its `.aimem.json` pins, on the
hub that file binds. Before, `.aimem.json` was read only in the directory
the hook or command ran in, so such sessions silently journaled under a
derived twin id on the machine's default hub while the pinned project's
journal, handoff and recall went stale. The hooks, `aimem project-id`,
`session-start`, the MCP server and the OpenCode submit path now resolve
the governing file the same way, without a git binary. The new-project
quickstart covers a setup resumed after a pause, the two "aimem" Login
Items on macOS and an optional step that installs the agent skills. No
schema change and no hub change. Includes PR #228 and #229.

### Upgrade notes

- **No schema change and no hub change.** Hubs may stay on 0.10.4.
- **Workstations: the client one-liner, then restart the agent
  sessions.** The identity fix lives in the client binary, the hooks and
  the submit path. Every workstation whose sessions run from a linked
  worktree or a subdirectory needs the 0.10.5 client one-liner, then a
  restart of its agent sessions, so the hooks and the MCP server load the
  new binary.
- **Events already journaled under a twin id stay there.** They are not
  migrated to the pinned project.
- **A worktree that inherits a local task credential requirement
  refuses.** When the main checkout's `.aimem.json` requires a local task
  credential, task tools in a worktree that inherits that file refuse
  until the worktree gets its own `.aimem.json`, as the entry below
  describes.

### Fixed

- **A session in a git worktree or a subdirectory journals under the
  pinned project.** `.aimem.json` was read only in the directory a hook,
  `aimem project-id`, `session-start` or the MCP server ran in. A linked
  worktree has no copy of the file when it is gitignored, and a
  subdirectory has none, so such sessions journaled under a derived twin
  id (the git remote plus a hash) on the machine's default hub, silently,
  while the pinned project's journal, handoff and recall went stale. Now
  every caller resolves the governing file the same way: the nearest
  `.aimem.json` up to the checkout's top, then, from a linked worktree,
  the main checkout's, found through the worktree's `.git` and
  `commondir` files with no git binary. The pin, hub binding and groups
  travel together, and bound documents are read where that file lives. A
  directory with its own `.aimem.json` keeps it; a nested checkout stops
  the search; outside any checkout nothing changes. The OpenCode plugin
  still sends its launch directory and the service resolves it. Task
  credentials keep the per-checkout rule: a subdirectory uses its
  checkout's, and a worktree whose main checkout requires a local task
  credential refuses until the worktree is configured itself. Events
  already journaled under a twin id stay there.

- **The new-project quickstart covers a paused setup, the macOS Login
  Items and the agent skills.** The verify step now names the hub
  registration steps as its prerequisite, and the troubleshooting table
  explains `no hub configured`, which is what the verify commands print
  when those steps were skipped. The two "aimem" entries macOS lists
  under Login Items are the serve and sync agents; the quickstart and
  `docs/INSTALL-CLIENT.md` say how to tell them apart. A new optional
  step 3.2 installs the agent skills from the aiskills set, which the
  review gates and team processes need; the later steps are renumbered.

## [0.10.4] — 2026-10-14

On macOS the client one-liner now runs aimem as a service. It registers two
launchd user agents: one runs `aimem serve` from login and restarts it if it
fails, and one runs `aimem sync --all-hubs` every 10 minutes, so a Mac pulls
from its hubs without a separate step, as Windows does (Linux runs the sync
once `install.sh enable-sync` sets up its timer). Before, the operator
started `aimem serve` by hand after each login and nothing ran the periodic
sync. OpenCode now gets the failure recall that Claude Code gained in
0.10.3: a failed shell call brings back the stored memories that share its
error's words. A new quickstart takes a fresh workstation to a working
project on an existing hub, and its Windows note now names the real
`.aimem.json` trap: a UTF-8 byte order mark is read, UTF-16 from `Out-File`
or `>` is not. No schema change and no hub change: a 0.10.3 hub or
workstation upgrades in place. Includes PR #223 to #226.

### Upgrade notes

- **No schema change and no hub change.** A 0.10.3 hub or workstation
  upgrades in place with its one-liner.
- **macOS: stop a serve started by hand once.** Releases up to 0.10.3
  had you start `aimem serve` by hand on a Mac. Stop that serve before you
  run the 0.10.4 client one-liner: the installer refuses while it answers,
  and stops nothing by name. After the one-liner the two agents run from
  login; `launchctl list | grep io.github.blackvs.aimem` shows them.
  `AIMEM_NO_SERVICE=1` keeps the manual mode. Run the one-liner in a login
  session; over ssh with nobody logged in at the Mac, the agents load at
  the next login.
- **Linux and Windows:** the client one-liner as before. A workstation gets
  the OpenCode failure recall from its next one-liner run, which installs
  the plugin file.

### Added

- **macOS runs aimem as launchd user agents.** On a Mac the client
  installer now registers two agents in `~/Library/LaunchAgents`:
  `io.github.blackvs.aimem` runs `aimem serve` from login and restarts it
  if it fails, and `io.github.blackvs.aimem.sync` runs
  `aimem sync --all-hubs` every 10 minutes, so the pull leg runs without
  a separate step, as on Windows (Linux runs it once `install.sh
  enable-sync` sets up its timer). Before, the installer set up no
  service on macOS: the operator started `aimem serve` by hand after each
  login, and an upgrade refused while that serve answered. Now an upgrade
  stops the agents, swaps the binary and starts them again;
  `uninstall-user` unloads and removes both; `enable-sync <ssh-dest>`
  repoints the sync agent and a later install keeps it. A serve started by
  hand under an earlier release has to be stopped once before the first
  upgrade: the installer refuses while it answers and stops nothing by
  name.
  `AIMEM_NO_SERVICE=1` (or `AIMEM_NO_SYSTEMD=1`) keeps the manual mode.
  Without a login session (ssh with nobody logged in at the Mac) the
  agents are written and load at the next login. The macOS CI job
  installs, upgrades and uninstalls against the runner's real launchd.
- **A quickstart for one new project on a fresh workstation.**
  `docs/QUICKSTART-NEW-PROJECT.md` takes an operator from a hub that
  already runs to a project on a new macOS, Linux or Windows machine with
  tasks, the session handoff and shared design documents. It covers the
  three names to decide, the hub-side commands (a writer token, tasks
  on, the user, the grant, the user task token), the workstation
  commands in order (the one-liner, `aimem hub add --token-file`,
  `aimem hub task-token`, `.aimem.json` with `docs` bound), the order to
  verify in, and how the two secrets move and are revoked. A test checks
  every `aimem` command in the guide against the CLI's usage text, so a
  renamed subcommand or flag fails the build instead of the operator's
  run. QUICKSTART and README link to it.
- **OpenCode gets the failure recall too.** When a shell call fails, the
  OpenCode plugin runs `aimem failure-recall` (#219) with the failure and
  gives the model what it prints: at most 3 stored memories that share
  the error's words, or nothing. The plugin carries no copy of the
  query, redaction or matching; the Go command does all of it. On
  OpenCode 2.x a non-zero exit arrives as a successful `shell` call with
  `metadata.exit` set, and the recall goes into the next model request
  through the `context` hook. On 1.x the plugin's `tool.execute.after`
  hook sees the failed `bash` call, and the recall is appended to its
  output. A recall that fails or takes over 5 seconds adds nothing. The
  plugin never cuts the error text, so the command redacts all of it
  before any size cap; a failure over 512 KiB is not recalled. The
  plugin's end-to-end check gains `recall` and `norecall`, which run the
  real binary against a fake store on both generations. The plugin
  file is installed by the client one-liner, so a workstation gets it
  from its next one-liner run.

### Fixed

- **The new-project quickstart names the real `.aimem.json` trap on
  Windows.** It said aimem cannot read a `.aimem.json` that starts with a
  byte order mark, but the loader strips a UTF-8 BOM before parsing. The
  trap is UTF-16: in Windows PowerShell 5.1, `Out-File` and `>` write
  UTF-16LE, and aimem then warns that `.aimem.json` is unreadable and
  derives a hash-suffixed project ID. `Set-Content -Encoding ascii` and
  `-Encoding UTF8` (with a BOM) are both read. Step 3.4 and the
  troubleshooting row now say so (#225).

## [0.10.3] — 2026-10-13

Agents consult aimem before they report an environment blocker. A failed
Bash command in Claude Code now brings back the stored memories that share
its error's words, through a new user-level hook running `aimem
failure-recall`. The installers wire that hook on the next client
one-liner run: the 0.10.3 one-liner upgrades an older workstation and adds
the hook to its Claude Code settings, while a binary swapped by hand or a
hub-only install does not get it. Session start and the installed
`AGENTS.md` template tell the agent to recall aimem with the error text
before reporting such a blocker. The OpenCode 2.x plugin now injects the
handoff with the 8 KiB limit of `aimem session-start`, and `aimem handoff
compact` keeps a closing section under any of six common names. No schema
change and no breaking change: a 0.10.2 hub or workstation upgrades in
place and rolls back in place. Includes PR #218 to #221.

### Added

- **The guidance sends environment blockers to aimem first.** Session
  start (Claude Code and Codex) now carries one line, and the installed
  `AGENTS.md` template a short "Environment blockers" section. Before
  the agent reports a blocker caused by the environment (credentials,
  SSH, hardware keys, network, missing tools), it recalls aimem with the
  error's text and applies what it finds. A team conversation, where
  recall is off, does not get the line. An existing project keeps its
  own `AGENTS.md`, and gets the rule from the session-start line.
- **A failed command recalls aimem before the agent reports it.** Agents
  reported environment blockers, such as a push refused because the SSH
  identity needed a hardware key's interactive PIN, although aimem held
  the remedy; they found it only when told to check aimem. The installers
  now add a user-level Claude Code `PostToolUseFailure` hook for `Bash`
  that runs the new `aimem failure-recall`. A command that exits non-zero
  fires `PostToolUseFailure`, not `PostToolUse` (observed on Claude Code
  2.1.296). The hook reduces the error to at most 20 distinctive words:
  redacted first, with the exit line, paths, numbers, hashes and common
  words left out. It recalls the project's and the user's memories with
  them through the local service, within 2 seconds. It returns at most 3
  memories that share enough of the words (2, or 3 for an error of 8 words
  or more), each redacted and cut at 400 bytes, 1500 bytes in all, as
  context the agent reads before it answers. It is silent when nothing
  qualifies, when the local service is not running, for an interrupted
  command and in a team conversation, and it always exits 0. An existing
  installation gets the hook from its next one-liner upgrade; uninstall
  removes it with the other aimem hooks. Codex is not covered: its hooks
  document no event for a failed command.

### Fixed

- **OpenCode 2.x injects the handoff with the session-start limit.** The
  plugin's context hook, which runs on every model call, injected
  `docs/SESSION-STATE.md` whole up to 64 KiB, and skipped a larger one
  without a word. It now applies the limit of `aimem session-start`
  (0.10.1): a handoff over 8 KiB is injected as its head, cut at the last
  line break within the limit, followed by the same one-line note naming
  the file, its size and `aimem handoff compact`. Only the head is read.
  A Go test keeps the plugin's limit and note equal to session start's,
  and the plugin's end-to-end check gains an oversized-handoff scenario.
  OpenCode 1.x loads the handoff through `opencode.json` `instructions`,
  whole and outside aimem's control; `docs/INSTALL-CLIENT.md` now says so,
  and that the 50-line rule and `aimem handoff compact` keep it small
  there.
- **`aimem handoff compact` keeps the closing section under the names
  projects use.** It recognised the closing section only by the name
  "Pick up here". On a real 9530-line handoff whose closing section was
  "Exact next action", that section went to the archive with the history.
  Compact now keeps the last section of up to 10 lines whose heading, or
  bold or plain lead-in, is one of Pick up here, Pick up, Exact next
  action, Next action, Next or Next steps, in any case, alone or followed
  by a colon, a parenthesis or a dash ("Pick up here" with anything after
  it, as before). A heading that only starts with a name, such as "Next
  week", is not one. The section starts at its first such line, so a
  "Next:" line inside it does not cut it short. The command's usage and
  the installed `AGENTS.md` template list the names.

## [0.10.2] — 2026-10-13

A handoff that has grown past the session-start limit is now compacted
with a command instead of by hand. `aimem handoff compact` keeps the
current state in `docs/SESSION-STATE.md` and moves the history to
`docs/SESSION-STATE-ARCHIVE.md`; `--summarize` adds a short summary from
the curation backend. Existing projects keep their own `AGENTS.md`,
because the installers never overwrite it: such a project does not get
the new template rule, and learns the command from the session-start
note, which names it whenever the handoff is over the limit. No schema
change and no breaking change: a 0.10.1 hub or workstation upgrades in
place and rolls back in place. Includes PR #213 and #216.

### Added

- **`aimem handoff compact`** moves history out of `docs/SESSION-STATE.md`
  into `docs/SESSION-STATE-ARCHIVE.md`, under a dated heading, so a handoff
  that has grown past the session-start limit no longer needs cutting by
  hand. It moves everything below the line `<!-- aimem: history below -->`.
  In a file without that line, it keeps the head that fits 50 lines and
  8 KiB, ending at a heading or blank line, plus a closing "Pick up here"
  section, and adds the line. It prints the moved line range, size and
  headings. A rerun, or a run on a compact handoff, changes nothing, and a
  run interrupted after the archive write does not archive the block twice
  when run again. `--summarize` asks the curation backend (the one
  `aimem curate` uses) for a short summary of what moved, at most 12 lines
  of done, decisions and open items, redacting the text first, and puts it
  before "Pick up here". Without a backend, or when the call fails, the
  command archives only and says why. `--dry-run` reports without writing
  or calling the backend. The session-start note over the limit now names
  the command, and the installed `AGENTS.md` template tells the agent to
  run it rather than cutting the file by hand, while the agent still
  rewrites the current state itself at milestones.

### Changed

- `golang.org/x/sys` moves from v0.48.0 to v0.49.0. The on-disk schema
  is unchanged.

## [0.10.1] — 2026-10-13

Session start no longer injects a growing handoff whole. `aimem
session-start` injects at most the first 8 KiB of `docs/SESSION-STATE.md`
and a one-line note when the file is longer. The installed `AGENTS.md`
template says the handoff holds only the current state and history goes to
`docs/SESSION-STATE-ARCHIVE.md`, which is never injected. No schema change
and no breaking change: a 0.10.0 hub or workstation upgrades in place and
rolls back in place. Includes PR #214.

### Fixed

- **Session start injects at most 8 KiB of the handoff.** `aimem
  session-start`, the SessionStart hook for Claude Code and Codex,
  injected `docs/SESSION-STATE.md` whole, again after every compaction.
  Handoffs grow over sessions: on one machine the injection took about
  half the context at once, and on another it was 13 KB. A handoff over
  8 KiB is now injected as its head, cut at the last line break within
  the limit, followed by one line that names the file, its size and the
  limit, and points history to `docs/SESSION-STATE-ARCHIVE.md`. A handoff
  within the limit is injected unchanged, and team conversations get the
  same limit. The installed `AGENTS.md` template now says the handoff
  holds only the current state and that history goes to
  `docs/SESSION-STATE-ARCHIVE.md`, which nothing injects. A project that
  already has an `AGENTS.md` keeps it: the installers never overwrite it.

## [0.10.0] — 2026-10-09

aimem's prerequisites for aicrew's control plane (aicrew's control-plane
design, A0, A0b and A0c). aicrewd reads a cursor feed of task state
changes across its teams' granted projects with a new peer operation,
`board.read`. A task can name the capability it requires
(`required_capability`), and the feed carries it. A team member can
deliver a report as a `report-*` hub document under the team's grant. An
existing peer gets the new `board.read` credential by running
`aimem identity peer provision` again.

Project schema 24 and access schema 8 are one-way: read the upgrade notes
before upgrading a hub or a workstation. Includes PR #208 to #211.

### Added

- **The board feed, `board.read`** (`GET /v1/identity/peers/{service_id}/board-changes`),
  for aicrew's control plane (its design's prerequisite A0). With one call
  per tick, aicrewd reads the task state changes of every project granted
  to its enabled teams: task, project, the state before and after, the
  revision and its time, and never a task's content. Paging is by an
  opaque cursor bound to the peer: the same cursor reads the same page
  again, reading on never skips or repeats a change, and a revoked grant
  that comes back resumes where the peer stopped. A malformed or foreign
  cursor is `invalid_cursor`; a cursor past the feed, as after a restore,
  is `cursor_ahead`. It needs its own single-operation peer credential,
  under the same TLS, version, rate and audit rules as `team.read`.
  `aimem identity cred issue|rotate --operation board.read` issues one,
  and `aimem identity peer provision` now writes five credentials, adding
  `aimem-board-read.token`. Contract: identity.v1, "Board feed".
- **Schema.** Project schema 24 adds `task_state_changes`, the feed's
  record, and fills it from the task history, so the feed also covers the
  changes made before the upgrade. Access schema 8 rebuilds the peer
  credential table to admit `board.read`, keeping every credential. Both
  are one-way: a hub or workstation that has run this release does not
  start an older binary on the same state.
- **A task's required capability** (`required_capability`), for aicrew's
  control plane (its design's prerequisite A0b, decision D15). It is an
  optional field of the task content, `"<kind>: <name>"` such as
  `"ops: network-x"` or `"code: forge github.com"`: a lowercase kind, one
  colon and one space, then a name with no surrounding space, at most 256
  bytes. Empty means the project's own capability. Whoever may write the
  task sets or clears it through REST, `create_task` and `update_task`,
  under the usual revision rules; a malformed value is refused and
  nothing changes. It is kept in the task's history, and each `board.read`
  entry carries it as of the change's revision. It lives in the task
  body, so it needs no schema change.
- **A team member's report as a hub document**, for aicrew's control
  plane (its design's prerequisite A0c). Team mode now serves
  `PUT /v1/projects/{p}/docs/{name}` for every member role, under the team
  profile's live grant, for documents named `report-*` only: another name
  is `invalid_request`, an ungranted, unknown or reserved project is
  `grant_denied`. The revisions are compare-and-swap as in personal mode.
  The revision's writer is `<token name>/team:<team id>/<role>`, and the
  write is audited as `team.doc.write`. A team conversation lists
  `write_report`, and the context report says `doc_write: "report-*"`.
  Deleting, merging and the log stay off in team mode, and an ordinary
  token in personal mode still writes no document. Contract: identity.v1,
  "Team-mode report documents".
- **Adding a new operation's credential to an existing peer** is pinned
  by a test and described in the runbook (step 3, "After an upgrade that
  adds an operation"). After the hub is upgraded, rerunning `aimem identity
  peer provision` with the same service, endpoint, trust and output
  directory issues only the missing `board.read` credential into
  `aimem-board-read.token`. It keeps every earlier file, credential and
  the hub ID unchanged, and a further rerun issues nothing.

### Fixed

- **OpenAPI's peer credential issuance schema** listed only
  `identity.redeem` and `reservation.read` as operations, although the hub
  also issues `team.register` and `team.read`. It now lists every
  operation, `board.read` included, and a test keeps the spec, the CLI's
  `--operation` list and `peer provision`'s files equal to the operations
  the hub issues.

### Upgrade notes

- **Project schema 24 and access schema 8 are one-way.** Each project
  store, when 0.10.0 first opens it, adds `task_state_changes` and fills
  it from the task history; the access store rebuilds its peer credential
  table to admit `board.read`. Every existing row is kept. An older binary
  refuses the migrated stores, so a hub or a workstation that has run
  0.10.0 does not start 0.9.2 on the same state. Roll back only from a
  backup taken before the upgrade. The one-liner and `install-hub.sh`
  copy the state root before the new binary first opens it, and say
  where.
- **Upgrade the hub before aicrew uses the new routes.** `board.read`,
  `required_capability` and team-mode report writes need a 0.10.0 hub.
  Then give the existing aicrew peer its `board.read` credential by
  running the same `aimem identity peer provision` command again, with
  the same service, endpoint, trust and output directory
  ([`docs/PILOT-HUB-RUNBOOK.md`](docs/PILOT-HUB-RUNBOOK.md), step 3,
  "After an upgrade that adds an operation"). It issues only
  `aimem-board-read.token`; `aimem identity cred list` then shows one
  active credential per operation.
- **The version is 0.10.0, not 0.9.3,** because of the one-way schema
  steps: as with 0.9.0, a minor release marks a store that an older
  binary will not open, and a patch release keeps in-place rollback.

## [0.9.2] — 2026-10-08

Identity peers are set up and removed with one command each. `aimem
identity peer provision` registers a peer and writes its four
credentials and the hub ID into one directory, and `aimem identity peer
retire` removes a disabled peer's state so its name and teams can be
registered again. The one-liner no longer wires the directory it runs in
on an upgrade, and `install.ps1` keeps non-ASCII settings intact. No
schema change: a 0.9.1 hub or workstation upgrades in place, and rolls
back in place. `peer retire` needs a 0.9.2 hub, since it adds the
retirement route. Includes PR #200 to #206.

### Added

- **`aimem identity peer retire SERVICE`** removes a disabled identity
  peer's state (`POST /v1/identity/peers/{service_id}/retirement`, hub
  admin). After the pilot's aicrew peer was renamed, `team.register`
  under the new peer was refused with `peer_forbidden`: the old peer's
  team profile, although disabled, still held the team UUID, and nothing
  could remove it. Retirement deletes, in one transaction, the peer's
  credentials, its team profiles with their grants, its proof receipts
  and redemptions, and then the peer itself, and writes one audit row
  that lists the counts; the audit history is kept. The service ID and
  the team UUIDs it held then register again, and a receipt redeemed
  under the retired peer reads as unknown, so members prove again on
  their next join. An enabled peer is refused (409), and so is an
  unknown one (404). `team.register` now checks its peer credential again
  inside its write transaction, as redemption does, so a request that
  authenticated before its peer was disabled, its credential revoked or
  the peer retired writes nothing and cannot leave a profile behind.

- **`aimem identity peer provision SERVICE`** sets up an identity peer
  in one command. Setting up the pilot's aicrew peer had taken seven
  hand-typed commands, and the first try failed with a 400 under the
  hub's one-active-peer rule. The command takes `--endpoint`, the trust
  flag of `peer register`, and `--output-dir DIR`. It registers the peer,
  issues its four credentials into owner-only files under fixed names
  (`aimem-redeem.token`, `aimem-read.token`, `aimem-team-register.token`,
  `aimem-team-read.token`), and prints the hub ID and writes it into
  `aimem-hub-id`, one line, owner-only; `aicrew hub add --cred-dir DIR`
  reads them, so the hub ID is never retyped. A rerun keeps an
  `aimem-hub-id` that holds this hub's ID, replaces one that is empty or
  holds only whitespace, and refuses, before anything changes, one that
  names another hub. A rerun keeps every file that holds a
  credential and issues only for a missing or empty one, so it reports
  the existing peer and files and issues nothing, and it finishes a
  provisioning that was cut short; a kept file whose operation has no
  active credential on the hub is named in a warning, and if the hub's
  credential list cannot be read, the run says the kept files were not
  checked. A non-empty
  credential file is never overwritten. `--replace OLD` disables the enabled old peer in the same
  step, and enables it again if the new registration fails. Credentials
  last 90 days unless `--expires` says otherwise; an expiry in the past
  or more than 366 days ahead is refused before the hub changes. The
  pilot hub runbook now uses it.

### Changed

- **The one-liner upgrades without wiring the current directory.** Run on
  a machine where aimem is already installed, `boot.sh` and `boot.ps1`
  wire the current directory only if it already holds `.aimem.json`;
  otherwise they upgrade the user level and print how to wire a project.
  A fresh install still wires the directory it runs in.
  `AIMEM_USER_ONLY=1` upgrades or installs the user level and wires
  nothing. Before, an upgrade run from the home directory wired the home
  as a project, and every Claude Code session under it read that
  `CLAUDE.md`.

### Fixed

- `install.ps1` reads the user's files as UTF-8. Windows PowerShell 5.1
  read a `settings.json` without a BOM in the ANSI code page, so
  non-ASCII content, such as an emoji in a custom `statusLine`, came
  back garbled when the hook merge wrote the file out as UTF-8. The
  JSON settings and Codex's `config.toml` are now read as UTF-8, with or
  without a BOM. Files an earlier run already garbled are not repaired;
  fix them by hand.

- `aimem identity cred issue` and `cred rotate` refuse an `--expires`
  day count too large for Go's duration type (more than 106751 days).
  Such a count overflowed and wrapped to an arbitrary expiry, as close as
  minutes away, instead of failing.

- The installers refuse to wire the home directory as a project in any
  mode: the one-liner skips it, and `install.sh project ~` and
  `install.ps1 -Target ~` stop with an error before writing anything.
  Homes an earlier run already wired keep their files; remove them by
  hand.

## [0.9.1] — 2026-10-07

The one-line installer becomes the upgrade path. Every documented
one-liner now names this release. On an existing installation the
installer backs up the state root, swaps the binary, waits for health at
the new version, and rolls back both binary and state if the new release
does not come up. Also: the x/text advisory fix and two dependency
bumps. No schema change: a 0.9.0 hub or workstation upgrades in place,
and rolls back in place. Includes PR #195 to #198, with the dependency
PRs #128 and #171.

### Changed

- **The one-line installer is the upgrade path.** Every documented
  one-liner now names a release tag and fetches its boot script from that
  tag, and the script installs that release (`RELEASE` in `boot.sh` and
  `install-hub.sh`, `$release` in `boot.ps1`) instead of whatever is
  latest; the binary is still refused unless its hash is in the release's
  `SHA256SUMS`. On an existing installation, `install.sh`, `install.ps1`
  and `install-hub.sh` now stop only that installation's service and sync
  or curation job, copy the state root without the socket to
  `<state root>.backup-<UTC time>` beside it, swap, and wait for health at
  the new version. If the new release does not come up, the previous
  binary and the state copy both go back and the installer exits with an
  error that says so. A schema move is one-way (the access store moved
  from schema 5 to 7 between 0.7.4 and 0.9.0), so upgrade hubs and
  workstations with the newer release's one-liner, not by swapping
  binaries by hand
  ([ADMIN-MANUAL, Upgrades](docs/ADMIN-MANUAL.md#upgrades)). The backup
  stays; remove it once the new release has proved itself.
- `golang.org/x/text` moves from v0.3.8 to v0.42.0, which clears
  GO-2026-5970 (an infinite loop on invalid input) from the release
  binary. The module arrives only through the TUI dependencies, and no
  aimem code path reached the affected function.
- `golang.org/x/sys` moves from v0.47.0 to v0.48.0, and
  `modernc.org/sqlite` from v1.59.0 to v1.60.1 (with `modernc.org/libc`
  v1.77.1). The on-disk schema is unchanged.
- The pilot hub runbook checks a member home's hub credential
  (`aimem hub credential`) before the first identity proof. It also
  corrects its CRLF note: a carriage return in the hub's own
  `AIMEM_HTTP_TOKEN` breaks only that bearer (401); named admin tokens
  still work.

### Fixed

- A PowerShell module analysis cache, written by the Windows installer
  tests into the source tree, came in with #197 and is removed. The
  tests keep PowerShell's working directory and cache in their sandbox,
  and CI refuses a committed tool cache (`scripts/check-tool-caches.sh`).

## [0.9.0] — 2026-10-07

Enrollment (D1, enrollment.v1:
[`docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md`](docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md)):
the hub admin issues a one-time onboarding subcode, and a clean
installation redeems it for its own user-scoped credential, delivered
sealed with HPKE. Also: hub.json is owner-only on Windows, and building
from source needs Go 1.26.

The access schema moves to 7, one way: read the upgrade notes before
upgrading a hub. Includes PR #184 to #193.

### Added

- **The hub admin issues enrollment bundles** (D1, enrollment.v1:
  [`docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md`](docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md)
  §1, §2, §4 and §5).
  - `aimem identity enroll issue --purpose new-user --output -` records a
    new-user bundle on the hub and writes its one-line subcode record to
    standard output, for a pipe into aicrew's console
    (`aicrew invitation issue --bundle -`). `--output -` is the only
    destination, refused on a terminal. The bundle ID and the command
    that revokes it go to standard error before the hub is asked.
  - The subcode is `aes1_` and 256 random bits, stored only as a digest.
    It expires after 24 hours by default (`--expires`, at most 72 hours).
    The record carries the hub's URL and the trust the issuing command
    used (pin, CA file or system roots).
  - `aimem identity enroll revoke --bundle-id ID` revokes a bundle that
    was not redeemed. A redeemed one names the user and token it issued,
    which the operator revokes separately. `aimem identity enroll list`
    shows bundles and their states, never a subcode.
  - The routes are `POST` and `GET /v1/identity/enrollments` and
    `POST /v1/identity/enrollments/{bundle_id}/revocation`: hub admin
    only, over TLS the hub terminates, with `X-Aimem-Enrollment-Version:
    1`. An ordinary token gets `credential_scope_forbidden`, and a peer
    credential `peer_forbidden`.

- **A clean installation redeems its enrollment subcode** (D1,
  enrollment.v1 §3 and §5). `aimem enroll redeem --hub-name NAME --label
  LABEL`, which aicrew-agent join runs with the subcode record on standard
  input, stores one user-scoped credential as hub NAME's individual
  credential.
  - The hub (`POST /v1/identity/enrollments/redemptions`, hub TLS, no
    bearer: the subcode is the authority) creates one user with no grant
    and one user-scoped token in one transaction, and returns it sealed
    with HPKE (X25519, HKDF-SHA256, ChaCha20-Poly1305) to the client's
    key. The bearer is stored only as a digest and travels only in the
    ciphertext.
  - An identical retry replays for an hour, also past the subcode's
    expiry; another key is `enrollment_conflict`; every other refusal is
    the same `enrollment_invalid`. At most 10 requests per minute per
    address and 20 per subcode (`rate_limited`).
  - The client verifies the hub only against the record's trust (pin, CA
    or system roots), keeps its delivery key and request key in owner-only
    pending state, and resumes after a lost reply or a crash: a rerun
    replays with the same keys, and one that finds the credential stored
    verifies it without spending anything. Exit codes 0, 3, 4, 5 and 2, as
    the reservation commands use them.

### Changed

- **`aimem enroll redeem` follow-ups.** Two runs started at once for one
  bundle now share one set of keys: the first creates the pending state
  exclusively, and the other resumes with its keys, so a redemption that
  reached the hub stays recoverable. A first run stops retrying at the
  record's expiry with outcome unknown (exit 5); the rerun is a recovery,
  which the hub replays. The redemption route's OpenAPI operation declares
  `security: []`, so schema consumers do not send a bearer.
  A run that stopped between creating the pending state and writing it
  left an empty file that blocked every later run. Runs now create the
  pending state under a lock that the OS releases when its holder exits,
  so an empty file found under the lock has no live writer. A later run
  recovers it once it is a minute old; a younger one is never touched,
  and the run asks to be repeated.

- **A repeated enrollment issue is classified before its input is
  validated.** An identical retry of `aimem identity enroll issue`'s
  request after its requested expiry answered `invalid_request`, which
  read as if nothing had been recorded. Now a repeated bundle ID always
  answers `enrollment_exists` for the same input and
  `idempotency_conflict` for other input (enrollment.v1 §1, corrected to
  say so); only a new bundle's input is checked.

- **hub.json is owner-only on Windows.** It holds every hub's checkpoint
  token and individual credential, including an enrolled installation's.
  It was written with a file mode only, which on Windows sets no ACL, so it
  inherited its folder's access; a local group's read access was observed
  on a development installation. Every save now writes it atomically with
  a protected owner-only DACL (0600 on Unix, as before). `aimem hub` warns
  when the file is not private, and `aimem hub repair` rewrites it.

- **A hub without a checkpoint token gets no journal push.** An installation
  enrolled through D1 holds only its individual credential, and journal
  push needs the hub's checkpoint token. Before this, every checkpoint
  would be refused and spooled forever. Now the push is skipped, with one
  note per run. Docs push still needs the checkpoint token too.

- **Building from source needs Go 1.26.** `go.mod` now says `go 1.26.0`,
  and CI and the release workflow build with the newest Go 1.26 patch.
  Enrollment delivery (D1) uses the standard library's `crypto/hpke`,
  which Go 1.26 added. Each CI job and the release build check that they
  run the runner's own Go 1.26, never a toolchain that `go.mod` would
  download. [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) has the toolchain
  rules. The release binaries are static, so installing from a release
  still needs no Go at all. A source build with an older Go fails under
  `GOTOOLCHAIN=local`, and with the default `GOTOOLCHAIN=auto` it
  downloads Go 1.26 first.

### Upgrade notes

- **Make an existing hub.json private on Windows.** The next hub change
  rewrites it owner-only; to do it at once, run `aimem hub repair`, which
  rewrites the file and changes nothing else (the same hubs and the same
  default, or none). `aimem hub` lists the hubs and warns while the file
  is not private.

- **Access schema 7.** On first start the hub adds the enrollment ledger
  (one new table). Nothing else changes, and every existing row is kept.
  An older binary refuses the migrated store, so roll a hub back only
  from a backup taken before the upgrade.

## [0.8.0] — 2026-10-05

The first-pilot prerequisites
([`docs/DESIGN-AIFORGE-PILOT-1.md`](docs/DESIGN-AIFORGE-PILOT-1.md)):
a project owns its one repository, aicrewd registers and reads its own
teams, a team's coordinator triages tasks, and the admin commands name
every entity by a flag and write issued secrets to `--output`. Also: the
`writing_rule` tool, one socket per state root, and tests that cannot
reach a developer's real service.

The access schema moves to 6, one way: read the upgrade notes before
upgrading a hub. Includes PR #168 to #182, except the unmerged #171.

### Added

- **One rule keeps the text agents keep readable.**
  [`docs/WRITING-PERSISTED-TEXT.md`](docs/WRITING-PERSISTED-TEXT.md)
  covers:
  - its scope: the exact text sent to a write tool (task fields and
    comments, documents, memories, records, team messages and reasons)
    and the Markdown files agents save;
  - what it requires: complete, normally spaced sentences, named actors,
    explained abbreviations, and code, paths and IDs kept exact;
  - that shortening by compression is never allowed, even under time or
    context pressure;
  - a quick check before each write, invented examples and a review
    checklist.

  It complements the `oh-technical-writing` skill instead of competing
  with it. It is delivered:
  - by the new `writing_rule` MCP tool, on the personal and hub endpoints
    and in team conversations. It returns the rule with its version,
    digest and terminator line.
  - by one fixed sentence at the end of every write tool's description,
    pointing to `writing_rule`. A test classifies every listed tool, so a
    new write tool cannot ship without it.
  - by one line at session start (Claude Code and Codex) for saved
    Markdown, which no tool description reaches.
  - by the team guidance: every role's required set now includes the
    section `writing`, which changes the guidance digest.
- **A project owns its one repository**
  ([`docs/DESIGN-AIFORGE-PILOT-1.md`](docs/DESIGN-AIFORGE-PILOT-1.md) §1).
  - On the hub host, `aimem project repo set --project <p> --kind
    github|gitea|gitlab --url <clone URL> [--access write|read]` sets it,
    and `aimem project repo clear` removes it. The URL is stored as given
    and never fetched, and no default branch is stored. A URL that could
    carry a secret is refused: any password, or a user name on `https`.
  - Every set and clear is kept in the project's repository history, with
    the actor and the values before and after (`project.repository.set`,
    `project.repository.clear`).
  - `GET /v1/projects/{p}/repository` answers the repository and its
    host, with the port when the URL names one (`forge.example.org:3000`),
    so two forge instances on one host name two credentials. An ordinary
    token reads it under its own current grant on the
    project, and a read-only token does not. A team session reads it
    under its profile's grant (below).
  - `aimem project show --project <p>` prints the repository, the process
    pin and the project's grants (users, groups and team profiles) with
    names beside IDs, from the new admin route
    `GET /v1/projects/{p}/grants`.
- **`project_repository`**, an MCP tool on the local server and the hub's
  `/mcp`, reads a project's repository (kind, clone URL, host and access)
  under the caller's own grant. In a team conversation it reads a project
  the team is granted. OpenAPI marks the repository read `x-team-mode`.
- **A team's coordinator triages tasks**
  ([`docs/DESIGN-AIFORGE-PILOT-1.md`](docs/DESIGN-AIFORGE-PILOT-1.md) §4,
  decision (a)).
  - `POST /v1/tasks/{id}/triage` (MCP `triage_task`) is a partial update:
    it moves a task between BACKLOG and READY and sets its `next_action`,
    and leaves every other field as it is. Ordinary write-authorized
    callers and the hub admin may use it too.
  - In team mode, a session whose verified role is coordinator may triage
    and comment on unheld tasks of projects its profile is granted, under
    its own user. Another role gets `role_forbidden`; a task under any
    reservation gets `task_held` (409). The team-mode report says
    `task_write: triage` for the coordinator.
  - A claim on a task that is not READY now answers `task_not_ready`
    (409, not retryable) instead of `reservation_conflict`: the
    coordinator triages it, then begins the step again through aicrew.
  - Tasks have no priority or size fields yet; triage gains them when
    they exist.
- **aicrewd registers and reads its own teams**
  ([`docs/DESIGN-AIFORGE-PILOT-1.md`](docs/DESIGN-AIFORGE-PILOT-1.md) §2,
  §3; identity.v1 "Team registration and read").
  - `team.register` (`PUT /v1/identity/peers/{service}/team-registrations/{team_id}`):
    - creates or renames the calling peer's profile for an aicrew team
      UUID, with a name unique per peer;
    - never touches a grant and never re-enables a disabled profile;
    - refuses with `peer_forbidden` (another peer's team),
      `team_name_taken` (409), `profile_disabled` (403) or
      `invalid_request`;
    - is audited with the old and new name for every outcome.
  - `team.read` (`GET …/team-reads` and `…/team-reads/{team_id}`) returns
    the peer's own teams with their granted projects, each with its
    repository and process pin.
    - A disabled profile reads as `enabled: false` with no projects.
    - An unknown or foreign team is `not_found` (404).
  - Each operation needs a peer credential issued for it
    (`aimem identity cred issue --operation team.register|team.read`), over
    hub-terminated TLS. Both share the read scope's 60 calls per minute
    per credential.
  - A team session now reads `GET /v1/projects/{p}/repository` under its
    profile's grant.
  - `aimem identity team` commands name a team by `--team-name` (resolved
    within `--peer`) or `--team-id`. Grant and revoke print the name and
    team ID they acted on, and listings and `aimem project show` print the
    registered name.
  - `aimem identity team create` is kept for this release with a notice:
    aicrewd now creates and names profiles itself.
- **The `aimem project` namespace and `--project`.** `aimem project list`,
  `project id` and `project drop` are the namespace forms of `projects`,
  `project-id` and `drop-project`, which keep working unchanged. Every
  command that takes `-p` now also takes `--project`; `-p` stays as its
  alias.

### Changed

- **The Windows installer restarts only its own service.** `install.ps1`
  used to stop every running `aimem.exe serve` before restarting the
  scheduled task. With several installations under one OS user, that also
  stopped the others' services. It now stops only the processes running
  this installation's binary or its parked copies, selected by executable
  path.
- **Tests cannot reach a real aimem service or stop one.**
  - In a test binary, the local socket must lie in the temporary
    directory as the state root already must, or the test panics.
  - The test packages that reach the socket clear an inherited
    `AIMEM_SOCKET`, `AIMEM_STATE_DIR` or `XDG_RUNTIME_DIR` that points
    elsewhere. Before this, an inherited socket or Linux's runtime
    directory routed some tests to the developer's own service.
  - CI refuses process stops by name (`pkill`, `killall`,
    `taskkill /IM`, `Stop-Process -Name`) in tests, scripts and workflows.
  - [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) records the rules and the
    2026-10-04 incident behind them, when a stop by name ended a
    developer's installed service.
  - Nothing changes outside tests.

- **`aimem access` and `aimem identity` name every entity by a flag**
  ([`docs/DESIGN-AIFORGE-PILOT-1.md`](docs/DESIGN-AIFORGE-PILOT-1.md) §5).
  - Users and groups are named by `--user-name` or `--user-id` and
    `--group-name` or `--group-id`, exactly one of each. An unknown name
    is refused with the names that exist, and a name held by several
    users with their IDs. `user-add` and `group-add` take the name only;
    `user-set` takes `--user-id` and the new `--user-name`.
  - `access grant add|rm` takes `--project`; tokens are revoked by
    `--token-id`; `token-issue` takes `--project` or `--read-only`.
  - `aimem identity` commands with several entities take `--peer`,
    `--credential`, `--team-id` and `--project`. A command with one entity
    (`peer check`, `cred list`, `cred issue`, ...) still takes it as its one
    argument, or as `--peer`. The revoke commands the CLI prints use the
    new form.
  - Each `--help` shows full examples.
- **An issued secret is written to `--output <file|->`**
  ([`docs/DESIGN-AIFORGE-PILOT-1.md`](docs/DESIGN-AIFORGE-PILOT-1.md) §6),
  in `aimem identity cred issue|rotate` and `aimem access token-issue` and
  `token-issue-user`. A file must not exist and is created readable only
  by its owner before anything is issued. `-` writes to standard output
  for a pipe and is refused when standard output is a terminal; every
  other line then goes to standard error. `token-issue` with `--output`
  prints the token's record without the secret.
  `aimem teams provision create|add` take `--output` too, for the member
  token they issue; the file is claimed before any step runs, and a run
  that issues no token removes it again.

- **Tests cannot reach a real hub through the client state root.** The
  CLI and the MCP server now resolve the state root in one place
  (`adapter.StateRoot`). In a test binary it fails closed without an
  isolated `AIMEM_STATE_DIR`, instead of falling back to the developer's
  home directory. This changes nothing outside tests.
- **Several installations under one OS user each get their own socket.**
  An installation is a state root. With `AIMEM_STATE_DIR` set, the local
  socket now defaults to `<root>/aimem.sock` instead of `XDG_RUNTIME_DIR`,
  so two state roots no longer share one service and one journal on Linux.
  `AIMEM_SOCKET` still wins. A root too deep for a Unix socket path fails
  with a message naming the length and `AIMEM_SOCKET`.
  `docs/DESIGN-AIFORGE-CONTEXT.md` describes the layout and the env-file
  hazard below.

### Upgrade notes

- **Access schema 6.** On first start the hub rebuilds the peer credential
  table to allow the two new operations. Every credential keeps its ID,
  digest, expiry and operation. Team profiles gain an empty `team_name`
  until aicrewd registers them. An older binary refuses the migrated store
  (schema 6 is newer than it understands), so roll a hub back only from a
  backup taken before the upgrade.

- **The old command forms work for this release only.** The positional
  forms of `aimem access` and of the `aimem identity` commands with
  several entities, and `--secret-file` (in `identity cred` and
  `teams provision`), keep working and print a one-line notice on
  standard error naming the new form. So does `teams provision` printing
  the secret on standard output without `--output`. The old
  `token-issue` and `token-issue-user` forms still print the secret on
  standard output; a script that reads it there should move to
  `--output -`.

- **A service started with an explicit `AIMEM_STATE_DIR` moves its socket**
  into that root on restart.
  - Clients that set the same `AIMEM_STATE_DIR` follow it automatically.
  - A client without it still looks in `XDG_RUNTIME_DIR`. Set
    `AIMEM_SOCKET` on both sides to keep the old place.
  - Installs that never set `AIMEM_STATE_DIR` are unaffected.
  - When more than one installation exists, `~/.config/aimem/env` must not
    set `AIMEM_STATE_DIR` or `AIMEM_SOCKET`: aimem folds that file into
    every process without its own value.

- **Rollout of the writing rule.**
  - A machine receives the tool and the description sentence when its
    `aimem` binary is upgraded and the client restarts its MCP server.
  - It receives the session-start line at the next session after the
    upgrade; the hook command is unchanged, so no reinstall is needed.
  - Clients of a hub's `/mcp` endpoint receive the tool when the hub is
    upgraded.
  - Team members receive the `writing` section when they read their role
    set at the new guidance digest. aicrew pins no guidance digest; its
    interim copy of the rule (in its managed role guidance) can now point
    to this section instead.
  - A repository edit alone delivers none of this.

## [0.7.4] — 2026-10-02

AIForge prerequisites: aimem can now serve an aicrew team as a verified,
separate context next to each member's personal use, with no second agent
credential and no embedded team scheduler. The work adds:
- a member's session verified online through aicrew;
- task reads and a pilot set of knowledge reads under the team's own
  access profile;
- reservations that make aimem the single owner record of a task being
  worked, with fences, receipts, and operator recovery;
- coordination facts that bind every team step except a holder's update
  to aicrew's decision.

The wire contracts are in `docs/DESIGN-AIFORGE-*.md`, with fixtures under
`docs/fixtures/`. This is what a first pilot needs from aimem: one
coordinator, one worker, one project.

Also: OpenCode 2 support, and `modernc.org/sqlite` 1.58.0 to 1.59.0 (#55).
Includes PR #55 and #110 to #166, except the unmerged #128.

### Added

- **A hub's private CA or pin from the CLI.** `aimem hub add NAME URL TOKEN
  --ca-file PATH` (or `--pin sha256-BASE64`) records the hub's trust with
  its entry; no hand-edited `hub.json`. Every client path to that hub uses
  it: checkpoint pushes, sync, the MCP server and docs, the read clients,
  the dashboard and team sessions. A re-add without the flag keeps it, an
  explicit one replaces it, and an unreadable CA file is refused, naming
  its path; a trust that cannot be loaded never falls back to the system
  roots or to skipping verification.
- **Hub tokens from a file or standard input.** `aimem hub add` and
  `aimem hub task-token` take the token with `--token-file PATH` (a file
  only you can read) or `--token-file -` (standard input), so no secret
  goes on a command line or into shell history. A trailing LF or CRLF is
  dropped; an unreadable, wider or multi-token file is refused, naming the
  source, never its content.
- **Identity and verified team context** (identity.v1):
  - The hub serves single-use identity proofs and aicrew's proof redemption.
  - `aimem identity` manages the aicrew peer, its credentials and team
    access profiles; `aimem identity proof` asks for a proof.
  - A request carrying `X-Aimem-Team-Context` is verified online through
    aicrew introspection, on every request, and served only under the
    linked team profile's live grant, never the member's personal grants.
    It is served only on the team routes. Every other route refuses it
    with `team_operation_unsupported`, and it is never served as a
    personal request.
  - `GET /v1/access/identity` in team mode is the context report: the
    session, the granted projects, `task_write: false` and
    `knowledge: read`.
- **Team conversations.**
  - `aimem team-session open|refresh|close|status` keeps a session file.
  - An `aimem mcp` started with `AIMEM_TEAM_SESSION` serves that one
    verified context only:
    - the task and epic reads;
    - the reservation tools;
    - the pilot knowledge reads;
    - `session_context`.
  - In a team conversation the capture hooks record nothing, and the
    session start injects only the checkout's handoff.
- **Task reservations** (reservation.v1), the single owner record of a
  task being worked:
  - Routes: claim, transfer, update, release and finalize, with status and
    receipt reads, under `/v1/projects/{p}/tasks/{task_id}/reservation`.
  - Each transition carries a revision, a fence and a request key, and
    returns a committed receipt. A retry replays it, and a lost reply is
    reconciled from the receipt.
  - Generic task writes cannot bypass a hold.
  - A claim requires every same-hub dependency to be readable and DONE.
  - MCP tools `task_reservation_*`, and `aimem reservation` for members:
    the body on standard input; exit codes 0, 3 (final), 4 (retryable)
    and 5 (outcome unknown).
- **Coordination** (coordination.v1). Every team transition other than a
  holder's update is backed by a single-use aicrew proof and verified
  against aicrew's coordination fact before it commits:
  - the offer, the worker's accepted attempt, the release of an offer
    never accepted or of stopped work, and the independent claim;
  - the acceptance for finalization, whose `evidence_digest` binds the
    exact delivery evidence the coordinator confirmed;
  - the fact's process pin, which must be the project's current process
    selection.
- **Operator recovery.**
  - `aimem reservation recover` releases or cancels a hold its holder
    cannot close, on aicrew stop evidence or an attestation. Every
    request is audited.
  - A recovery reader and receipt lookup serve the operator.
- **aicrew's read-only scope** (a `reservation.read` credential): a
  receipt by proof, a receipt by request key, and a hold's status with
  closure evidence (`closed_by`, `closing_fence`, `closed_at`).
- **Actionable refusals.**
  - Reservation and team refusals share one envelope: `{code, message,
    active_mode, denied_action?, active_role?, retryable, next_action,
    correlation_id}`.
  - Each refusal gives one next action, chosen by code, role and
    operation, and never advises another credential, a personal-mode retry
    or a bypass.
- **The first pilot's knowledge reads.** `recall_memory`, `list_docs` and
  `read_doc` (recall, the document list, a document's current revision)
  are served to ordinary user tokens:
  - in team mode, under the team profile's live grant;
  - personally, under the token's own direct or access-group grant.

  In both modes:
  - the personal store and group spaces are refused;
  - `?rev=` is refused, since a scoped caller reads the current revision
    only;
  - every other knowledge route stays refused.

  In team conversations and on the hub's `/mcp` the three tools use the
  caller's own authority and refuse any scope but the project.
- **`aimem hub credential [<name>] [--json]`** reports whether this
  installation holds an individual credential for a hub (`set` or `none`),
  and, when set, the hub's answer for it: `active` with its scope and user
  and token IDs, `refused`, or `unreachable`. It prints no part of any
  secret. aicrew's client bootstrap checks it before an invitation attempt.
- **OpenCode 2 support.**
  - The OpenCode plugin (`aimem.ts`) now loads on both OpenCode 1.x and
    2.x. OpenCode 2 rejects 1.x plugins, so the same file also carries a
    2.x implementation built on the new plugin API.
  - On OpenCode 2 it journals turns, failures and compaction markers, and
    adds the `AIMEM HANDOFF:` line to compaction summaries, as on 1.x.
  - OpenCode 2 accepts but ignores opencode.json `instructions`. For
    projects that list `docs/SESSION-STATE.md` there, the plugin adds the
    handoff to each model request itself.
  - OpenCode 2 has no toast API, so the context warning is given to the
    model instead of the user.
- **The license travels with every release.** `LICENSE` is attached as a
  release asset and listed in `SHA256SUMS`. The release notes end with the
  license URL and the `Required Notice:` lines, and the release workflow
  stops if `LICENSE` has none. The installers check only their own binary's
  line in `SHA256SUMS`, so they are unaffected.

### Removed

- **`docs/aimem.service`**, an early hand-written systemd user unit that
  nothing referenced. Its `ExecStart` pointed at a developer's local path,
  so following its instructions produced a broken unit. The installers
  (`install.sh`, `install-hub.sh`) write the real units.

### Upgrade notes

- **Schema migrations, one way.**
  - Each project database moves from schema 18 to 23: the reservation
    ledger, its receipts, dependency and coordination records.
  - The access database moves from 2 to 5: identity proofs, peers and
    their credentials, team profiles and grants.
  - A binary refuses a database newer than it understands, so a hub
    cannot be downgraded after it has run this release. Back up the state
    root first.
- **Existing tasks and tokens keep working.** Protected tasks of the
  legacy team runtime cannot be claimed as reservations. While a
  reservation holds a task, generic task edits of it are refused
  (`409`); the holder changes it through the reservation.
- **Ordinary user tokens gain three reads:** recall, the document list and
  a document's current revision, in projects they hold a live grant on.
  Read-only-scope tokens gain nothing. Legacy writer and admin tokens, the
  local socket and the personal stdio MCP are unchanged.
- **Reservation transitions obey the project's task switch.** On a project
  whose tasks are off, every reservation transition is refused with
  `tasks_disabled`, operator recovery included. Holds survive a switch off
  and back on unchanged.
- **Identity routes and team mode require TLS terminated by the hub
  itself** (`AIMEM_TLS_CERT`, `AIMEM_TLS_KEY`). Plain HTTP and a
  TLS-terminating proxy get `tls_required`, whatever forwarded headers
  claim, and the local socket serves neither.
- **Agent machines need the new binary as well as the hub.** The
  team-conversation MCP, `aimem team-session` and `aimem reservation` are
  client-side.
- **OpenCode 2 plugin:**
  - Re-run the user-level install (`install.sh user` / `install.ps1`) to
    refresh `~/.config/opencode/plugins/aimem.ts`; the previous copy does
    not load on OpenCode 2.
  - No config change is needed: the existing opencode.json wiring
    (`instructions`, `mcp.aimem`) works on both generations.
  - `AIMEM_AUTO_COMPACT` works on OpenCode 1.x only; OpenCode 2 plugins
    cannot request compaction. Use OpenCode 2's own `compaction` settings.
  - On the 1.x line the plugin now supports OpenCode 1.18.0 or newer.
    Releases before 1.14 cannot load it at all. They call every plugin
    export as a function and fail on the default object that OpenCode 2
    requires, and OpenCode itself stops with "Unexpected error" (observed
    on 1.1.4).
  - The user-level installers (`install.sh`, `install.ps1`, and so the
    one-liners) check `opencode --version`. Below 1.18 they keep the
    installed plugin and print a warning instead of replacing it: upgrade
    OpenCode, then re-run the install.

### Configuration for an aicrew team

Nothing here is on by default: a hub without it serves no team request.
Run the `aimem identity` commands against the hub's TLS listener with a
hub-admin token file (`--hub https://HOST:PORT --admin-token-file PATH`).

1. **Register aicrew as the identity peer.**
   `aimem identity peer register SERVICE --endpoint URL` with
   `--peer-trust-dns` or `--peer-trust-pin`. `URL` is aicrew's
   `/v1/crew/introspect` route.
2. **Give the hub its outbound introspection credential.** aicrew issues
   it. Put it in a private file and name that file in
   `AIMEM_INTROSPECTION_TOKEN_FILE` for the hub service. Verify with
   `aimem identity peer check SERVICE`.
3. **Issue aicrew's two credentials** with
   `aimem identity cred issue SERVICE --expires ... --secret-file PATH`:
   - one with `--operation identity.redeem` (the default) for proof
     redemption;
   - one with `--operation reservation.read` for the read-only
     reservation scope.

   Each bearer is written once to its secret file. Deliver it to aicrew's
   protected storage, then delete the file.
4. **Create the team's access profile and grant it the pilot project:**
   `aimem identity team create SERVICE TEAM`, then
   `aimem identity team grant SERVICE TEAM PROJECT`. A team session reads
   only the projects its profile is granted, checked live on every
   request.
5. **Give each member a user-scoped token**
   (`aimem access token-issue-user USER LABEL EXPIRY`), linked to their
   aicrew membership through a single-use `aimem identity proof`, which
   aicrew's client drives. A project-scoped or read-only token cannot
   enter team mode. When the hub's certificate is not in the system roots,
   each member records its CA or pin with the hub entry
   (`aimem hub add NAME URL TOKEN --ca-file PATH` or `--pin`).
6. **Serve TLS from the hub itself** (`AIMEM_TLS_CERT`, `AIMEM_TLS_KEY`).
   Team mode refuses TLS terminated by a proxy.
7. **On the pilot project:**
   - enable tasks (`aimem tasks on -p PROJECT` on the hub host);
   - select its process
     (`aimem process select REPO COMMIT MANIFEST -p PROJECT`): a
     coordinated claim needs the process pin.

### Lockstep with aicrew

A coordinated finalize now requires `evidence_digest` on aicrew's
`accepted_for_finalization` fact. This was the last in-place amendment of
coordination.v1 before the pilot.

- **Newer aimem, older aicrew:** the aicrew fact lacks the digest, so
  aimem refuses it as the wrong shape (`context_unavailable`) and applies
  nothing.
- **Older aimem, newer aicrew:** aimem refuses aicrew's added field.

Deploy this aimem together with an aicrew that sends the digest, or deploy
aicrew first. Every other transition is unaffected.

## [0.7.3] — 2026-09-24

Portable team context: a member joining from any project's checkout now
receives the team guidance and the project's selected process through MCP,
with an explicit readiness report, instead of being sent to a playbook file
that exists only in an aimem checkout. A worker whose context is missing is
announced unavailable and does not start, continue or resume work; an
accepted attempt keeps the process version it was accepted under. The
`/join_team` and `/resume_team` entry points are regenerated to use all of
it. A hub that refuses the checkout's credential is no longer mistaken for
an offline one. Includes PR #102-#108.

### Upgrade notes

- No schema change and no new hub route; hubs on v0.7.1 or later remain
  compatible. Upgrading a hub adds `team_context` to its MCP endpoint;
  nothing in the member flow needs that.
- Upgrade every participating agent machine to v0.7.3. The new entry points
  stop with an upgrade-and-restart message on an older `aimem mcp`.
  Then, in each member checkout, run `aimem teams commands .` (it also
  refreshes the Codex prompts under `~/.codex` when that directory exists)
  and restart the agent clients so their MCP processes load the new binary.
- Select the project's process before a team starts work: with tasks on and
  no process selected, workers are never ready and are announced
  unavailable.
- Upgrade between attempts, not during one. The process version of an
  accepted attempt is recorded only by the checkout that accepted it with
  this release; an attempt accepted before the upgrade, through the hub
  endpoint or from another checkout has no record, so its worker is not
  ready when it resumes and a running attempt is blocked, with no
  automatic resume.
- Missing context blocks running work, including a temporary outage, and a
  blocked attempt stays blocked until the coordinator answers. Blocking an
  attempt is a coordination record: it does not stop a process on the
  worker's machine.
- Real-client validation (Claude Code, Codex and OpenCode on Windows, Linux
  and macOS) is still pending; the release is covered by fixture tests
  through the real local MCP only. See the
  [v0.7.3 rollout and pilot checklist](https://github.com/BlackVS/aimem/blob/master/docs/TEAM-ROLLOUT-0.7.3.md).

### Added

- Readiness in `team_setup` and `team_continue` (and `aimem teams setup` /
  `continue`): for a verified session the report carries a `readiness`
  object (membership, role guidance, project process, and execution, which
  is always `not_verified` because nothing here can observe the agent's own
  shell or runner) and `ready_for_work`. The role's complete guidance and
  the complete selected project process are delivered in the same result:
  as their own MCP text blocks after the unchanged JSON report, printed
  after the CLI report, under `delivered` in `--json`, each ending with its
  terminator. A changed guidance digest since the last delivery is
  reported. Delivery is not recorded as acknowledgement.
- A worker that is not ready no longer only lists the attempt the hub holds
  for it: `team_setup` and `team_continue` decline an `OFFERED` attempt
  (including one that raced the join) with the readiness reason, block a
  `RUNNING` one with the missing context (reading the task's current
  revision; the attempt stays reserved), leave `BLOCKED` as it is, and send
  nothing for `STOP_REQUESTED`, `STOPPED` or `SUBMITTED`; a stop is never
  acknowledged for the worker. Each write is recorded with its retry key
  and exact content before it is sent, repeated identically only while the
  attempt is unchanged, and never replayed across a new session generation
  or once the worker is ready. The reserved attempt's next step no longer
  tells a worker that is not ready to accept its offer.
- An accepted attempt keeps the process version it was accepted under:
  `team_accept` on the checkout-bound local MCP and `aimem teams accept`
  record the attempt's exact process commit and manifest and the team
  guidance digest in the checkout's nonsecret team state before sending
  the accept, and a retry reuses that record. `team_setup`, `team_continue`
  and `process_context` then deliver that exact version, marked PINNED and
  with the usual live checks, for as long as the attempt is reserved; a
  newer project selection is named but applies to new work only. An
  accepted attempt whose version was not recorded here or cannot be
  recovered is not ready (a `RUNNING` one is blocked); none is assumed.
  A worker run that cannot read its reserved attempt delivers no project
  process and is not ready, since the version that applies is unknown.

### Changed

- The generated entry points stop on a `blocked` report even when context
  blocks are attached (a coordinator whose inbox read failed after its
  context was delivered): attached context does not authorize proceeding.
- The generated `/join_team` and `/resume_team` entry points (every client:
  Claude Code, OpenCode, Codex skills and prompts; refreshed by `aimem
  teams commands` and by every `aimem teams setup`) use the delivered
  context instead of `docs/TEAM-PLAYBOOKS.md`, a path that exists only in
  an aimem checkout. Step 0 also requires `team_context` and
  `process_context`: when the server lacks one, the agent tells the user to
  upgrade aimem and restart the client and stops, with no shell fallback.
  The agent reads `readiness` before any role step, checks each delivered
  block's terminator against the readiness version and digest, re-reads a
  cut or stale block with `team_context` or `process_context` and treats
  itself as not ready until it holds both complete, follows the report's
  next steps where they differ from the entry point, heartbeats `available`
  only while ready, never resumes a blocked attempt because readiness came
  back, and never infers its own execution environment from the MCP server.
  The Claude Code skills pre-approve the two read tools. The `team_setup`
  next steps name the delivered guidance and `team_context` sections
  instead of the repository path.
- `team_accept` from a checkout (the local MCP tool and `aimem teams
  accept`) is refused locally, with nothing sent, when the process version
  cannot be recorded: no saved membership for the session in the request,
  or a project process that is not ready. An attempt accepted before this
  release, or through the hub endpoint, has no record: `team_continue`
  reports it as not ready and blocks it if it is running, so upgrade
  members between attempts.
- A worker whose role guidance or project process is not ready is now
  announced `unavailable` by `team_setup` and `team_continue`, instead of
  `available`, so the hub neither offers it work nor lets it accept any. In
  a project with tasks on but no selected process, workers are therefore
  never ready: select the process (`aimem process select` on the hub host)
  before a team starts work. A coordinator that is not ready is told to
  issue no offers. The heartbeat failure message now names the
  availability that was sent.

- `process_context` on the checkout-bound local MCP: the project's selected
  process as one complete unit (what `aimem process show --full` prints)
  or one template by kind, read with the checkout's own credential and no
  fallback, from the hub's selection and this machine's exact-commit cache
  or Git at that commit. It takes an optional template kind and nothing
  else (no path, URL, repository, commit or project), is neither listed nor
  served by the hub endpoint, and ends every delivery with a terminator
  naming project, commit, manifest and the SHA-256 of the text. A unit over
  32 KiB is refused whole. It delivers in state `ready`, or `last_observed`
  (hub unreachable; the exact cached commit of the last observed selection,
  marked as such); the other states are errors naming the state, cause and
  fix: `disabled`, `not_selected`, `denied`, `unavailable`, `too_large`. The states come from a typed
  `processctx.Load`; the session-start hook, `aimem process show` and
  `team_setup` keep their text. `team_setup` and `team_continue` deliver
  the same unit with their readiness report (above).

- Team guidance built into the binary, for agents without an aimem
  checkout or a working shell: the team playbooks and request templates
  are embedded from their canonical files in `docs/` (no copy), split into
  sections with stable ids, and served with the unit's build version,
  protocol compatibility and a SHA-256 digest over the manifest and every
  section. The `team_context` MCP tool, on the local and the hub endpoint
  for any caller the endpoint already admits, returns a role's complete
  required set (`role`), one section (`section`) or the index; it needs no
  session, checkout, project grant, network or shell, changes nothing, and
  takes no path, URL or command. `aimem teams context` is the same read on
  the command line. Every read ends with a terminator line so a result the
  client cut is recognizable. The build fails when a section exceeds
  16 KiB, a role's set 32 KiB or the unit 128 KiB, when a heading has no
  stable id, or when a relative link neither resolves inside the unit nor
  is listed as informative. `team_setup` and `team_continue` deliver the
  role's set with their readiness report (above);
  [design](https://github.com/BlackVS/aimem/blob/master/docs/DESIGN-portable-team-context.md).

### Fixed

- A hub that refuses the checkout's credential (HTTP 401 or 403) no longer
  makes the session-start process context fall back to the last observed
  selection and its cached handbook, as if the hub were offline: the hook
  now reports the refusal and uses nothing cached. Only a hub that cannot
  be reached (a transport failure, or a gateway answering 502, 503 or 504)
  lets the last observed selection stand in, as before; any other hub
  error is reported as unavailable.

## [0.7.2] — 2026-09-23

Team join and resume now use the local MCP process, so agents can use
their existing checkout credentials even when shell commands run under
a different sandbox account. Includes PR #99 and PR #100.

### Upgrade notes

- No schema change or new hub route; v0.7.1 hubs remain compatible.
- Upgrade participating agent machines to v0.7.2, run `aimem teams commands .`
  in each member checkout, and restart agent clients so their MCP processes
  load the new binary. Existing project tokens do not need replacing.
- Live Windows, Linux and macOS pilot validation remains pending. This
  release does not fix client-specific shell-runner failures.

### Added

- Team onboarding over the checkout-bound local MCP server: `team_setup`
  and `team_continue` run the same code and saved state as `aimem teams
  setup` and `continue` from the `aimem mcp` process the client starts in
  the checkout as the credential's owner, so a shell the client sandboxes
  under another account is not needed. They take the team, role, a declared
  profile and explicit recovery choices, no token, checkout or command;
  report HEAD but never probe the working tree; write no integration files
  unless asked (`repair_integration`); and are absent from the hub's MCP
  endpoint. The `/join_team` and `/resume_team` entry points call them and
  stop with an upgrade-and-restart message when an older `aimem mcp` does
  not list them, never falling back to a shell command.
  [Design](docs/DESIGN-team-onboarding-mcp.md); step-by-step
  [team quickstart](docs/TEAM-QUICKSTART.md).

### Changed

- The session-start process bootstrap moved into `internal/processctx`,
  shared by the CLI and the MCP facade; `aimem session-start`, `aimem
  process show` and `aimem teams setup` behave as before.
- `team_leave` through the stdio MCP facade clears the saved membership of
  the checkout the facade is bound to (before: the process working
  directory).

- Team onboarding runs on one shared core (`internal/teamsetup`): `aimem
  teams setup` and `aimem teams continue` are now shells over `Run` and
  `Continue`, which take the checkout, state root, hub session caller and
  probes from the caller and nothing else. This is the first step of
  [checkout-bound onboarding over local MCP](docs/DESIGN-team-onboarding-mcp.md);
  CLI behavior, reports, saved state and retry keys are unchanged, and the
  JSON report gains `run_as` (the OS account the run held the credential as).
- Credential failures are classified (`taskcred.Classify`): a credential
  this process cannot read or decrypt is reported as belonging to another OS
  account, with the fix pointing at the owner-context path (the checkout's
  local MCP process) rather than at replacing the token; only a malformed or
  rebound credential is told outright to run `aimem task-token set`, and a
  missing one is reported with the state root and account the process runs
  as, since another account's state root looks the same as nothing installed.

## [0.7.1] — 2026-09-23

One-command team onboarding for the pilot clients: `aimem teams setup`
verifies a checkout and joins or reconciles a team session, `/join_team`
and `/resume_team` entry points for Claude Code, OpenCode and Codex are
rendered from one source, members list their own teams before joining,
`aimem teams continue` restores a member's duties after a restart, and
`aimem teams provision` creates a team and its members on the hub host
without hand-written JSON.

### Data integrity

- **No schema change.** The project schema stays at 18 and the access
  database at 2; a hub or client on 0.7.0 opens the same databases. Upgrade
  the hub before the agent clients: `GET /v1/projects/{p}/teams/mine` is a
  new route, and a 0.7.1 client on a 0.7.0 hub reports the enrollment check
  as unavailable and lets the join decide as before. Restart agent sessions
  after upgrading a client: a running `aimem mcp` keeps the old binary and
  does not show `team_list` until it restarts.

### Added

- `aimem teams provision create PROJECT --team NAME --coordinator USER
  --expiry RFC3339` and `aimem teams provision add PROJECT --team NAME|ID
  --member USER --role worker|coordinator --expiry RFC3339`: the guided
  operator path on the hub host, replacing hand-written team JSON and setup
  scripts. Each run resolves the user by exact name or ID (`--create-user`
  for a new one), sets the project grant, creates the team with its first
  coordinator or adds the member to the existing enrollment at the read
  revision (one retry on a conflict; unrelated enrollment kept; a worker
  request never removes a coordinator flag), and issues one project-scoped
  token labelled `team-<team>-<user>` whose secret is shown once at the end
  or written to `--secret-file` (new file, 0600). Every step is reported as
  existing or created; reruns duplicate nothing and never reissue a live
  token with the same label (one that cannot authorize this project, such
  as another project's, is refused as a label collision); a disabled user
  is refused by name or ID before anything is granted; `--no-token` enrolls
  a member that has one.
  `aimem teams setup` prints these commands as its operator handoff.

- `aimem teams continue [TEAM] [--fence] [--json]` and the `/resume_team`
  entry points (Claude Code skill, OpenCode command, Codex skill and home
  prompt, rendered by `aimem teams commands` like `/join_team`): after a
  client restart the saved membership is verified or resumed (persisted
  retry key; a live session is only verified, `--fence` resumes it) and the
  duties the hub holds are reported: the reserved attempt with its
  assignment title and state mapped to the next protocol step, a
  reconciliation line (HEAD against the base commit recorded at the last
  verification, uncommitted changes, surviving processes, retry only with
  the original key), the unacknowledged inbox listed at cursor 0 with the
  next cursor and nothing acknowledged, and the roster. It never joins: a
  membership that ended is reported and `/join_team` is the way back in.
  `aimem teams setup` shares the richer role entry. The saved membership
  state moved to `internal/teamstate` and records the base commit; an
  explicit `team_leave` through the stdio MCP facade now clears it like the
  CLI leave does, so a closed handle after a leave is told apart from a
  stale one.

- Members list their own teams before joining: `GET /v1/projects/{p}/teams/mine`
  (ordinary write token, current grant, tasks enabled; read only, no session,
  nothing recorded, other members not disclosed) returns the teams enrolling
  the caller with `coordinator` (may take the slot) and `coordinator_active`;
  exposed as the `team_list` MCP tool and `aimem teams mine PROJECT`. `aimem
  teams setup` uses it to report "not enrolled", "enrolled elsewhere" (naming
  those teams) and "enrolled but not coordinator-eligible" before the join,
  each with the operator handoff; on an older hub it says so and the join
  decides as before. New route: requires a hub upgrade to be reachable.

- `/join_team TEAM [worker|coordinator]` entry points for Claude Code
  (`.claude/skills/join_team/SKILL.md`), OpenCode
  (`.opencode/commands/join_team.md`) and Codex (`.agents/skills/join-team/
  SKILL.md`, mentioned as `$join-team`, plus `~/.codex/prompts/join_team.md`
  for `/prompts:join_team` when a Codex home exists). One text, rendered by
  the binary per client and written as managed files by the new `aimem
  teams commands [DIR] [--check]`, by the installers' project mode, and by
  every `aimem teams setup` run; a file without the managed marker is never
  touched. The text has the agent declare its platform and only a
  runtime-reported model, run `aimem teams setup --json`, stop on a blocked
  report, and enter the role per the playbooks.

- `aimem teams setup` checks the checkout's client integration before
  joining and repairs only what aimem owns: `docs/SESSION-STATE.md`, the
  `SessionStart` handoff hook in `.claude/settings.json` and
  `.codex/hooks.json` (either installer spelling counts), `mcpServers.aimem`
  in `.mcp.json`, and the handoff instruction plus `mcp.aimem` in
  `opencode.json`. Missing entries are added in the installers' shapes
  (BOM-free, other keys kept); an entry that differs is reported, never
  replaced; an unreadable file is never overwritten; project-level `Stop`,
  `StopFailure` or `PreCompact` hooks block the run (`--allow-project-stop-hooks`
  overrides); `--no-repair` reports only. The report also lists the agent
  clients on PATH (`--client-versions` adds their versions) and, per client,
  where each skill the selected process requires is found (a directory with
  `SKILL.md` in a location that client reads) or that it is NOT FOUND.
  New package `internal/wiring`.

- `aimem teams setup TEAM <worker|coordinator>`: verified team onboarding
  from a configured checkout in one command. It checks the strict project
  binding and the selected credential (no fallback), the credential's
  identity (ordinary user, scope, current write grant, tasks enabled), the
  hub version and team protocol, the selected process and its required
  skills, and the Git base commit; then it joins the team or recognizes the
  session the checkout already holds, and ends in the role entry (roster for
  a coordinator; availability, reserved attempt and one bounded inbox read
  for a worker). Nonsecret session state lives under the state root in
  `team-sessions/`, bound to checkout, project, hub, URL and token ID, so a
  repeated run verifies instead of joining twice, a suspect session is
  resumed (`--resume` forces it, `--new-session` discards the handle), a
  stale handle is reported rather than taken over, and an unconfirmed join
  or resume replays with its original key. Refusals map to the exact fix,
  including a bounded operator handoff for a missing grant, team or
  enrollment. `--json` prints the report for command wrappers.
  (`mcp.TeamRequestIn` exposes the same credential path with raw hub
  responses; the request builders were split from the MCP forwarders.)

### Docs

- README and INSTALL-CLIENT: explain the empty-body failure of the Windows
  one-liner (`irm` returning an empty string) and document running
  `install.ps1` / `install.sh project` from a checkout, the path that
  skips the download and wires further projects once aimem is installed.

## [0.7.0] — 2026-09-22

The agent-team coordination protocol for a manual pilot: registration and
roster, durable messaging, fenced assignments, execution and results,
coordinator handoff, operator recovery and token replacement, lifecycle
inbox delivery, audit export, playbooks with validated templates, and a
scripted rehearsal of the pilot scenarios. The entries below are in the
order they landed; where an earlier entry says a capability "remains
deferred", a later entry in this same release delivers it.

### Data integrity

- **Five project schema bumps since 0.6.0: 14 (team and audit tables), 15
  (team sessions), 16 (durable team messages and inboxes), 17 (exclusive
  assignments and managed-task protection) and 18 (managed-task lifecycle
  flag).** Each step adds tables, indexes or one column and rewrites no
  existing rows, but a database opened by this build is refused by older
  builds. Back up the hub's state directory before upgrading and keep the
  previous binary for a rollback from that backup. The access database
  schema stays at 2.
- **Upgrade the hub before any agent client** that will join a team: the
  team, message, assignment and audit routes exist only on this build, and
  clients refuse to emulate them through generic task writes. A session must
  restart to see the new MCP team tools.

### Added

- Team protocol rehearsal: a scripted fixture drives a coordinator and two
  workers as three principals through the pilot scenarios (collision, restart,
  admin handoff, token revocation and replacement, duplicate commands,
  cross-project denial, legacy mutation, integration-base change, operator
  recovery) over the real MCP wire and admin routes on an isolated hub and
  checks the audit export; `docs/TEAM-REHEARSAL.md` records what it proves
  and what the manual pilot must still cover.

- Team playbooks: coordinator and worker guidance for joining, waiting,
  assignment choice with recorded rationale, evidence-backed asynchronous
  question resolution, human escalation, results and review
  (`docs/TEAM-PLAYBOOKS.md`), with request templates under
  `docs/examples/team` that a test drives through the hub's validators.
  The escalation expiry fallback names the hub's release sequence for
  accepted work (cancel, stopped, close-stop); decline stays for offers.

- Team audit timeline and export: the admin events page accepts session, task,
  attempt, operation and time-range filters, and a new admin export route
  streams JSONL pages of accepted events and message metadata (delivery and
  acknowledgement per recipient; bodies opt-in) at one snapshot with a schema
  header and completion marker; `aimem teams export` follows the pages.
  Unknown query parameters on the audit routes are now refused.

- Team token replacement: an admin route and operator CLI command rebind a
  session to a replacement token of the same user after recorded
  reconciliation; the generation advances, a reserved attempt follows as on
  resume, the old credential can use no handle and its receipts never replay.
  OpenAPI wording no longer promises inbox delivery for edit and finalize.

- Team lifecycle inbox delivery: every assignment transition and coordinator
  handoff writes a hub-authored `lifecycle` message into the counterpart
  sessions' inbox in the same transaction, with the existing delivery and
  acknowledgement semantics; clients cannot forge one. Responses now
  advertise `workflow_ready:true`; that describes the protocol, not a pilot.
  Handoff lifecycle messages name the acting session and the successor
  separately, and the OpenAPI team route descriptions state the same
  readiness contract.

- Team CLI and MCP parity: `team_offer`, `team_assignment`, `team_reserved`,
  accept/decline/withdraw, block/resume-work/cancel/stopped/close-stop,
  submit/review, handoff, edit and finalize as MCP tools and `aimem teams`
  agent commands bridging to the HTTP routes with the checkout credential;
  operator `aimem teams recover` and `aimem teams unmanage`.

- Team management HTTP routes: coordinator handoff (current coordinator, or
  admin with reconciliation), managed-task edit and finalize for the
  coordinator, and admin-only recover and unmanage, pinned in OpenAPI with
  their roles. Session-scoped assignment reads now refuse malformed query
  encodings. CLI/MCP execution tools and inbox lifecycle delivery remain
  deferred; responses still advertise `workflow_ready:false`.

- Team execution HTTP routes: reserved-attempt read, block/resume-work,
  cancel/stopped/close-stop, submit and review under the assignment routes'
  ordinary-token authority, pinned in OpenAPI. Handoff, edit, finalize,
  recovery and unmanage routes, CLI/MCP execution tools and inbox lifecycle
  delivery remain deferred; responses still advertise `workflow_ready:false`.

- **Project schema 18:** managed-task lifecycle storage. The coordinator edits
  managed task content under revision checks between attempts and finalizes an
  accepted attempt's task to DONE only with recorded merge/delivery evidence;
  an operator releases a task from team management with an audited unmanage,
  after which generic writes work again and a later offer manages it afresh.
  The migration adds one flag column; older binaries refuse schema 18. Public
  edit/finalize/unmanage commands remain deferred.

- Coordinator handoff storage: the current coordinator, or an admin with
  recorded reconciliation after a loss, transfers the slot to an active
  designated session with no reserved attempt in one transaction; the old
  handle is stale, existing attempts survive, and one audit event records the
  transfer. Public handoff commands and token replacement remain deferred.

- Worker session rebinding storage: a worker resume carries its reserved
  attempt to the new session generation in one transaction, with bounded rebind
  history, a correlated audit event and a reserved-attempt read. Offers, leave
  and coordinator resume are unchanged; public exposure of the read is deferred.

- Operator recovery storage: reconciled forced closure of abandoned execution,
  guarded by task and session/coordinator generations, with immutable evidence
  and atomic requeue. Public recovery commands and process control remain deferred.

- Cooperative team work-control storage: worker block/resume, coordinator stop
  request, worker acknowledgement and coordinator closure. Reservations remain
  held through STOPPED; only closure requeues the managed task. Public commands,
  forced recovery and process control remain deferred.

- Team result storage: immutable repository candidates, coordinator acceptance
  or return for a new offer, and transactional task/history/audit/retry updates.
  Submission retains reservations; acceptance leaves the task in REVIEW for
  delivery gates. HTTP/CLI/MCP result operations and recovery remain deferred.

- Team assignment HTTP primitives: offer/read/accept/decline/withdraw with live
  coordinator and worker authorization. These expose the schema 17 foundation;
  result/recovery operations and the full MCP/CLI execution workflow are deferred.

- **Project schema 17:** internal exclusive team offers, worker acceptance,
  decline/withdraw and persistent managed-task protection. Task reads expose
  derived coordination; generic edits/archive cannot bypass ownership, including
  as admin. HTTP assignment commands and the full agent workflow remain deferred.

- Team messaging through HTTP, CLI and MCP: typed messages, team-visible history,
  routed inboxes with bounded waits, explicit acknowledgements and retry receipts.
  Current access and session generation are checked while waiting. Messages do
  not assign work; execution assignments remain a later increment.

- **Project schema 16:** internal durable team messages, snapshotted recipient
  inboxes and explicit acknowledgements. Retry receipts, generation checks and
  audit events commit atomically. This storage foundation does not yet expose
  agent messaging through HTTP, CLI or MCP, or assign tasks.
- Agent team registration through HTTP, CLI and MCP: join by exact name or ID,
  inspect the roster, update declared profiles, heartbeat, resume and leave.
  Every operation checks current ordinary write-token scope, grants and team
  enrollment. Public session views omit access identity bindings. Workers get
  waiting instructions; messaging and assignments remain unavailable.
- **Project schema 15:** durable team-session storage with coordinator exclusion,
  generation fencing, profile revisions, liveness observations and atomic audit.
  This is the storage foundation for the session interfaces above.
- Administrator team setup: `aimem teams` and admin-only HTTP routes create,
  inspect and replace project-bound team configuration and user enrollment.
  Revision checks, retry receipts and atomic audit events preserve configuration
  history. Team-bearing projects cannot be dropped or merged; rename preserves
  their identity. Agent sessions and task assignments follow in later increments.
- **Project schema 14:** additive team and audit tables. Back up full project
  state, including access identities, before upgrading. Older binaries refuse
  upgraded databases; rollback requires restoring the pre-upgrade state as well
  as the old binary. No release or deployment is implied by this entry.

### Fixed

- The task page exposes the selected epic's objective, status and target
  through an expandable View epic panel above its filtered list or board.
  The epic selector has its own row so long titles do not change its position.
- The task-page project picker lists only projects with Kanban enabled,
  including empty boards ready for their first task. General project lists
  and direct links to tasks in disabled projects remain available.

## [0.6.0] — 2026-09-21

### Data integrity

- **Access database schema 2:** ordinary tokens gain an explicit scope.
  Existing project tokens stay project-scoped and existing read-only tokens
  stay read-only; no migration broadens access. Older binaries refuse the
  upgraded access database. Back up the hub state before upgrading;
  project database schema remains 13.
- **Upgrade every participating agent client** before relying on local
  task-token overrides: older clients ignore the nonsecret marker and
  continue using the per-hub credential. Each clone/worktree needs its own
  local credential setup. Hub authorization remains authoritative.

### Added

- **Repository-local task credentials:** `aimem task-token set` reads a
  project token from stdin and adds only a nonsecret requirement marker to
  `.aimem.json`. The credential stays in a protected per-user store, bound
  to checkout/project/hub; it overrides the per-hub task token without
  fallback on errors. `show-source` reveals no secret; `clear` explicitly
  restores per-hub selection. MCP calls and process bootstrap honor the
  requirement. See [Task credentials](docs/TASK-CREDENTIALS.md).

- **User-scoped task tokens:** `aimem access token-issue-user <user-id>
  <label> <expiry-RFC3339>` issues a token that follows the user's current
  direct/group project grants. Grant changes take effect without reissuing
  it. Existing `token-issue` syntax still issues project-scoped or read-only
  tokens. The admin API accepts `scope: user|project|read-only`, preserving
  legacy semantics when omitted. Identity and task/epic/comment writes use
  one live authorization check, including in-process MCP mutations.

### Changed

- **Kanban onboarding documentation:** a full quickstart covers Debian
  service-user setup, project enablement, user and project token issuance,
  per-hub credentials, checkout-local overrides and process selection.
  The roadmap separates merged implementation from release/rollout gates
  and describes current task pickup plus planned priority/complexity work.

- **Installers upgrade stale installs:** `boot.sh` / `boot.ps1` now replace
  an installed aimem that is older than the release being fetched. Same or
  newer versions are still left alone; `AIMEM_REINSTALL=1` still forces a
  refresh of a current install.

## [0.5.0] — 2026-09-14

### Data integrity

- **Two schema bumps since 0.4.0 (v12 epics, v13 typed references), and
  the v13 step rewrites stored task data in place.** Back up the hub's
  state directory before upgrading: a database opened by this build is
  refused by older builds, and the reference rewrite (task bodies, history
  and retry receipts) has no reverse migration. Details under Changed and
  Added below.

### Changed

- **Typed references (schema v13): `candidate_refs` and `evidence_refs`
  are objects, not strings.** Each reference is `{kind, ref, note?,
  scope?}` with kind one of `task` (a task id), `doc` (a document name),
  `record` (`<collection>/<record id>`), `commit`, `pr`, `ci`, `url` (an
  http(s) URL naming the service and the target — a bare number or hash
  is refused) and `text` (free text), so the board can link and the
  agents can check what a reference points at. This is a pre-1.0 break
  with a migration, not a dual contract: on upgrade every stored string
  reference — in current tasks, history and saved retry results — becomes
  `url` when it is a valid http(s) URL and `text` otherwise, with its
  exact content and never a guessed target, and every task retry receipt's
  digest is recomputed from its saved result, so a request committed
  before the upgrade still replays as its typed retry. A write that still
  sends strings is refused with 400 naming the new shape; old readers
  receive the typed form. The task page renders links only for http(s)
  URLs of the external kinds and for task ids, and edits references one
  per line as `kind ref | note` (`kind scope:ref` for a doc or record in
  another project or group). Schema bump: a database opened by this
  build is refused by older builds, and the rewrite of task bodies,
  history and receipts is in place and one-way: back up the database
  before upgrading.
- **Ordinary tokens may list projects; the task page shows every
  credential a project picker.** `GET /v1/projects` is now within an
  ordinary token's surface, read only, returning the ordinary projects
  only (the user memory store and the knowledge groups are filtered out
  for it; legacy writer and admin credentials receive every id as
  before). The `/tasks` page therefore offers the same drop-down to an
  ordinary token that admins had, instead of a free-text project field;
  the field stays as the fallback if the listing fails. Creating and
  deleting projects remain admin actions on the console.

### Added

- **Epics (schema v12): the grouping above tasks.** An `epics` table beside
  `tasks` with its own history — id, title, objective, state OPEN or
  RETIRED, the release or milestone it targets, revision — written through
  the same receipt-backed, revision-checked mutation as tasks. A task
  carries an optional `epic`, checked inside its own write transaction to
  be an OPEN epic of the same project; an assignment a task already has
  survives the epic's retirement, so links and history keep resolving, and
  ids are never reused. Routes `GET/POST /v1/projects/{p}/epics` and
  `GET/PUT /v1/projects/{p}/epics/{e}` on the ordinary surface (writes
  authorized like task writes), the `epic` filter on the task list, MCP
  tools `list_epics`, `get_epic`, `create_epic`, `update_epic` and the
  `epic` argument of `list_tasks`, an epic filter and labels on the task
  page's list and board and an epic field on the task form. Also the
  identity directory, `GET /v1/access/directory`: id, kind, name and
  enabled of every user and group, what the page labels assignees with.
  Schema bump: a database opened by this build is refused by older builds.
- **A project's process documents come from Git at a pinned commit; the
  session start injects them.** An admin selects, per project, the
  repository, commit and manifest that hold the handbook, the checklists
  gating READY and DONE and the task templates (`aimem process select
  <repo> <commit> <manifest> [--ref <branch>] [--expect <commit>]` on the
  hub host, or `PUT /v1/projects/{p}/process`, with a compare-and-swap on
  the previous commit and the history retained); the hub stores the
  reference only. Every credential may read the selection. On a machine,
  `aimem process show` — and the session-start hook, when the project's
  tasks are on — fetches the files with the machine's own Git access (by
  full hash, or by the ref with the hash verified) into a per-commit cache
  promoted only when complete, and injects one bounded unit: the signal
  that tasks are on and which tools exist, the process set's identity,
  the required skills with whether this machine has them, the handbook
  and the checklists. Unavailable, denied and unreachable are reported as
  such; an unreachable hub falls back to the selection as last observed
  with a warning; a unit over the manifest's budget is not injected and
  says how to read it. Nothing is written into the repository.
- **Tasks are enabled per project, by an admin.** A project's tasks are
  on only when an admin has switched them on: through the console's
  project menu, the admin API (`PUT /v1/projects/{p}/meta/tasks` with
  `on`/`off`), or `aimem tasks on|off -p <project>` on the hub host. Every
  task mutation — create, update, comment — in a project whose tasks are
  off is refused with 403 for every credential, the local operator and
  admin tokens included; reads stay. On upgrade the hub switches on every
  project that already holds a task (archived included), once: the setting
  is only ever written where unset, so a later admin "off" survives
  restarts, and a project created afterwards stays off until switched on.
  The identity route answers `tasks_enabled` for a named project to every
  credential; the task page says when a project's tasks are off instead
  of offering a create or a drag; the stdio MCP facade asks the hub once
  at session start and hides the task tools (refusing them by name) when
  the project is off, lists them when it is on or when the hub could not
  be asked, and says "Kanban availability changed for this project.
  Restart the session to refresh its tools and process context" once if
  the hub turns a session that started on away.

### Fixed

- **Task page: a rejected stored token is named as the remembered one.** A
  token left in the browser by an earlier visit that the hub no longer
  accepts produced "That token was rejected", which read as if the user
  had just typed a bad one. The gate now says the token remembered in this
  browser from an earlier visit was rejected and asks for a current one; a
  token the user just typed keeps the plain message.

## [0.4.0] — 2026-09-14

### Added

- **Kanban board on the task page (stage 3, second increment).** The
  `/tasks` page gains a list/board toggle (`/tasks?project=<id>&view=board`,
  remembered in the browser): one column per state, the active tasks as
  cards (archived excluded; the first 500, the list pages through the rest),
  each card opening its task. Dropping a card on a column — or, from the
  keyboard, its "move to" select — is the page's ordinary write: the task is
  read fresh, its content replaced with the new state under that revision
  and a retry key, leaving a terminal state un-archives; a revision conflict
  refreshes the board and says nothing was overwritten. Cards drag only
  when the credential may write in the project; everyone else gets a
  read-only board. Also: a late comment submission no longer releases the
  submit button while another view's submission is pending.
- **Task page, stage 3 of the Kanban work (first increment).** `GET /tasks`
  serves a task list / detail / discussion page: static chrome like the
  console, holding no data, asking for a token in the browser. An
  ordinary project token works and sees exactly what the task service
  allows it (admin and writer credentials pick a project from a list; an
  ordinary token types its project id). List with state filter and
  archived toggle, paged; detail with the prose fields rendered as safe
  Markdown (escaped first), history and paged discussion. Create, edit
  (full replacement under expected revision) and a state selector that is
  the same CAS update an agent makes: a revision conflict shows the
  current task beside the attempt with "reload" or "reapply", never a
  silent overwrite; every write is retry-safe (one idempotency key per
  open form); copy-link actions for the task, its JSON and each comment
  (`/tasks?task=<id>[&comment=<id>]`; `/admin?task=<id>` forwards in the
  browser). The console keeps its setup-and-maintenance role and merely
  links to the page. Both pages now also refuse framing, form posts and
  base overrides in their CSP.
- **Task service over HTTP and MCP, stage 2 of the Kanban work.** The
  storage from stage 1 is now reachable: `GET/POST /v1/projects/{p}/tasks`,
  `GET/PUT /v1/tasks/{id}`, `GET /v1/tasks/{id}/history`, `GET/POST
  /v1/tasks/{id}/comments`, `GET /v1/tasks/{id}/comments/{c}` (OpenAPI
  updated), and eight MCP tools (`list_tasks`, `get_task`, `create_task`,
  `update_task`, `get_task_history`, `list_task_comments`,
  `get_task_comment`, `add_task_comment`). Writes carry their retry key in
  the `Idempotency-Key` header / `idempotency_key` argument; bodies are
  decoded strictly (unknown fields, trailing values and oversized bodies are
  refused); a stale `expected_revision` answers 409 with the current task.
  **Authorization, re-checked on every attempt:** admin credentials and the
  local operator socket write anywhere; an ordinary token writes only in the
  project it was issued for and only while its user holds a current grant;
  cross-project and read-only ordinary tokens read everything; legacy
  writer tokens read tasks but never write them. Ordinary tokens are
  admitted to their identity check, the task routes and `/mcp` only, where
  they see the task tools alone; an assignee must name an existing user or
  access group. **MCP trust boundary:** on the hub, task tools run
  in-process with the identity of the request itself, never through the
  hub's trusted local client (which legacy tools keep using); the local
  stdio facade sends task tools to the project's hub with a dedicated
  per-user credential stored by the new `aimem hub task-token <hub>
  <ordinary-token>` — no credential configured is an actionable error, never
  a fallback to the checkpoint token or the local socket. Task history and
  comments record stable user/token IDs for ordinary actors and the trusted
  name for admins.
- **Task storage (schema v11), stage 1 of the Kanban work — internal only.**
  Each ordinary project database gains `tasks`, `task_history`,
  `task_comments` and `task_requests`: current task with expected-revision
  CAS and a complete actor-stamped history row per accepted revision;
  append-only Markdown comments with stable IDs and a database append
  sequence for paging; retry receipts committed with their mutation so a
  timed-out create/update/comment replays to its original result instead of
  duplicating. Bounds, UTF-8/control-character checks and the authored-
  content secret refusal apply to every field. No HTTP route, MCP tool, CLI
  command or UI touches tasks yet (stage 2 adds the authorized service);
  existing databases migrate on first open. **Lifecycle:** a project that
  holds any task — archived included — now refuses `drop` and refuses to be
  the source of a `merge` until a task-preserving export/removal exists; the
  check waits for a task write already in flight, so a task is never created
  and then deleted (a task landing mid-merge keeps the source; the history
  copy into the target is idempotent). Rename keeps every task, comment,
  history row and receipt reachable. **Upgrade note:** as with every schema
  bump, a database opened by this build is refused by older builds.
- **Project access foundation for the planned task subsystem.** Hub-local users,
  access groups, direct/group project grants, and expiring/revocable ordinary
  tokens, with transactional administration audit. Admin-only HTTP endpoints and
  `aimem access` host CLI manage them; `/v1/access/identity` checks current identity
  and project write eligibility. Host-console admin tokens keep full access.
  New ordinary tokens cannot access legacy writer APIs or MCP until task-specific
  permissions are wired in the next increment; existing credentials retain their
  behavior. Project grants survive rename and are not inherited on name reuse.

### Fixed

- **Retry receipts survive a content field being added later.** A task
  write's retry receipt fingerprints the request so a replay returns the
  original result and a changed request is a conflict. The fingerprint was
  the request's JSON as the binary encodes it, so a field added to the task
  content in a later version would have turned every retry across that
  upgrade into a conflict. It now ignores zero-valued fields, sorts keys
  and names its format; a later empty field does not change it, an absent
  optional field and an empty one match (the replace-all rule), and a
  changed value is still a conflict.
- **Task lookup no longer scans every project on every read.** Resolving a
  task id to its project opened and queried every ordinary project per
  lookup — for any valid credential, hit or miss. The hub now remembers
  where a task was last found (from its create or an earlier lookup) and
  checks that project first, falling back to the scan and correcting the
  hint after a rename, merge or drop; a conclusive miss is remembered for
  thirty seconds so a repeated unknown id costs one scan, and a miss that
  could not be concluded (a project that would not open) is never cached.
- **MCP task and document tools refuse an unreadable `.aimem.json`.** The
  config read every capture path uses treats a file that exists but cannot
  be parsed as absent (with one warning), so a checkpoint never blocks on a
  broken file — but the stdio facade's task and document tools took that
  "absent" for "the default hub" and would have sent a bound project's
  traffic there with that hub's credential. They now read the binding
  strictly (`ident.ProjectHubNameStrict`) and answer with the parse error
  instead; nothing is sent to any hub until the file is fixed. Checkpoints
  are unchanged.
- Reuse one server-owned access database handle and avoid migration write locks
  when opening a current schema; permission checks still read current state.
  Administrators can remove stale project grants by stored instance ID using
  `aimem access grant-rm-instance`, including after project deletion or merge.

## [0.3.31] — 2026-09-12

### Changed

- **Console: "merge into another project" picks the target from a
  dropdown.** The action used a browser prompt box that asked for a
  typed project id and listed the candidates as plain text (user
  notice). It now opens an inline chooser inside the project's ⋯ menu:
  a dropdown of the hub's other projects, a merge button, cancel — no
  free-text id, no list to read. The merge itself is unchanged
  (idempotent fold, citations relabelled, source removed) and still
  asks for one confirmation naming both projects.

## [0.3.30] — 2026-09-12

### Changed

- **Console: one shared status line for all model bindings.** The
  per-binding reserved result line from 0.3.29 put a placeholder under
  every row, which read as clutter (user notice). The bindings card
  now has a single fixed-height "last test" line under the list; each
  result names its model and op, the line resets whenever the list
  re-renders (so it never describes a binding that was just unbound),
  and the toast stays for the case where the line sits below the
  fold. Both earlier guarantees hold: nothing renders inside a row,
  and the line's height is fixed, so neither the buttons nor the rows
  ever move.

## [0.3.29] — 2026-09-12

### Changed

- **Console: provider test results no longer shove the buttons around.**
  The chat/embed test result used to render inline between "test
  embed" and "unbind", so every result pushed the button row sideways
  (user notice). Each binding now has a permanently reserved result
  line under its buttons — reserved, not on demand, so a result never
  shifts the rows below either — prefixed with the op it belongs to
  (failures lead with the elapsed time — "failed after 30042ms — …" —
  so the clipped tail never hides the timeout-vs-rejection signal;
  full text on hover), and a toast echoes the outcome so it is seen
  even when the row is scrolled away.
  Providers and model bindings also sit in separate cards now — one
  shared card made the two lists and forms read as a single block
  (user notice) — and long lists scroll inside their card so the
  add/bind form below stays in view (user question).

## [0.3.28] — 2026-09-12

### Fixed

- **Provider tests tell the truth about the configured provider.** A
  model bound to a provider that could not serve it (no token stored)
  used to fall through silently to the host's `AIMEM_OPENAI_*` env
  endpoint, so the console's test button reported another vendor's
  error for the wrong service (live: a Hetzner binding answered by
  Google's "unexpected model name format"). Bound models now resolve
  only through their binding; when that fails, the test button, the
  model list, the hub's curate factory, and the `curate`/`doc`/
  `embed`/`dedup` CLI paths all name the actual cause ("provider X has
  no token stored", "resolves to a claude endpoint but an openai one
  is required here"), and the service logs why semantic recall is off
  at startup instead of degrading to BM25 silently. Failed tests carry
  the elapsed time so a 15s timeout and a 100ms rejection read
  differently; the provider list flags tokenless providers.
  **Upgrade note:** a host that happened to serve a *bound* model
  through the env pair because its provider had no token now gets an
  explicit failure with the reason instead of silent service from the
  wrong endpoint — fix the provider (any non-empty token for an
  endpoint that needs none) or unbind the model to use env on purpose.
  A `providers.json` that exists but cannot be read or parsed now
  fails CLOSED (no model resolves, every path says why, and the
  console refuses to save over it) instead of counting as "no
  registry" — which had quietly re-enabled the env routing for every
  bound model and would have let one console save erase the
  operator's providers; only a genuinely missing file means "no
  bindings". Curation's backend selection keeps "bound but unusable"
  apart from "unbound": a broken binding no longer falls through to
  the default claude backend (which ran the model on the wrong
  service with no error).
- **Console no longer discards a pasted token on a rejected save.**
  The token field was cleared before the hub answered, so a save
  refused for an invalid name (uppercase) ate the token, and the
  corrected retry stored the provider with none — the root of the
  case above. The field now clears only after the hub confirms.

## [0.3.27] — 2026-09-11

### Added

- **Codex CLI support** — aimem's third client, wired like the first
  two. Codex adopted Claude Code's hook wire format (verified live
  against codex-cli 0.153), so the integration is symmetrical:
  user-level `Stop`/`PreCompact` hooks in `~/.codex/hooks.json` run the
  new `aimem submit-codex`, which parses the session rollout JSONL into
  the same journal events (`client: "codex"`); the project-scoped
  SessionStart handoff reuses `aimem session-start` unchanged
  (`.codex/hooks.json`); recall registers as `[mcp_servers.aimem]` in
  `~/.codex/config.toml` (via `codex mcp add` when codex is on PATH).
  Both installers wire it; Codex asks once to trust the new hooks.
  Codex hook commands are spawned without a shell, so they stay bare
  `aimem …` invocations — no `command -v` guards. Fail-open on the
  capture side: an unreadable, unflushed, or absent rollout degrades
  the checkpoint to what the hook payload alone carries (session,
  turn, final reply) instead of losing the turn, and the Stop adapter
  waits for the turn's `task_complete` line so late-flushed tool calls
  still land.

## [0.3.26] — 2026-09-10

### Changed

- **Console dropdowns offer only scopes with content** (user
  proposal): the Review tab lists only scopes whose review queue is
  non-empty AT THE SELECTED AGE WINDOW — changing the 7/30/90-day
  selector rebuilds the list from per-window counts, keeping the
  current scope when it still qualifies; the Docs
  tab lists only projects holding shared documents; the Wiki tab
  lists projects holding records plus an "all projects…" entry — the
  one place an empty project must stay reachable, because a project's
  FIRST record is created from that tab. Empty states say why the
  list is empty instead of "no projects", and the Review one names
  the selected window. Fail-open by design: a scope whose count query
  errored, or a window the server did not send, stays OFFERED — a
  possibly-empty queue beats a scope silently hidden. The per-project
  counts and the window list ride the existing one-shot /v1/overview
  (no N+1 requests); the console builds its day options from the
  server-sent window list (static markup is the fallback for older
  hubs, pinned by a test). The Docs filter counts retired docs too —
  the tab lists them with restorable history, so retiring a project's
  last doc must not make the project unreachable. Each tab's refresh
  button now re-fetches the overview snapshot before rebuilding its
  dropdown, and the Wiki mode toggle keeps the project being viewed.

### Fixed

- **Schema v10: `memory_audit` gains an index on (memory_id, ts).**
  The review-staleness predicate probes MAX(ts) per live memory, and
  the audit table's only index was its primary key — a full scan per
  memory, newly multiplied across every project once the overview
  began carrying review counts. Existing databases migrate on first
  open, as always.

### Dependencies

- modernc.org/sqlite 1.57.0 → 1.58.0 (SQLite 3.53.4; adds an
  opt-in Linux OFD-locking switch, off by default).

## [0.3.25] — 2026-09-06

### Fixed

- **The claude curation backend is paced, time-bounded, and a full
  llmrate citizen** (architecture review S4; hardened by the max-level
  review). `claude -p` ran with no timeout: a hung CLI blocked the
  hub's entire hourly sweep forever. Calls now go through llmrate
  pacing, are killed at the shared 5-minute completion bound
  (`cmd.WaitDelay` force-closes pipes so an orphaned CLI grandchild
  cannot wedge the kill — proven by a grandchild test; a SUCCESSFUL
  run whose straggler held the pipe is salvaged, not re-billed),
  report clean completions to llmrate (a persisted penalty now decays
  on claude-backend hubs), and claude-shaped rate blocks ("usage
  limit reached", 429/529/overloaded — on stderr or inside the stdout
  JSON) widen it. The prompt travels over stdin, never argv — as
  argv, Windows' 32K command-line limit bites and cmd.exe's unquoting
  (npm .cmd shim) is an injection surface the caller owns. Doc
  synthesis gets a shared 10-minute bound on BOTH backends
  (whole-chapter prompts); the admin provider test gets 30s and skips
  the batch pacer (a diagnostic probe must not queue behind the very
  outage it is diagnosing). `llmrate.Wait` reserves slots and sleeps
  outside the pacer mutex — a paced call no longer blocks health
  checks or query embeddings — and a `Penalize` while callers are
  queued re-spaces them at the widened gap instead of letting the
  in-flight burst keep the old cadence.
  Deliberately no retry loop, per the recorded curation-failure
  design: the cursor stays unadvanced and the next tick retries.
- **MCP `recall_memory` honors its token budget across scopes**
  (architecture review S7). The budget was applied per resolved scope,
  so `scope: "both"` with two declared groups returned up to 4× the
  requested tokens into the agent's context. The combined answer now
  trims against one budget (the pooled rule session-facts always
  used), with scope order — project, groups, user — as the priority
  order, and always at least one hit.

- **Origin-alias repairs now propagate over sync** (data integrity;
  architecture review S3, the mechanism behind the long-lived
  "gitea ghost"). A merge/rename's citation relabel recorded its alias
  map only in the local group DB — a peer that never ran the operation
  kept its dead `project:` citations forever and re-pushed the dead
  label back to everyone on each sync. The alias map now rides
  group-config sync: the import side merges it (union, local
  precedence on conflicts, chains re-pointed) and relabels its own
  existing citations, so one repair anywhere heals the fleet. Old
  peers ignore the new record; nothing else changes shape.

## [0.3.24] — 2026-09-05

### Added

- **`aimem curate --rewind <event-id>|start -p <project>`** — replay a
  consumed curation window (a zero-yield batch, a bad model day)
  without hand-editing the cursor file; the zero-yield warning now
  prints the exact command for its own window.
- **Sync is self-verifying** (data integrity; architecture review
  C1/C2). The event pull stream ends with a counted terminator
  (`?end=1`, hub v0.3.24+) and the client refuses to advance its pull
  cursor over a stream that is truncated or miscounted — previously a
  hub failing mid-stream terminated chunked encoding cleanly, the pull
  looked complete, and events in the gap were silently never fetched
  again. Per-line `failed` counts from the hub now warn loudly
  (`aimem logs`), and 100% failure on any leg — the signature of
  client/hub version or schema skew, previously reported as success
  with exit 0 — is a hard error. Sync also preflights `/v1/status`
  and warns when the hub's self-declared name differs from this
  machine's name for it (the mismatch that silently disabled hub-side
  curation on 2026-09-04). Old hubs and old clients interoperate
  unchanged: the terminator is requested by parameter and required
  only from hubs whose version advertises it.

### Security

- **Installers verify release binaries against SHA256SUMS** (architecture
  review C3: the checksum file was generated on every release and never
  consumed). boot.sh, boot.ps1, and install-hub.sh now abort loudly on a
  missing sums file or a mismatch — verification failure never degrades
  into the source-build fallback, which remains reserved for a release
  with no binary asset at all. macOS uses `shasum -a 256`.

### Fixed

- **Byte-budget retention can no longer silently corrupt search**
  (data integrity; architecture review C5). `events`/`memories` use
  TEXT primary keys, so their implicit rowids are renumberable by the
  VACUUM retention runs between delete batches — and both FTS indexes
  are external-content tables keyed on exactly those rowids. Retention
  now rebuilds the FTS indexes after any VACUUM, and a new
  `aimem fts-rebuild -p <project>|--all` command exposes the same
  repair for manual recovery. The no-UPDATE invariant the indexes
  depend on is now documented at the schema definition.
- The curation cursor is written atomically (temp+rename) — a torn
  write left an empty cursor, which means "re-curate the entire
  journal from the beginning".
- **An invalid hub name in `.aimem.json` no longer routes data to the
  default hub** (data integrity; architecture review C4).
  `ProjectHubName` returned an error precisely so this could not
  happen — and every caller discarded it, silently falling back to the
  default hub and crossing the partition multi-hub exists to defend.
  Now: checkpoints stay fail-open locally but their hub push is
  QUARANTINED under a reserved unconfigured name (spooled, with a loud
  `aimem logs` warning naming the fix; once the config is repaired the
  periodic sync delivers from the journal — nothing lost, nothing
  leaked). `aimem docs`, `aimem col`, and the MCP doc tools fail
  loudly; the session-start handoff notice skips hub contact (session
  start never blocks).
- install-hub.sh detects arm64 and installs `aimem-linux-arm64` (it
  hardcoded amd64, so an arm64 hub got an `exec format error` binary).

### Changed

- **Releases build only from master.** The release workflow now refuses
  to build a tag whose commit is not reachable from `master`, so a
  commit that bypassed the PR gate cannot ship as a release just by
  being tagged. Part of the move to branch-protected, PR-only master.

## [0.3.23] — 2026-09-04

### Added

- **LLM call pacing with adaptive rate and retry** (incident-driven: a
  hub's hourly curation sweep burst dozens of requests through a
  provider chain whose far end sits behind Cloudflare; the bot/rate
  rules blocked every batch for the hour, while interactive agents on
  the same chain — one polite request at a time — never saw an error).
  Outbound curate/embed calls are now spaced process-wide
  (`AIMEM_LLM_INTERVAL`, default 2s), transient blocks (429/5xx, or an
  HTML block page where JSON belongs — including one smuggled inside a
  proxy's error message) retry with backoff (`AIMEM_LLM_RETRIES`,
  default 3) instead of abandoning the batch, and each detected block
  **widens the spacing adaptively** (doubling, capped at 2 min,
  decaying again on success). The penalty persists in the state root,
  so a curate run inherits the spacing the previous run earned.
  Visibility: `aimem health` gains `llm_pace` (interval, penalty,
  spacing, block count, last block), the TUI's Hub tab shows it per
  hub (amber while penalized), and block/retry/recovery events log
  with an `aimem llmrate:` prefix. Provider error bodies are truncated
  in errors and logs — a Cloudflare page is ~8KB of HTML that used to
  flood the journal.

## [0.3.22] — 2026-09-04

### Fixed

- Groups tab: proposal/sweep status messages ("asking the curate
  model…", re-label progress) render on their own line below the
  chapter buttons instead of inline, where they shoved the save button
  onto a wrap (user notice).

## [0.3.21] — 2026-09-04

### Changed

- **Manual fact organization goes through labels, not chapters** (user
  verdict on 0.3.20's "new chapter…": project chapters that nothing
  consumes are labels with ceremony). "new chapter…" is gone —
  chapters are never invented from a fact card; "+ chapter" still
  offers declared + derived ones where they exist. Instead every fact
  card gains **"+ label"** (inline input with type-ahead over the
  scope's vocabulary; free text extends it) and label chips get an ×
  to untag — add/remove symmetric with chapters, landing in the
  vocabulary the facet, search, and recall already run on.

## [0.3.20] — 2026-09-04

### Fixed

- **"+ chapter" on fact cards works in project and user scopes** (it
  visibly did nothing — the select filled only from the DECLARED
  chapter list, a group concept, so it rendered empty everywhere
  else). It now unions declared chapters with ones derived from the
  scope's fact tags, and non-group scopes gain a "new chapter…" entry
  (validated name, reserved views refused) so the first chapter can be
  started right from a fact; the tree's derived chapter rows
  (v0.3.13) pick it up immediately. Group chapters stay managed in
  the Groups tab.

## [0.3.19] — 2026-08-31

### Changed

- The label picker is properly **faceted** (user proposal): with
  filters active it offers only labels that co-occur in the current
  result set — chapter, origin, selected labels, and the search text
  all narrow the suggestions, so a pick can never land on zero facts,
  and each count is the exact result size of adding that label.

## [0.3.18] — 2026-08-31

### Changed

- Label-picker suggestions are one line each — "label (N)" — instead
  of the two-line value + annotation rendering; the count strips back
  off when a suggestion is picked.

## [0.3.17] — 2026-08-31

### Changed

- The label picker is a **type-to-search combobox** (user: many labels
  were hard to select and read as unsorted): typing filters the list
  natively, options sort alphabetically for scanning, per-label fact
  counts remain as annotations, and only a real label of the scope is
  accepted (half-typed text never becomes a dead filter).

## [0.3.16] — 2026-08-31

### Changed

- **Label filters moved beside the search bar, and several combine**
  (user proposal): a "＋ label filter" picker next to the search box
  adds labels one at a time; active ones show as removable chips
  ("facts carrying all N labels" — each added label narrows), and the
  label chips on fact cards toggle the same set, highlighting when
  active. The left tree lists only chapters and origins again — its
  height no longer depends on a scope's label vocabulary at all.

## [0.3.15] — 2026-08-31

### Changed

- The label facet is a **dropdown** instead of an expanded row list
  (user: a scope can carry dozens of labels and the left bar grew too
  long) — one row regardless of count, holding every label most
  common first with its fact count; "all labels (N)" clears. Card
  chips still toggle the same filter.

## [0.3.14] — 2026-08-31

### Added

- **Label facet in the KB tree** — the real fix behind "no chapters
  for projects": project facts carry no `chapter:` tags at all; the
  labels visible on their cards are ordinary entity tags. The left bar
  now lists the scope's most common labels (count-ordered, capped,
  long tail via the search box which matches labels too) as toggling
  filters, combinable with the chapter and origin facets; the label
  chips on fact cards are clickable shortcuts to the same filter.
  Groups get the facet too, as a refinement under their chapters.

## [0.3.13] — 2026-08-31

### Fixed

- **Project scopes now show chapter filters in the KB tree.** The tree
  listed only a scope's DECLARED chapters (a group concept, managed in
  the Groups tab), while project facts carry `chapter:` labels too
  (explicit filing, charter-routed copies) — so a label was visible on
  the fact card but impossible to filter by (user report). Chapter
  rows now derive from the facts' own tags, unioned with the declared
  list, exactly the way the origin facet always derived from sources;
  a derived row's tooltip says it is not a declared chapter of the
  scope.
- boot.ps1 works under the default Restricted execution policy: the
  downloaded install.ps1 now runs in a child shell with a
  process-scoped `-ExecutionPolicy Bypass` (`irm | iex` is exempt from
  the policy, but invoking a downloaded .ps1 as a FILE is not — a
  fresh machine died right there). No machine state is changed; the
  scheduled tasks were already policy-safe (they execute aimem.exe
  directly). The Windows one-liner is now documented policy-wrapped
  everywhere. (boot.ps1 is served from master, so this half was live
  before the release.)

## [0.3.12] — 2026-08-30

### Fixed

- **Merged/renamed origin labels stay dead** (data integrity). Sync
  UNIONS source labels across copies, so the merge's local relabel was
  not durable: a lagging peer (lived: a machine with only ssh down,
  still syncing every 10 minutes) pushed the old `project:<id>` label
  right back within minutes. Merges and renames now also record an
  origin ALIAS ({old: new}, chains re-pointed) in each group DB, and
  every future source write — sync import or new fact — normalizes
  through it. Re-run the relabel once after upgrading to clean current
  rows; the alias then blocks resurrection from any peer, upgraded or
  not.

## [0.3.11] — 2026-08-30

### Changed

- **Re-label sweeps the whole KB in one click**: instead of clicking
  per slice, the console now loops the bounded proposal calls itself
  (the server's rotation cursor continues across them), merging every
  suggestion into ONE combined review. Progress reads in facts —
  "re-labeling… 160/380 facts reviewed · 12 suggestions" — with a
  cancel link that stops after the current step and keeps everything
  found so far; a mid-sweep error likewise surfaces partial results.
  Implementation batch sizes never appear in the UI. Nothing is
  written until "apply approved", as before.

## [0.3.10] — 2026-08-30

### Fixed

- **Re-label runs now rotate through the candidate pool** (user asked
  "what happens when facts don't fit in context" — the answer exposed
  a starvation bug). Proposals batch at 80 facts per call; the unfiled
  pool drains as facts get filed, but the revisit pool does not, so
  with stable ordering the same first 80 would be reconsidered forever
  on a big KB. A persisted cursor now walks the pool across runs,
  wrapping at the end, advancing only on a successful proposal (a
  failed model call retries the same slice).
- KB tree ⋯ menu names what actually lives on the Groups tab
  ("chapters, charter, AI propose / re-label") — the re-label button
  was hard to find from the KB tab.

## [0.3.9] — 2026-08-30

### Changed

- KB tree: a scope with nothing to filter by (no chapters, no origins)
  no longer expands into a lone "all" row — it expands into nothing
  and keeps its fact count on the title line. The "all"/unfiled/from
  sub-rows appear only when they distinguish something.

## [0.3.8] — 2026-08-30

### Added

- **Re-label pass for filed facts**: the chapter set evolves as a KB
  grows, and facts filed under the early chapters may belong in the
  newer ones too. `POST .../chapter-proposal?revisit=1` (console:
  "re-label filed facts (AI)" next to the propose button) pools
  already-filed facts with room under the 3-chapter cap, shows the
  model their current filings, and proposes ADDITIONAL labels only —
  the model can never move or unfile; the same human review/apply flow
  gates every write. Automatic re-labeling (curation-driven, opt-in
  per group) is the recorded follow-up once the manual pass proves its
  judgment.

### Changed

- KB tree: clicking the selected chapter again deselects it (back to
  "all"), matching how origin filters already behaved.

## [0.3.7] — 2026-08-30

### Added

- Merge-into handles the **orphaned-origin** case found live minutes
  after 0.3.6: a project dropped before merge existed leaves group
  facts citing a ghost id ("from RC-000668815ca9" with no such
  project). When the merge source no longer exists but group facts
  still cite it, the operation degrades to a pure citation relabel
  (zero citations = typo, refused); responses now count relabeled
  citations. NOTE: sources union across copies during sync — relabel
  every copy of a group DB (each hub and machine holding it), or the
  next sync re-unions the old label from an untouched peer.

## [0.3.6] — 2026-08-30

### Added

- **Merge two ids of the same project**: `POST
  /v1/projects/{p}/merge-into {into}` (admin) folds one project's
  journal, facts, and curation history into another and removes the
  source — the fix for one real project living under two ids (a
  derived id from before its `.aimem.json` `{"project"}` pin plus the
  pinned name), which splits the KB's origin facet. Every copy path is
  the idempotent machinery sync already trusts, so a re-run after a
  partial failure completes rather than duplicates; group citations
  relabel like rename; refused while the source holds shared docs or
  collections (names could collide silently). Console: "merge into
  another project…" in the KB tree's ⋯ menu.

### Changed

- The console verb for changing a fact's text is now **edit** (was
  "supersede" — the mechanism's name, not the user's intent;
  supersession with full lineage is still exactly what happens, and
  the toast says so). The KB tab's editor is now in-card and multiline
  (a one-line `prompt()` made long facts unreadable), and the Review
  tab's update flow uses the same auto-growing textarea.

## [0.3.5] — 2026-08-30

### Changed

The TUI's parity pass from the architecture review — the console/TUI
split itself was judged correct (console operates the memory content
on the hub; the TUI answers "is this machine healthy and flowing"),
so these are the three local-only gaps, each costing zero network:

- **Pending merge conflicts surface on the Projects tab**: a bound
  file with a `<file>.merge` preview waiting for `aimem docs merge`
  now shows as a warning line at the top — the one state on a machine
  that waits for a human, previously visible only in `aimem logs`.
- **Spool is broken down per destination**: the Hub tab counts queued
  EVENTS per spool file ("local service: N, hub work: M") instead of
  counting files — a 500-line hub spool used to read as "1".
- **Projects show their hub binding** in a new column, making the
  multi-hub partition visible at a glance.

## [0.3.4] — 2026-08-30

The storage-architecture review's parity slice (PROJECT-REVIEW §6):
the wiki now has every affordance documents already had, and the
console's structure mirrors the architecture instead of its own
accretion history.

### Added

- **Search sees the wiki**: `aimem search`, MCP `search_journal`, and
  the hub search route now return record hits (collection/id, rev,
  snippet) alongside journal events and doc hits — same exact-scan
  design, retrieval stays fetch-by-id.
- **Record history is enumerable**: `GET .../collections/{c}/log/{id}`
  (wildcard ids end the pattern, hence not a `/log` suffix),
  `aimem col log`, and a history button in the console's record
  editor — the parity trio documents already had.
- **Console Search tab**: full-text over the journal (its first
  console surface), doc hits, and wiki-record hits in one place.
- Records get the softer-tier secret warning documents had (CLI put
  and MCP put_record; hard shapes were always refused).

### Changed

- Console tabs are grouped into two clusters mirroring
  docs/STORAGE-GUIDE.md — Knowledge Base · Review · Groups · Docs ·
  Wiki, a separator, then Search · AI Setup · Usage · Health · Log —
  instead of accretion order (Review sat orphaned at the far end,
  storage tabs after Health).

## [0.3.3] — 2026-08-30

### Changed

- The console wiki view is now a collapsible **tree** instead of a flat
  heading-per-segment page (which split `admin/get` into two heading
  rows): branches fold, entries fold too but start open so content
  stays previewable, and the edit link no longer toggles the fold it
  sits in. Release bodies now carry the tag's CHANGELOG section
  (workflow change; v0.2.1–v0.3.2 backfilled).

## [0.3.2] — 2026-08-30

### Changed

- Collections get their own console tab — **Wiki** — instead of a
  cramped line under the Docs tab (user feedback minutes after first
  contact). Collections list as cards; opening one shows the
  **rendered wiki by default** (headings from the id tree,
  title/description promoted, scalars as a definition list, nested
  shapes as JSON — mirroring `aimem col render`), with an "edit ·
  rev N" link on every entry and a table mode for operators. The
  record editor fetches fresh before editing, so the rev on screen is
  the base_rev the save asserts.

## [0.3.1] — 2026-08-30

### Fixed

First-live-run fixes for structured collections, found dogfooding the
aimem API wiki minutes after 0.3.0:

- `aimem col` flags (`--base-rev`, `--scope`, `--out`) now work in the
  natural trailing position (package flag stops at the first
  positional; they are scanned manually now, like `docs --force`).
- `aimem col import` disambiguates listing/item GET collisions: after
  parameter stripping, `/docs` GET and `/docs/{name}` GET collapsed to
  one id — the item op now gets `-one` (`projects/docs/get-one`), and
  only when an actual collision occurs. Import counters made honest:
  "applied (created or already identical)" vs "diverged and left
  alone".
- A UTF-8 BOM on a record body file (PowerShell's `-Encoding utf8`
  writes one) no longer turns into a confusing "hub: EOF": the client
  strips it and a marshal failure now reports "record body is not
  valid JSON".

### Added

- `docs/API.md`: the first release cut of the aimem API wiki —
  generated from the hub's `api` collection (58 records seeded from
  openapi.json) by `aimem col render`.

## [0.3.0] — 2026-08-30

### Added

- **Structured collections** (`docs/DESIGN-structured-docs.md`): live
  trees of small JSON records on the hub for authored structured state
  under concurrent multi-agent edit — the motivating case is a
  framework's API wiki. The compare-and-swap unit is the RECORD, so
  writers touching different records never conflict; ids are slash
  paths forming the tree (`api/messages/create`); group-scoped
  collections ride the existing knowledge-group machinery. Surfaces:
  hub API (`/v1/projects/{p}/collections/...`, in OpenAPI + parity),
  CLI (`aimem col list|get|put|rm|render|import`), MCP tools
  (`list_records`/`get_record`/`put_record` — scope resolves from the
  `.aimem.json` `{"collections":[...]}` binding), and a console table
  editor in the Docs tab (conflict shows the current record to re-apply
  onto). Markdown is strictly GENERATED (`aimem col render`, one file
  or a directory tree, deterministic, marked "do not edit") — git
  receives release cuts, never the living copy. `aimem col import`
  seeds a collection from an OpenAPI spec; the first live collection is
  aimem's own API wiki. Schema v9 (two new tables; existing data
  untouched).

## [0.2.6] — 2026-08-30

### Added

- `aimem docs sync` — run the doc-collab reconcile on demand instead of
  waiting for the ~10-minute periodic sync: fast-forward pulls, clean
  merges auto-applied and pushed, conflicts previewed, then anything
  locally changed is published. Each action prints as it happens.
- The `<file>.merge` conflict preview now opens with a self-describing
  header: the base revision the three-way merge used, the hub revision
  and writer it merged against, and the conflict count — so the merge's
  provenance survives even after the hub moves on to newer revisions.

## [0.2.5] — 2026-08-30

### Added

- **Git-like reconciliation for shared documents**
  (`docs/DESIGN-doc-collab.md`): the periodic sync now fast-forwards a
  bound file the machine hasn't changed to the hub's newer revision,
  auto-applies and pushes back CLEAN three-way merges when both sides
  changed disjoint parts, and on real overlaps leaves the bound file
  untouched — dropping a `<file>.merge` preview beside it, warning in
  `aimem logs`, and flagging it at session start until
  `aimem docs merge` resolves it. Console saves that hit a 409 now
  auto-merge the draft in the editor via a new compute-only
  `POST .../docs/{name}/merge` endpoint (clean → save again;
  overlaps → markers to resolve in place). The hub still never merges
  on write; only unchanged files are ever overwritten.

## [0.2.4] — 2026-08-30

### Fixed

- Console doc-conflict hints now say how a conflict is *resolved*, not
  just that one will happen: the handoff editor's note explains the
  refuse-never-overwrite contract and points the divergent machine at
  `aimem docs merge`, and the save-conflict message names the same
  command for any bound doc.

## [0.2.3] — 2026-08-29

### Added

- **`aimem logs`** — local diagnostics in one command: client-side
  warnings and the service log ring. Adapter warnings (spooled
  checkpoints, orphaned hub bindings, shared-doc conflicts, replay
  drops) are now also persisted to `<state-root>/adapter.log`
  (timestamped, rotated at 512KB) — previously they lived only on the
  submit process's stderr, which OpenCode's detached spawn discards
  and Windows hook plumbing buries; that silence is how the RC binding
  incident hid for four hours. The orphaned-binding sync warning
  (0.2.2) persists there too.

## [0.2.2] — 2026-08-29

### Fixed

- **`aimem sync` now warns, loudly and by name, about projects bound
  to a hub name this machine has not configured.** Since the
  no-fallback partition guard such a project syncs NOWHERE and its
  checkpoints spool indefinitely — correct, but quiet enough to hide a
  project for four hours (found live: a `.aimem.json` said
  `"hub": "work"` on a machine whose hub is named `seclab`; facts
  stayed local and curation saw no events until the binding was
  fixed). Hub names are machine-local — use the same names everywhere,
  as ADMIN-MANUAL already said.

## [0.2.1] — 2026-08-29

### Fixed

- Console KB tree: an expanded scope (project, group, or user) no
  longer shows a count in its title — the "all" row directly beneath
  carries the same number, and the duplicate read as two different
  figures. Collapsed entries keep their counts.

## [0.2.0] — 2026-08-29

### Changed

- **License: PolyForm Noncommercial 1.0.0** (was MIT). Free for
  personal, research, and any other noncommercial use; commercial use
  requires a license from the author (open a GitHub issue titled
  "commercial license"). Versions ≤ v0.1.90 were published under MIT
  and irrevocably remain so. Contributions are accepted under
  CONTRIBUTING.md terms (author may sublicense commercially).

The milestone release closing the 2026-08-29 review-and-build day: a
full project review actioned end to end, all seven feature proposals
shipped (hub-API sync, named tokens + OpenAPI, docs merge, staleness
review loop, session-start knowledge injection, honest headless
metering, and document search below), plus the partition no-fallback
guard, console action menus, and the context/auto-compact toolkit
(v0.1.82–v0.1.90 rolled up — see their entries).

### Added

- **Search finds shared documents** (FEATURE-PROPOSALS #4): `aimem
  search` and MCP `search_journal` now also return matching shared
  documents — name, revision, and a snippet around the hit — so "which
  runbook mentions the proxy" has an answer for humans and agents
  alike. Retrieval stays fetch-by-name and whole; documents are never
  ranked alongside facts. Implemented as an exact case-insensitive
  scan (a project holds a handful of ≤256KB docs; FTS is the recorded
  upgrade path if that shape ever changes).

## [0.1.90] — 2026-08-29

### Added

- **Per-project context knobs**: `.aimem.json` may set `auto_compact`,
  `ctx_warn_fraction`, and `ctx_limit`, overriding the host for
  projects whose models degrade mid-context ("lost in the middle")
  long before the window fills — auto-compacting at 0.2–0.4 is sane
  there while the host default stays lax. Precedence: process env >
  project > `~/.config/aimem/env` > default; fraction values above 1
  read as percent (30 == 0.3).

## [0.1.89] — 2026-08-29

### Added

- **The OpenCode plugin reads `~/.config/aimem/env`** with the CLI's
  exact fold semantics (AIMEM_* lines, quotes stripped, process env
  wins), so `AIMEM_CTX_WARN_FRACTION`, `AIMEM_AUTO_COMPACT`, and
  `AIMEM_CTX_LIMIT` are set in the machine's one aimem config file
  instead of whatever shell launched OpenCode. Restart OpenCode to
  apply.

## [0.1.88] — 2026-08-29

### Added

- **OpenCode context warnings now escalate** — first at
  `AIMEM_CTX_WARN_FRACTION` (default 0.8), again every 5% — so one
  missed toast is no longer the last word before the hard context
  error (observed live: a session sailed past its single warning and
  stuck). **`AIMEM_AUTO_COMPACT=<fraction>`** (opt-in, e.g. 0.9)
  additionally triggers compaction at that fraction, while the
  summarizer still has room to run; the journal, compaction marker,
  and handoff instruction already make the "prepare" half automatic.
  Both re-arm after each compaction. REMINDER: custom-provider models
  report limit 0 and both features stay silent by design — declare
  `limit:{context}` in opencode.jsonc (ADMIN-MANUAL 3b) or set
  `AIMEM_CTX_LIMIT`.

### Fixed

- Console chapter rename refuses the reserved view names `all`,
  `unfiled`, and `~` — a chapter carrying one would be
  indistinguishable from the built-in row it collides with.

## [0.1.87] — 2026-08-29

### Data integrity

- **A project bound to an unconfigured hub silently routed to the
  default hub.** `ResolveHub` fell back to the default for a NAMED hub
  missing from this machine's `hub.json` — so pinning
  `{"hub": "work"}` before running `aimem hub add work` sent that
  project's checkpoints across the work/home partition. Found live
  during a project binding. Push now spools under the named hub
  (delivered the moment it is configured) and warns; nothing else
  falls back.

### Added

- **Console KB tree actions moved into a `⋯` menu on the titles** (user
  feedback: the old "rename…" pseudo-row between chapters read as a
  filter). Selected projects get *rename* and *drop* (drop confirms
  with event/fact counts and says plainly that pushing machines
  re-create the id); groups link to their Groups-tab charter; selected
  chapters get **rename chapter** — the add-then-remove relabeling from
  the taxonomy runbook, automated: chapters meta updates, every filed
  fact swaps labels without ever going chapterless (cap-filled facts
  swap remove-first), and failures are counted and reported, never
  hidden.

### Fixed

- MCP `list_docs` now matches its design and description: the default
  listing covers the project AND its member groups (group docs labeled)
  — a group runbook has no bound file in most member checkouts, so the
  tool is often its only access. An explicit `scope` still narrows to
  one scope.

## [0.1.86] — 2026-08-29

### Added

- **Console Review tab**: the staleness queue in the browser — pick a
  project and an age window, see each stale fact with its confidence,
  corroboration, and last-seen age, and record a verdict in place:
  *still true* (confirm), *update…* (supersede with lineage), or
  *expire*. Acting on a row removes it; the queue stays derived from
  the audit trail.

### Fixed

- **`install-hub.sh` no longer echoes the bearer token on re-runs.**
  The secret is printed once, at creation — the same rule named tokens
  follow; an upgrade must not re-echo a live credential into logs. The
  closing hint now suggests per-machine tokens (`aimem token add`) and
  drops the obsolete `--sync` suggestion.

## [0.1.85] — 2026-08-29

### Added

- **Opt-in session-start knowledge injection** (`.aimem.json`
  `{"session_facts": <tokenBudget>}`): a budgeted slice of recalled
  facts rides into context with the handoff, matched against the
  previous session's requests — conventions reach the agent before the
  first mistake instead of waiting for it to think of `recall_memory`.
  Mechanical and local end to end (BM25-only without embeddings, zero
  egress), fail-open at every step, and framed with "verify before
  relying". Covers project, declared groups, and user scopes.

### Fixed

- **The headless `claude` curate backend now reports real token usage.**
  The CLI's top-level `input_tokens` is only the uncached slice of the
  final call; the bulk of an extraction rides prompt caching, so runs
  recorded ~40-80 input tokens and cross-backend comparison was
  meaningless. Input now counts uncached + cache-creation + cache-read
  tokens. Cache reads are cheaper per token, so counts slightly
  overstate the backend's relative cost — the safe direction for the
  budget brake.

## [0.1.84] — 2026-08-29

### Added

- **Staleness review loop** (`aimem review`, MCP `review_memories` /
  `confirm_memory`, `GET .../memories/review`, `POST .../confirm`):
  surfaces active, unpinned facts that are old (default 30 days),
  thinly corroborated (default <= 2 sources), and untouched since.
  Verdicts are the existing audited writes — confirm (new: audited
  touch + modest confidence reinforcement), supersede, forget — so the
  queue is derived from the audit trail and empties itself as you
  review; nothing new is stored, nothing can rot. Facts that arrived by
  sync with no local history queue by their creation time. Console
  review tab not built yet — next UI iteration.

- **`aimem docs merge <name>`** — three-way merge for a diverged shared
  document: base is the revision this machine last synced (fetched from
  the hub's retained history), non-overlapping edits from both sides
  apply automatically, overlaps land as `<<<<<<<` conflict markers in
  the bound file. After a clean merge the next checkpoint publishes the
  result; after a conflicted one, auto-publish stays quiet until the
  markers are resolved by an edit — conflict markers can never reach
  the hub on their own. The storage contract is unchanged: the hub
  still refuses and hands both sides back; this is client tooling
  around it. MCP's `update_doc` conflict message now points bound docs
  at it.

## [0.1.83] — 2026-08-29

### Fixed

- **The Windows installer's new `aimem-sync` task failed to register**:
  `[TimeSpan]::MaxValue` as the repetition duration renders as
  `P99999999DT...`, which Task Scheduler rejects. An omitted duration
  repeats indefinitely. Found on the first live run of v0.1.82.
- **Windows upgrades left the OLD service running.** Stopping the
  `aimem-serve` task kills its conhost wrapper but orphans the aimem
  child, which keeps serving the parked binary — one machine was found
  serving v0.1.73 after three "successful" upgrades. The installer now
  kills stray serve processes before restarting. If you upgraded on
  Windows before this, run the installer again (or kill `aimem.exe
  serve` yourself) to actually switch binaries.

## [0.1.82] — 2026-08-29

### Added

- **Anti-entropy sync over the hub API** (`docs/DESIGN-hub-sync.md`):
  `aimem sync` now exchanges events, memories, and group config through
  `/v1/sync/*` on the hub's existing bearer+TLS channel — no ssh
  account, keys, or remote binary path. Windows machines finally PULL
  curated knowledge (previously they could only push events); the
  installer registers an `aimem-sync` scheduled task, and Linux
  `enable-sync` without an argument uses the API for every configured
  hub. Both directions carry the machine's project filter (bound
  projects + user + declared groups). Hubs that predate the routes fall
  back to their ssh destination with a warning.
- **Named hub tokens** (`aimem token add|list|rm`): per-machine bearer
  secrets stored as sha256 digests in `tokens.json` (0600, host-local),
  with writer/admin roles — writers get events, sync, recall, and
  shared documents; admin adds config, providers, rename, drop,
  retention, chapter tools, and logs. A named token's writes stamp
  `updated_by` on shared documents, so attribution comes from
  authentication. Revocation is deleting one entry, read per request —
  no restart, no fleet re-key. `AIMEM_HTTP_TOKEN` remains an implicit
  admin.
- **OpenAPI**: the full `/v1` surface is described in
  `internal/server/openapi.json`, served bearer-gated at
  `GET /v1/openapi.json`, and pinned to the real route table by a
  two-way CI parity test (paths, methods, and per-route roles).

### Changed

- The hub's HTTP body timeouts widened to 15 minutes (30s header
  timeout guards the connection): first syncs legitimately stream for
  minutes.

### Data integrity

- **A crash mid-replay could silently lose spooled checkpoints.** Both
  spool replayers claim the file by renaming it to `.replay-<pid>` and
  delete the claim when done; a hard kill in between orphaned the claim
  and nothing ever re-scanned it. Replays now sweep orphaned claims back
  into the spool first (events are idempotent, so the rare sweep of a
  still-live claim is deduplicated, not duplicated).
- **Sync dropped multi-chapter filings.** `ImportMemory` attached tags
  through the merge path, which keeps only the first chapter — so a fact
  deliberately filed in three chapters arrived on peers with one, and
  machines diverged permanently on labels. Import now replicates the
  source's filings (the 3-chapter cap still applies).

### Fixed

- **A malformed `.aimem.json` blocked checkpoints** despite the v0.1.77
  changelog saying it is treated as absent. An unparseable file now
  really is treated as absent — with a stderr warning naming what was
  lost (pin, hub binding, groups) — while an invalid value in a
  parseable file remains a hard error.
- **MCP `update_doc` on a bound doc now rewrites the local file and the
  sync bookkeeping** (push+pull equivalent), as DESIGN-shared-docs §4b
  always specified — previously the next checkpoint's auto-publish
  fought the agent's own write with a spurious conflict.
- **A record the service rejected (4xx) was spooled and reported as
  "service unreachable, checkpoint spooled".** Replay would drop it
  later with a warning, but the submitter saw success and a misleading
  message. A rejection now surfaces immediately and is never queued.
- **Two bound files with the same base name** (`docs/A.md`,
  `notes/A.md`) published as the same document and fought each other on
  every checkpoint. The first binding (the default handoff, then
  declared order) now wins and the later one refuses loudly until
  renamed or unbound.
- **Schema migrations are now atomic per step**: the DDL and the version
  bump commit in one transaction, so a crash mid-migration leaves the
  old schema cleanly instead of a partial one the next start would trip
  over.

### Added

- **Secret scanning for shared documents.** Documents publish as written
  (never silently redacted), so publishers now scan: private key blocks
  and recognised vendor token formats refuse publication on both the
  client and the hub; softer secret-shaped matches warn on stderr.

### Changed

- **Knowledge mutations now surface storage errors.** Audit, tag, source
  and link writes used to be silently best-effort; the design promises an
  audited trail for every mutation, so a failed side-effect write now
  fails the mutation instead of losing the record quietly.
- **Hub spool flushes are bounded** (100 records per checkpoint, early
  give-up after 3 consecutive transport failures) so a large backlog or a
  hub dying mid-flush cannot stall the coding client's Stop hook; the
  remainder drains on later checkpoints. A spool past 8 MB adds its size
  to the per-checkpoint warning so long outages are hard to miss.
- Transactions open with SQLite's immediate lock (`_txlock=immediate`),
  so a concurrent CLI-beside-daemon doc write waits its turn instead of
  failing with a non-retryable snapshot error; conflict-payload
  truncation no longer splits UTF-8 runes; `aimem docs list` compares
  the local file against the sidecar hash instead of fetching every doc
  body from the hub.
- Test coverage extended to the previously untested edges: the real TCP
  auth wrapper (the old test exercised a drifted copy), the embedding
  width guard, UUIDv7 monotonicity and cursor math, MCP's SESSION-STATE
  refusal and CAS-conflict payload, and `aimem docs pull`'s
  refuse-to-clobber guard.

## [0.1.81] — 2026-08-29

### Added

- **Console Docs tab**: browse a project's shared documents, read and
  edit them, walk their revision history, load an old revision back into
  the editor (saving restores it as a new revision, never a rewrite of
  history), and retire one. Saving is a compare-and-swap write, so a
  conflict refuses, names the other writer, refreshes the listing, and
  keeps your unsaved text in the editor.

## [0.1.80] — 2026-08-29

### Added

- **Shared documents** (`docs/DESIGN-shared-docs.md`): whole authored
  files — the handoff, runbooks — versioned on the project's hub with
  compare-and-swap, never newest-wins. `docs/SESSION-STATE.md` is bound
  by default and publishes automatically on every checkpoint whose hash
  changed; a conflict refuses and names the other writer. New CLI
  `aimem docs list|push|pull|diff|log|rm` (tombstoned deletes; `pull`
  refuses to clobber local changes), MCP tools `list_docs` / `read_doc` /
  `update_doc` (CAS conflicts return both sides so the agent can merge;
  SESSION-STATE is file-only by design), and a session-start notice when
  the hub holds a newer handoff than this machine last saw. Schema v8.
  Opt out per checkout with `"docs": []` in `.aimem.json`.

## [0.1.79] — 2026-08-29

### Fixed

- **The admin console was unresponsive on 0.1.78.** Raw newlines inside
  JavaScript string literals made the page's script a SyntaxError, which
  discards the entire script block rather than one function — the token
  gate accepted a paste and Connect did nothing, so a valid hub token
  looked like a dead hub. `TestAdminPageScriptParses` now scans the
  embedded script for string literals left open at end of line.

## [0.1.78] — 2026-08-29

### Added

- **Rename a project from the console.** `POST /v1/projects/{p}/rename`
  moves the journal, its facts, its embeddings and its curate cursor, and
  relabels the `project:<id>` citations that shared knowledge bases keep
  about their contributors. Refuses reserved ids (`user`, `group-*`) and a
  target that already exists. The rename does not reach client machines: a
  client still deriving the old id re-creates it, so pin
  `{"project": "<id>"}` in that project's `.aimem.json`.

### Fixed

- Releases are cut by pushing a tag; `.github/workflows/release.yml`
  tests, cross-builds five platforms with `SHA256SUMS`, and publishes
  them as release assets.

## [0.1.77] — 2026-08-29

### Data integrity

- **The Windows installer wrote UTF-8 BOMs into every JSON config it
  touched.** Go's `encoding/json` rejects a BOM, and a malformed
  `.aimem.json` is deliberately treated as absent so it cannot block
  checkpoints — so a project's hub binding and group membership silently
  vanished and its data went to the machine's default hub. `install.ps1`
  now writes BOM-free, and `ident.readConfig` tolerates a leading BOM
  because editors will keep producing them.

## [0.1.76] — 2026-08-29

### Added

- **Public status page at `/`** plus `GET /v1/status`: liveness, build,
  hostname, uptime, and nothing else. With `/admin` these are the complete
  unauthenticated surface; every other route stays bearer-gated.
- `docs/ADMIN-MANUAL.md` documents that surface, and why the listener is
  8440 rather than 443 (a systemd `--user` service cannot bind below 1024).

## [0.1.75] — 2026-08-29

### Changed

- Admin header is no longer a full-width slab of the accent color: panel
  ground, hairline rule, one line tall, accent kept as the title's ink.

## [0.1.74] — 2026-08-28

### Data integrity

- **Sync re-broadcast defeated hub partitioning.** `aimem import-events` —
  the pull half of every sync — went through `adapter.Submit`, which always
  ends in a hub push. Exported events carry no hub binding, so the push
  went to the importing machine's *default* hub: a machine subscribed to
  two hubs replicated one hub's projects onto the other, no matter how the
  projects were bound. Imports now use `SubmitLocal` (store, never deliver).

## [0.1.72] — 2026-08-28

### Data integrity

- **Superseding a fact with unchanged text destroyed it.** `Remember`
  folded the new assertion into the very row being retired, so the fact was
  marked superseded by itself: expired, and pointing at itself.

## [0.1.63] — 2026-08-28

### Data integrity

- **Curation reinforced stale facts instead of superseding them.** A new
  assertion that contradicted an existing fact raised the old fact's
  confidence. Conflicts now resolve newest-wins, with pinned facts exempt,
  and land in the audit log so they are visible in the Log tab.

## [0.1.49] – [0.1.73] — 2026-08-28

### Added

- **Multiprovider registry.** Per-model provider entries (URL, token, kind)
  in a host-local `providers.json`, managed from the console with a live
  per-model test probe. Model aliases key vector spaces as `<model>@<dim>`.
  The binding's kind selects the curation backend.
- **Console rebuilt around tabs**: Knowledge Base, Groups, AI Setup, Usage,
  Health, Log. Color schemes for both the console and the TUI. The hub's
  name appears in the header and the browser tab. The Log tab carries the
  service ring buffer and the knowledge audit, so curation conflicts and
  zero-yield runs are visible.
- **Knowledge Base browsing**: group, project and user scopes in one tree,
  paged 50 facts at a time, with `/` focusing a quick search.
- **Chapters became labels**: a fact may be filed in up to three, the first
  staying primary. Unfiled facts get a propose → approve → refile pass.
- **Partitioning enforces itself**: hub bindings travel with config sync,
  and `curate --all` skips projects bound to another hub.
- **Configurable embedding dimension** (`AIMEM_EMBED_DIM`), with a width
  guard that refuses a provider which ignores the request. Scaling
  thresholds and the vector-index decision — in-file pure-Go ANN, not
  sqlite-vec (the driver is pure Go) and not a vector server — are recorded
  in `docs/DESIGN-scale.md`.

## [0.1.26] – [0.1.48] — 2026-08-27

### Added

- **Multi-hub topology**: named hubs, per-project binding via `.aimem.json`,
  partitioned sync, and a hub installer (`install-hub.sh`) with self-signed
  bootstrap and a cert-pull enrollment path.
- **Admin web console** at `/admin`, token-gated in the browser.
- **Knowledge-base chapters** and charter-driven group routing.
- **Semantic dedup** at curation time, plus a retroactive `dedup` command.
- `drop-project` across service, API and CLI.
- Design-document synthesis from a knowledge base.

### Fixed

- Read endpoints no longer auto-create projects, which used to resurrect
  dropped ids as empty husks.
- MCP tool schemas never emit `"required": null`, which broke OpenCode.
- `submit-claude` tolerates the UTF-8 BOM PowerShell 5.1 adds to pipes.

## [0.1.0] – [0.1.25] — 2026-08-26

The system's first day: proposal to running fleet.

### Added

- **Session journals**: every turn checkpointed to a local per-project
  SQLite through a unix-socket service, with spool fallback when it is down,
  and compaction assistance for both Claude Code and OpenCode.
- **Hub and spoke**: an authenticated TCP hub that aggregates journals, with
  push-on-checkpoint and cursor-based incremental sync for anti-entropy.
- **Curated memory**: an asynchronous curator extracts durable, typed,
  tagged, confidence-scored facts through an audited write path; knowledge
  groups share curated facts between consenting projects while raw journals
  never cross a project boundary.
- **Hybrid recall**: FTS5 BM25 fused with cosine over in-SQLite embeddings.
- **MCP facade** for recall, journal search and design-doc access.
- **`aimem tui`**: an operator dashboard (projects, groups, AI, hub) with
  token-usage history, budget caps enforced before spend, and hub resource
  monitoring.
- One-liner bootstrap installers for Linux and Windows.
