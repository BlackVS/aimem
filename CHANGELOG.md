# Changelog

Notable changes to aimem. Versions are release tags on the repository;
because releases are cut per-deployment, entries are grouped by the change
that matters rather than one heading per tag. Fixes that could lose or
misplace data are called out under **Data integrity** — read those before
upgrading a fleet.

The format follows [Keep a Changelog](https://keepachangelog.com/1.1.0/);
this project does not yet promise semantic versioning. The on-disk schema
version is tracked separately (`currentSchema` in `internal/store/store.go`,
currently 18); a binary refuses a database newer than it understands.

## [Unreleased]

### Added

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
  is listed as informative. Setup and continue do not deliver it yet
  (increment 1c); [design](docs/DESIGN-portable-team-context.md).

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
