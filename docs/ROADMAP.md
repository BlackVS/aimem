# Roadmap

Released baseline: v0.7.3 (PR #60-108): the enabled-project picker and epic
details, the agent-team coordination protocol through the scripted
rehearsal, one-command team onboarding (`aimem teams setup`, the
`/join_team` and `/resume_team` entry points, team listing, guided
provisioning), onboarding through the checkout-bound local MCP
(`team_setup`, `team_continue`), and portable team context (guidance and
project process delivered through MCP with an explicit readiness report),
with the process set 1.15.1 selected for this project. The manual cross-platform pilot and any deployment are
separate owner decisions.
This file records delivery order and scope. The aimem task board owns live
task state, dependencies and completion evidence; design documents own the
contracts. Do not maintain a second checklist of task statuses here.

## Delivered

- Task storage, authorized HTTP/MCP service, task page and board: v0.4.0.
- Per-project task enablement, pinned process references and session
  bootstrap, epics and identity directory, typed references: v0.5.0,
  PR #50-54. Schema 13 rewrites stored references; the release notes
  describe backup and compatibility requirements.
- Explicit token scopes and repository-local credentials: v0.6.0. Multi-project
  access, revocation and installed-client rollout verification lift the token
  onboarding hold. Completion evidence lives on the board.
- Reviewed process assets selected at a pinned commit, with complete bootstrap
  and templates verified on participating clients. Selection is per project.
- The [agent-team protocol](DESIGN-agent-teams.md) for one project: roster,
  structured inbox, protected assignments, results, recovery, audit export,
  scripted rehearsal and guided onboarding: v0.7.0-v0.7.1. Onboarding through
  the checkout-bound local MCP, so a sandboxed shell needs no credential:
  v0.7.2. Portable team context, increment 1a-1c of the
  [contract](DESIGN-portable-team-context.md): v0.7.3, PR #102-108, with the
  [rollout and pilot checklist](TEAM-ROLLOUT-0.7.3.md). The live
  cross-platform pilot is still pending.

## Next delivery sequence

The owner brought team coordination ahead of access-console implementation
on 2026-09-21, and on 2026-09-23 approved the staged direction of the
[portable teams review](TEAM-MULTIPROJECT-REVIEW.md): portable instruction
delivery first, then user-level agent contexts and multi-project teams, each
behind its own design and review gates.

1. Portable teams increment 1, per the
   [portable team context contract](DESIGN-portable-team-context.md): 1a the
   contract (docs only); 1b team protocol guidance embedded in the binary and
   readable through MCP; 1c readiness in `team_setup`/`team_continue`, with
   the project process delivered through MCP and not-ready workers reported
   `unavailable`; 1d a hub-served process bundle only if a pilot host lacks
   Git access to the process repository. 1a-1c are delivered (v0.7.3). Next
   the single-project cross-platform pilot with every member upgraded, per
   the [rollout and pilot checklist](TEAM-ROLLOUT-0.7.3.md). The separate Codex shell
   runner failure is not fixed by this work.
2. User-level agent context independent of the checkout (review increment 2),
   after its own design approval. It preserves checkout mode and strict
   project-token selection.
3. Work-store ADR and migration prototype (review increment 3). No
   multi-project implementation or migration starts before its evidence is
   reviewed.
4. Multi-project protocol and migration (review increment 4), split into
   storage, authorization and client increments; then the cross-host pilot
   (review increment 5).
5. Stage 4 increment 1: access snapshot, users and groups. Include the
   tracked console escaper/handler correction and focused executable UI
   regressions. Grants and tokens remain read-only in this increment.
6. Stage 4 increment 2: grant management, stale-instance cleanup and
   ordinary-token issue/revoke, including one-time secret handling and
   operator documentation. Depend on increment 1.
7. Pull maintenance work as observed failures or use justify it. The
   remaining task-page harness is incremental work, not a prerequisite to
   implement a new general browser-testing framework before stage 4.

Items 2 to 4 are larger and gated; the owner may place stage 4 ahead of them
once increment 1 and its pilot are done. The board records that choice.

Stage 4 has no assigned release version yet. Keep its existing authority
and route boundaries; no new identity system or admin-token web management.
The acceptance criteria live in the implementation plan and board tasks.
Batch minor changes into worthwhile releases; a merged PR does not request
a release or deployment. Team planning does not block ordinary project onboarding.

## Backlog triage

The board groups work under agent enablement/task integrity, task page/board,
and console access management epics. Epics group work; they do not rank it.
Only promote a task to READY once its objective, acceptance criteria,
non-goals and next action are concrete. Dependencies express agreed delivery
order; they are advisory and must be checked by the agent.

Agent pickup policy: honor an explicit user selection first, while checking
readiness and dependencies; otherwise select an eligible READY task whose
dependencies are DONE, following the delivery sequence above. Break ties
by oldest task ID. The service lists by ID (approximately creation order),
not priority, and has no automatic allocator. Read the full task, then
replace its state with IN_PROGRESS under the revision just read, preserving
all content and setting the acting assignee when a configured identity is
available. A conflict requires re-reading and choosing again; never continue
on a stale claim. An IN_PROGRESS task needs an explicit handoff before a
different agent resumes it. This is a process convention, not a server-side
lease or ownership restriction. If nothing is eligible, triage backlog with
the user instead of treating its display order as authorization to start.

This remains the standalone-agent workflow. Under the proposed team protocol,
joined workers wait for addressed coordinator assignments and do not pick tasks
independently. Standalone pickup excludes team-managed work. Communication and
model-fit guidance do not silently change existing permissions on unmanaged tasks.

Planned task metadata (BACKLOG `01a0c44a-5ce8-7000-8957-61ad2be4b836`):
editable priority P0 urgent / P1 high / P2 normal / P3 low, and independent
complexity XS / S / M / L / XL. The creating agent assesses both with a
short rationale; authorized users and agents can revise them with history
and revision checks. Legacy tasks remain unassessed until triaged. Once
implemented, priority ranks eligible READY work within agreed release gates,
before roadmap order and task ID. Complexity guides decomposition, not
priority, and is not a time promise. Explicit team assignment can precede this
feature; it does not change the current pickup behavior described above.

Dependent pickup-policy task `01a0c44b-4143-7000-8cdf-2e89214fd2c5`
adds editable per-project modes: priority-first (default), complexity-first
(smallest first), and combined (priority, then smallest complexity). A run
may have an explicit policy override or complexity limit. All modes check
readiness, dependencies and release gates before ranking; P0 urgent work
is surfaced first, and unknown estimates need triage. Policy and selection
reasons must be visible to agents and operators. This is planned behavior.

Decomposition guidance belongs in agent creation/pickup docs, templates and
applicable shipped skills/bootstrap context: prefer small, coherent tasks
that an agent can finish and verify in a focused session. L tasks trigger a
split assessment; XL or uncertain tasks need a concrete split proposal or
a reason to stay whole before READY. Each increment has its own outcome,
acceptance criteria, estimate and dependencies, with completion criteria
for the original outcome. Use an investigation task when uncertainty is the
main problem; avoid arbitrary file-sized fragments and duplicate children.

Existing follow-ups remain BACKLOG: reference size/migration behavior,
strict request decoding, task-page tests and accessibility, registry lock
hold and unassigned filtering. Additional triage tasks cover stable admin
receipt identities, credential-file persistence, actor-name validation,
project lifecycle, authorization consistency and OpenAPI role metadata.
These are investigations or bounded follow-ups, not new release blockers.

Retention, listing projections, vector scans/indexing and synthesis limits
require fresh measurements before scheduling; the scale proposal's dated
counts are not current capacity evidence. The capacity task records the
threshold decision. Rendering format characters and warning-tier secret
feedback remain conditional design notes until a consumer and acceptance
criteria are identified. Replace-all update semantics are already the
documented contract, not a pending partial-update feature.

Review gates follow AGENTS.md: medium before push; high with verification
and the deployed external reviewer before merge. Max/ultra require an
explicit request. Dependency maintenance follows its own release-age and
review gates and does not imply a feature release or deployment.
