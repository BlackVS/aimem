# Portable team context and honest readiness

Status: proposed contract for increment 1 of the
[portable teams review](TEAM-MULTIPROJECT-REVIEW.md), written against
e3dea35 (v0.7.2). Task `01a0cf0f-d158-7000-8956-40d43405d5fe`. What is
implemented is stated below. Tool, field and state names below are proposals until
the implementing increment merges them; the existing single-project v1
authority, session, assignment and credential contracts stay as they are.

Implementation status: increment 1b (decision 1: the embedded unit,
`team_context` on both MCP facades and `aimem teams context`) is implemented
in `internal/teamguide`, `docs/embed.go` and `internal/mcp/guidance.go`.
Its section ids are `common`, `responsiveness` (the probe section, which the
list below did not name), `coordinator`, `worker`, `questions`,
`escalation`, `process-authority` and `example/<name>`; a role's required
set is its playbook sections plus every template they link to. The unit
records `team_protocol` 1 and `min_hub` 0.7.0. Increments 1c and later are
not implemented.

## Problem, from source

A member joins through `team_setup` (`internal/mcp/onboard.go:63`) and the
report sends it to `docs/TEAM-PLAYBOOKS.md` (`internal/teamsetup/teamsetup.go:884`,
`:891`; `internal/wiring/assets/join_team.md:60`). That path exists only in an
aimem checkout, and reading it needs a working shell. Neither holds for an
agent in another project's repository or under a broken sandbox, which is
the failure the owner observed after a successful join.

Three kinds of content are involved, and they have different owners:

| Content | Authored in | Reaches the agent today | Size at e3dea35 |
| --- | --- | --- | --- |
| Team protocol guidance: `docs/TEAM-PLAYBOOKS.md` and the request templates in `docs/examples/team/` | aimem repository, reviewed by aimem PRs; the templates are driven through the real validators by `internal/store/team_examples_test.go` | Only by reading the file from an aimem checkout | 14,048 B and 9,305 B (14 files) |
| Protocol mechanics: tool names, arguments, waits | aimem source | Already delivered: every MCP tool definition carries its description and input schema; `docs/TEAM-AGENT-QUICKSTART.md` repeats them for CLI and HTTP users | 23,064 B (quickstart) |
| Project process: handbook, READY/DONE checklists, templates, required skills | The process repository, selected per project by an admin (`internal/server/process.go:107`) | Session-start hook injects the bootstrap unit when the hook is wired; `team_setup` only reports a one-line process check that warns and never blocks (`internal/teamsetup/teamsetup.go:571`) | aimem's selection: 11,759 B of files, budget 12,288 B |

The selected process handbook already contains a "Team work" section: the
binding team rules (wait for offers, never self-select while joined, offer
rationale, review by evidence, question classes, escalation) are delivered
with the project process. What does not reach the agent is the playbook's
step-by-step detail and the request templates.

## Decisions

### 1. Team protocol guidance ships inside the aimem binary

The playbook and its templates describe the protocol of the release that
contains them: the templates are validated against that release's storage
code, and the command templates already follow the same model
(`internal/wiring/commands.go:15` embeds them; the release workflow builds
them). The contract:

- **Authoring.** Git, in the aimem repository, reviewed like any other
  change. One canonical copy: the release embeds the existing files (for
  example a small `docs` package with `//go:embed`), or the files move and
  the rendered documentation is generated from them. A hand-maintained second
  copy is refused by a test.
- **Publication and pinning.** A tagged release build is the publication.
  The unit's version is the aimem build version plus a digest; there is no
  separate publish step, no hub storage and no admin selection.
- **Digest.** SHA-256 over the canonical encoding: first the manifest (the
  section ids, titles, sizes, section digests and required roles, as compact
  JSON in manifest order), then for each section in that order its id, a NUL,
  its byte length in decimal, a NUL, then its bytes. Each section also
  carries its own SHA-256. The same unit always produces the same digest; a
  changed byte of content or of the role mapping changes it.
- **Sections.** Stable ids (`common`, `coordinator`, `worker`, `questions`,
  `escalation`, `process-authority`, `example/<name>`), each with a title,
  its size, its digest and the roles that require it. The required set for
  a role is what that role must have before work; everything else is read
  on demand by id.
- **Referenced content.** Every relative link in a required section either
  resolves to a section of the unit or is listed as informative (the design
  documents, the quickstart) with a note that it is not delivered. A test
  fails the build on any other link, so the playbook cannot again point at a
  file the agent cannot reach.
- **Limits.** A section is at most 16 KiB; a role's required set at most
  32 KiB; the unit at most 128 KiB. The build fails when a limit is
  exceeded, so an oversized unit cannot ship, and nothing is ever truncated
  at delivery. The measured content above fits: the whole playbook plus all
  templates is 23,353 B.
- **Authorization.** The content is already public with the repository.
  The local MCP serves it without a team session, so an agent can read it
  before joining. The hub facade serves its own embedded copy to any caller
  it already authenticates. No project grant is needed and none is implied.
- **Offline.** The content is in the running binary: no cache and no
  network.

Trade-off, stated plainly: this pins guidance per binary, not per team.
Members on different releases receive different digests, and a guidance
change needs a release. That matches today's release rhythm and the fact
that the guidance changes with the protocol. A per-team pinned selection of
team-wide policy belongs to the multi-project increment, where a team stops
being inside one project; see the corrections below.

### 2. Project process stays the policy authority, delivered through MCP

No new policy store. The process set, its admin selection with
compare-and-swap and history, the exact-commit cache, the complete-marker
rule and the availability notices (`docs/DESIGN-kanban-docs.md`, "Session
bootstrap") are reused unchanged. What changes is reachability:

- `team_setup` and `team_continue` carry the project's full bootstrap unit
  in their result (the `aimem process show --full` text), built by the same
  `processctx.Bootstrap` for the same state root. An agent without the hook,
  or after a compaction, gets the rules through the call it already makes.
  Delivery is all or nothing: a unit over 32 KiB is not delivered and the
  state is `too_large`, with the fix addressed to the process owner (split
  the handbook), the same rule the session-start budget applies. aimem's
  current unit is under 12 KiB.
- A read-only local MCP tool (proposed `process_context`) returns the same
  unit on demand, for an agent that lost it without re-running `team_continue`,
  or with `template` set to a kind the manifest names, that template (today's
  `aimem process show --template <kind>`). It takes no other argument: the
  project is the
  bound checkout's, the selection is the hub's, and the credential is the
  checkout's, selected strictly as today (`taskcred.LocalRequired`: a
  required project token is used or the read fails; no fallback to another
  credential).
- Templates are read by kind from the same unit, never by path.

The project process still needs Git read access on the member's host. A host
without that access gets `project_process: denied`, not a partial policy.
Serving process content from the hub for such hosts is a separate,
optional increment (below), because it copies content from an access-
controlled repository into the hub and needs its own authorization rule.

### 3. Readiness has four independent parts

`status` keeps its meaning (`joined` or `blocked`: the membership outcome)
so existing consumers do not change. A new `readiness` object reports:

| Part | States | Established by | Authority |
| --- | --- | --- | --- |
| `membership` | `active`, `suspect`, `ended`, `refused` | The hub session read or write in this call | Hub |
| `role_context` | `delivered`, `unavailable`, `incompatible` | The required set for the role is present, complete and digest-verified in this same result | Local binary |
| `project_process` | `ready`, `disabled`, `not_selected`, `denied`, `unavailable`, `too_large` | `ready` only when the complete unit (fetched, or the exact cached commit) is in this same result; the others are the existing availability classes plus the delivery limit | Hub selection, Git content |
| `execution` | always `not_verified` | Nothing the MCP process can observe | The agent, per attempt |

`incompatible` means the unit's manifest names a team protocol version other
than the one the hub answers with (today every session response carries
`protocol_version` 1 and the setup core refuses any other); `unavailable`
means the running binary has no usable unit, which the build tests make a
packaging defect rather than a runtime condition.

`ready_for_work` is true only when membership is `active`, role context is
`delivered` and the project process is `ready`. Execution is deliberately
excluded: a live MCP process, a join and a readable `HEAD` do not show that
the agent's own shell, build tools or coding runner work. The worker checks
those itself before accepting a coding attempt and declines or blocks with
the reason when they fail.

`delivered` means the content is complete in this tool result as the MCP
process sent it. A client may cut a long tool result before the model sees
it, and the server cannot observe that. Every delivered unit therefore ends
with a terminator line naming its id, version and digest, and the role
context tells the agent that content without its terminator is incomplete
and must be re-read by section. Client result limits are measured per client
in the pilot, not assumed. Nothing on
the hub or in the MCP process tracks whether the agent read it: a
compaction discards it without a trace, which is why every entry point
(`team_setup`, `team_continue`) delivers it again instead of recording a
flag. What an agent acknowledges is a declaration: the worker names the
role-context digest and the process commit it followed in its `submit`
evidence, the same typed references it already uses.

### 4. Missing context stops work, not membership

- Readiness never changes the membership reconciliation: the context parts
  are evaluated beside it and the membership part is read from its outcome.
  Missing context never leads to `new_session`, a second join or
  a leave; a re-run of `team_setup` or `team_continue` verifies the saved
  session exactly as today and re-evaluates readiness.
- A worker that is not ready heartbeats `unavailable` instead of
  `available`. The hub already refuses both an offer to and an accept by a
  worker that is not `available` (`assignmentWorker`,
  `internal/store/team_assignments.go:130`, called at `:164` and `:239`), so
  the gate is enforced by the existing server check with no schema change;
  a pending offer can still be declined. The hub creates every session
  `available` (`internal/store/team_sessions.go:229`), so readiness is
  computed before the join and the setup core sends the `unavailable`
  heartbeat in the same call, right after it, where it heartbeats
  `available` today. An offer that lands in that one round trip is declined
  with the readiness reason; it is never accepted. The agent's own periodic
  heartbeats must also carry `unavailable` until a later `team_continue`
  reports it ready; the report's next steps and the role context say so. It
  is a client declaration, not an authority: an old client, or an agent that
  heartbeats `available` anyway, is not stopped by it.
- A coordinator that is not ready issues no offers. That is a process rule
  in the role context and the report's next steps; the hub does not check a
  coordinator's context.
- An attempt already reserved before readiness was lost is reported with
  its state, as today, and is not released by the report. The worker blocks
  it with the missing part as the reason (`team_block`); ownership changes
  only through the existing coordination operations.

### 5. Setup, continue and read behavior

| Call | Membership | Delivers | When not ready |
| --- | --- | --- | --- |
| `team_setup` | Joins, or recognizes the saved session (unchanged) | Required role context for the requested role, project bootstrap unit, readiness | Joined and not ready: `unavailable` heartbeat for a worker, next steps name the missing part and its fix |
| `team_continue` | Verifies or resumes the saved session (unchanged); never joins | The same, for the saved role | The same |
| `team_context` (proposed) | None; no session needed | The required set for a named role, or one section by id | Unknown role or section id is an error that lists the valid ids |
| `process_context` (proposed, local only) | None | Full project bootstrap unit and its availability class, or one template by kind | The class and the fix; never a partial unit |

The CLI keeps its human report short: the unit's version and digest, the
readiness line, and `aimem teams context` / `aimem process show --full` as
the read paths.

Neither read tool takes a path, URL, repository, commit, command or
checkout. `team_context` resolves ids against its own embedded manifest;
`process_context` reads only the admin-selected set for the bound project.
The owner-context MCP process therefore does not become a file or command
proxy, and team membership grants no project access: the process read uses
the project credential exactly as the session-start hook does.

### 6. Versions during an existing attempt

- Role context follows the running binary. After an upgrade and restart,
  `team_continue` delivers the new digest and the report names the change
  when the saved state recorded a different one. The tools the agent calls
  are the new binary's, so the new guidance is the one that matches them.
- Project process: an accepted attempt stays under the process commit that
  was current when the worker accepted, for its checklists and evidence; a
  newer selection is reported, not silently applied. Adopting it mid-attempt
  is a coordinator decision (a new offer), as any change of requirements is.
- The versions are recorded by the worker in its evidence (decision 3). The
  hub does not yet record them on the attempt: the assignment snapshots the
  task requirements and the profile (`internal/store/team_assignments.go:36`)
  but no process commit or context digest. Recording them server-side is a
  schema change and a later increment.
- Revocation is never cached: grant, token and enrollment checks stay live
  on every hub call, whatever version the agent holds.

### 7. Compatibility

| Client | Hub | Behavior |
| --- | --- | --- |
| New | New or v0.7.x | Role context comes from the client binary and needs no hub change; project process through the existing routes. Works against any hub with protocol v0.7.0. |
| v0.7.2 or older | Any | Unchanged: no `team_context`, no readiness object, the playbook path in the report. The hub cannot detect this; the pilot must upgrade every member. |
| New command assets | Old `aimem mcp` still running | Step 0 of the regenerated entry points already stops when a tool is missing (`join_team.md:20`); it must check for `team_context` too and tell the user to upgrade and restart. No shell fallback. |
| Any | Hub without the optional process bundle routes | The client uses Git as today; a 404 or 405 on the bundle route is "unsupported", never "denied". |

The report gains fields and never removes one; a consumer of `status`,
`checks` and `next` keeps working.

## Server enforcement versus process rules

| Gate | Enforced by |
| --- | --- |
| Offer and accept only for available, active, current-generation workers | Hub (existing) |
| Worker declares `unavailable` when not ready | Client code, in the setup core |
| Coordinator issues no offers while not ready | Process rule (role context, report) |
| Required context complete, not truncated, within limits | Build-time tests (role context); existing all-or-nothing bootstrap (process) |
| Execution readiness checked before a coding attempt | Process rule; the agent's own check |
| Context versions recorded per attempt | Worker evidence now; hub later |
| Credential selection, project grants, revocation | Hub and `taskcred` (existing, live) |

## Contradictions and corrections

1. **Separate team instruction selection.** The review proposed a team
   instruction selection beside each project's process selection. In v1 a
   team lives in exactly one project (`internal/store/teams.go:33`), and the
   project's process handbook already carries the binding team rules. A
   second selection would give one team two policy authorities. Corrected:
   team policy stays in the project process; protocol guidance ships with
   the binary; a team-wide selection is reconsidered with multi-project
   teams, where it has a real owner.
2. **Hub-published bundle as the first step.** The review and task 1b put
   a validated bundle on the hub first. For the public protocol guidance it
   adds storage, a publish route and an authorization rule for no gain.
   Corrected: the bundle is only needed for project process content on hosts
   without Git access, and is optional (increment 1d).
3. **"Starting a new attempt records the instruction versions."** Not true
   of the current store (decision 6). Corrected to worker-declared evidence
   now, server-recorded later.
4. **Process reference reads are not grant-limited.** An ordinary token may
   read any ordinary project's process reference
   (`internal/server/tasks.go:87`), consistent with v1 task reads. That is
   acceptable for a reference, which grants no repository access, but not
   for hub-served process content: a hub bundle read must require a current
   grant on that project, stricter than the reference read, because it
   replaces a Git permission.
5. **"Delivered" versus "read".** The review asked for delivered versus
   acknowledged context. Only delivery in the same result is observable;
   acknowledgement is an agent declaration and is reported as one.

## Increments

Each has its own task, review gates and human merge.

- **1b. Embedded role context and `team_context`** (S/M). Canonical
  embedding, manifest, digest, sections, link and size tests, the tool on the
  local and hub facades, `aimem teams context`. No hub storage or routes.
- **1c. Readiness in setup and continue** (M). The `readiness` object,
  inline delivery of role context and the project bootstrap, `process_context`,
  the `unavailable` heartbeat, report of version changes, regenerated entry
  points (from the templates, never hand-edited) that check for the new tool.
  Tests: a non-aimem checkout, no agent shell, an old hub, a reconnect, cache
  boundaries and both roles through the real local MCP.
- **1d. Optional hub-served process bundle** (M, only when a pilot host
  lacks Git access to the process repository). An admin publishes the
  validated files of the selected commit with its digest; the selection
  records the digest in the same compare-and-swap; reads require a current
  project grant and return a complete unit or an error; the client verifies
  the digest and caches by it. No Git fetch on the hub, no paths or URLs from
  agent requests. OpenAPI parity for any new route.
- **Later.** Context versions on the attempt record; an optional hub check
  that refuses `team_accept` from a session that declared no current
  context. Both are schema changes and wait for evidence that the process
  rule is not enough.

Out of scope here: user-level agent context, multi-project teams and the
work-store migration (review increments 2 to 4), live joins and any change to
credentials, sandboxes or merge policy.

## Verification of this document

Docs-only. The claims above cite source at e3dea35 and were read, not
executed; sizes were measured with `wc -c` on the tracked files and on the
process set cached for this project. No build, test or live validation is
claimed for the proposed behavior.
