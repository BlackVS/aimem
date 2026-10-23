# aimem working process, version 1

## Authority and session start

This handbook is selected by an aimem administrator at an immutable Git commit.
The bootstrap identifies that commit and manifest; fetch templates and other
assets from the same version. Project AGENTS.md and explicit owner instructions
control local exceptions. Resolve contradictions explicitly before the affected
step; do not silently replace a project rule with this generic text.

Read the project's canonical handoff, then verify its claims against Git, task
records and CI. The board owns live state and evidence; the roadmap owns delivery
order; design documents own contracts. Keep plans separate from verified work.
Do not copy live task statuses into a second roadmap or treat recalled facts as
proof. Process context must be complete even when no memories are recalled.

## Scope, pickup and decomposition

Honor an explicit owner-selected task first, checking readiness and dependencies.
Otherwise select eligible READY work with DONE dependencies in roadmap order,
then oldest task ID. Dependencies are advisory: the agent must check them.
Respect postponed work, serial-PR rules and explicit release gates. A displayed
list order is not priority. If no task is eligible, triage rather than assuming
that the first backlog item is authorized. Obtain an explicit handoff before
resuming another agent's IN_PROGRESS task.

Before work, state the objective, observable acceptance criteria, non-goals,
next action and risk assumptions. Assess scheduling priority P0 urgent / P1 high /
P2 normal / P3 low and complexity XS / S / M / L / XL with a brief rationale.
Until structured fields ship, put that assessment in next_action; do not send
unsupported API fields. These estimates do not change the current pickup order.
Complexity describes scope and uncertainty, not a time promise. Reassess when
scope changes and assess old tasks before promoting them to READY.

Prefer small, independently useful and verifiable increments. L work needs a
split assessment; XL or uncertain work needs a concrete split proposal or a
reason to remain whole before READY. Each proposed child has an outcome,
acceptance criteria, assessment and dependencies; state how the parent outcome
will be completed. Investigate first when uncertainty prevents a credible split.
Do not split arbitrarily by file, create duplicate children, or silently replace,
close or reprioritize the original task.

## Claiming, evidence and completion

Read the full task and update with its expected revision, preserving every
editable field you are not changing; omitted optional fields clear under the
current replacement contract. Record IN_PROGRESS, next_action and an existing
configured assignee when available. Re-read after conflicts; never treat a stale
claim as accepted. Reuse the same idempotency key only for the same request.

States describe work, not an enforced server workflow. READY means the readiness
checklist is met; IN_PROGRESS identifies active work; REVIEW carries a concrete
candidate and evidence; BLOCKED names the obstacle and next unblock action.
DONE requires acceptance evidence and any required merge or deployment. A PR
awaiting human merge remains REVIEW. CANCELLED records the owner's decision.
Record checklist item IDs with the selected process commit in task evidence or
comments. Missing evidence is not a checked item. At handoff, record verified
results, remaining work and the exact candidate; avoid secret or private details
in public commits, PRs, screenshots and logs.

## Reviews and releases

Use oh-code-review at medium before every push, including docs. Use high with
verification on the final PR diff and actual CI before merge, posting the review
to the PR. Max/ultra require an explicit owner request; do not spawn reviewer
agents merely because a change is large. Give each finding a disposition; only
a confirmed in-scope BLOCKER returns work to implementation. Put follow-ups in
separate tasks. Verify runtime-dependent hypotheses at the real boundary before
changing production code to address them.

Request the deployed external reviewer with plain review-this, then watch and
read its result for the exact head. Never push while hands-reviewing is present.
A source change makes the review stale: review the delta and re-trigger. A
base-only update keeps both reviews when the patch identity is unchanged
(`git diff $(git merge-base <base> <head>) <head> | git patch-id --stable`, old and
new head): post a delta note naming both heads and identities instead. Metadata
or additional evidence alone does not require another source review. The human
owns merge. Keep one active PR at a time unless explicitly exempted; postponed
PRs step out of the queue. Rebase from fresh main/master before resuming them.

Run the repository's required checks and behavior-focused validation. GUI work
needs actual browser interaction and desktop/mobile inspection where relevant;
record limitations honestly. A skill marked missing must be made available or
the dependent step deferred with a reason; hooks do not install skills.

A merge does not request a release. Batch minor changes; release when the batch
is worthwhile, for an urgent fix, or on explicit owner request. Verify all
release gates, including the seven-day release-age/provenance gate for dependency
updates. Report merged, released and deployed as separate facts.

## Team work

Joining an aimem team replaces independent pickup for that team. A joined
worker waits for an offer addressed to its session, works only inside an
accepted attempt in an isolated worktree from the recorded base commit, and
waits again after submitting a result; it never selects, claims or edits
backlog tasks while joined, even while the coordinator is disconnected.
Team-managed tasks are excluded from standalone pickup and generic writes;
only the hub's coordination operations move them.

The coordinator applies the pickup order above, then assesses complexity and
required capabilities before offering; L work needs its split assessment and
XL work a split proposal first. Offer to the least costly suitable available
member and record the suitability and cost rationale with the offer; a
stronger model on trivial work or a weaker one on complex work needs an
explicit reason. Review a submitted result against its candidate head with
its evidence, never by vote; rework means a new offer. Finalize DONE only
with delivery evidence, under the same review and human-merge gates.

Questions are factual, project decisions or permission requests. Resolve
factual questions asynchronously from the task, the repository, aimem
documents, a peer, the coordinator, then a human. Every answer names its
sources and their freshness; unknown, conflicting and expired evidence is
stated and never becomes authority. One delegation hop, bounded retries and
waiting time; substantial investigation becomes a coordinator-issued attempt.
Questions and answers never transfer ownership, decide policy or grant
permission. Escalate to a human with the question, task, attempted
resolution, evidence, options, impact and exact pending request. An expired
escalation releases work the way the hub allows: an offer is declined;
accepted work is cancelled by the coordinator, acknowledged as stopped by the
worker and closed with close-stop. Templates and details: aimem
docs/TEAM-PLAYBOOKS.md and docs/examples/team.

## Credentials and unavailable context

Use ordinary task credentials: user-scoped tokens follow live grants, while
project-scoped tokens require a checkout-local override. Check task-token
show-source from the configured root. A missing or invalid required override
must never fall back to a broader credential. Never store a token in Git or
substitute checkpoint/admin credentials for agent task work.

Unavailable, denied, disabled and cached context are distinct. Read the client
notice and retrieve a complete matching version if possible; do not treat a
partial bootstrap, missing checklist or cache as authorization. An offline cache
must match the selected repo, commit, manifest, project and credential context.
Use aimem process show --full for an over-budget unit. Report missing required
skills before the step that needs them.
