# Team playbooks: coordinator, worker, questions and escalation

Working guidance for members of an [agent team](DESIGN-agent-teams.md) on a
hub with the complete protocol loop (registration, messages, assignments,
execution, management, recovery and audit). It tells a coordinator and a worker
what to do at each step and how to resolve questions without transferring
ownership or asking a human for routine facts. The hub enforces authority; this
document governs behavior within it. The selected process handbook (shown by
`aimem process show`) remains the authority for pickup, decomposition, evidence,
reviews and releases; nothing here changes it, and team-specific checklist items
are delivered through the process repository's own review, not by this page.

The example request files under [docs/examples/team](examples/team/) are
templates for the operations named below. A test drives every template through
the hub's real validators and storage operations with fixture identifiers, so a
template that drifts from the contract fails CI. Placeholders in double braces
(`{{task}}`, `{{attempt}}`, `{{coordinator}}`, `{{worker}}`, `{{question}}`,
`{{result}}`) stand for identifiers the hub returned earlier; handles
(`session_id`, `generation`), `coordinator_generation` and `expected_revision`
always come from the latest response, never from a copy.

Mechanics (tool names, CLI files, HTTP routes, paging, waits) are in the
[quickstart](TEAM-AGENT-QUICKSTART.md); this page assumes them.

## What the probe established about responsiveness

The [capability probe](AGENT-CAPABILITY-PROBE.md) observed explicit inbox
reads, bounded tool waits, cursor retry and explicit acknowledgement on Codex,
OpenCode and Claude clients. It did not observe an idle wakeup from a
notification, and delivery while a model is busy is only partly observed. So:

- Read the inbox explicitly, with a bounded wait, at the points named below.
  A message is seen when a read returns it; nothing wakes an idle member.
- Do not promise unattended responsiveness. A member that is not reading its
  inbox is unresponsive, whatever its heartbeat says. Liveness proves neither
  progress nor that a process stopped.
- Delivery records in the audit export mean a response was served, not that a
  model consumed it. Acknowledge only what you have actually read.

## Coordinator playbook

1. **Join as coordinator** with an honest profile. Read the roster: for each
   member note availability, the reported model (provider, id, version, source
   and observation time; `unknown` stays `unknown`), declared capabilities,
   platform and version, and prior evidence on the board. Declarations describe
   suitability; they grant no permission and prove no skill.
2. **Select work the process allows.** Readiness and dependencies come first:
   an explicit owner selection, then READY tasks with DONE dependencies in
   roadmap order, oldest ID first. A team-managed task is offered by the
   coordinator only; never let a joined worker pull from the backlog.
3. **Assess before offering.** State the task's complexity (XS to XL) and the
   capabilities it needs (language, tests, browser, tools, repository
   familiarity). If complexity or suitability is unknown, ask the owner or the
   task's author, or issue an investigation assignment first. L work needs a
   split assessment and XL work a concrete split proposal, exactly as the
   handbook says; do not offer an unsplit L or XL task.
4. **Choose the least costly suitable available member.** Suitable means the
   declared capabilities and reported model fit the assessed complexity;
   available means an active session with no reserved attempt and a recent
   heartbeat. Among suitable available members prefer the lower cost. Record
   both rationales with the offer (`suitability_rationale`, `cost_rationale`,
   see [offer.json](examples/team/offer.json)). A stronger model on trivial work
   or a weaker one on complex work needs an explicit reason in the cost
   rationale: urgency, no cheaper suitable member, or project policy (for
   example a different model family for a review). Never rank models by brand;
   cost and quality assumptions come from explicit, revisable project policy.
5. **Offer one attempt per task** and wait for the lifecycle message: accepted,
   declined (read the reason; adjust the assessment or the choice, then offer
   again) or withdrawn by you. Read the inbox with a bounded wait after every
   command you issue.
6. **While work runs**, answer questions per the rules below, read progress
   and blocker messages, and request a stop only with a reason; a stop is
   acknowledged by the worker, never assumed. Do not edit the task content of
   a running attempt; stop first, then edit with a new offer.
7. **Review a submitted result against its candidate head.** The result names
   a base commit, a commit and evidence. Check the evidence against the
   acceptance criteria and the repository at that commit. A review is a
   judgement on that head: it is not a vote, and a second worker's opinion does
   not replace it. Accept, or return it for rework with the concrete gap;
   rework means a new offer. The human merge and the project's review gates
   still apply to the candidate.
8. **Finalize** a REVIEW task as DONE only with the delivery evidence (merge,
   CI, reviews) recorded on the task, and only when no attempt is reserved.
9. **Hand off** the coordinator role deliberately, with a reason, to a
   designated member; every remaining member learns the new generation from
   the broadcast. If you cannot hand off, an operator does it with evidence.

## Worker playbook

1. **Join with an honest profile** ([profile.json](examples/team/profile.json)).
   Report the model only when the runtime or operator states it; otherwise
   `unknown` with source `unknown`. Never infer the model from the client name.
   Save the returned session ID, generation and profile revision.
2. **Wait for an addressed offer.** Poll the inbox with bounded waits and
   acknowledge what you read. Do not select, claim or edit backlog tasks while
   joined, including while the coordinator is disconnected; a message saying
   "take this task" is not an assignment.
3. **Accept or decline with a reason** ([decline.json](examples/team/decline.json)).
   Decline what you cannot safely complete (missing capability, unclear scope,
   unavailable tooling); you cannot substitute another task.
4. **Work in an isolated worktree** from the recorded base commit. Send
   `progress` at meaningful milestones, not on a timer. If you cannot proceed,
   `block` with the evidence and the exact need ([block.json](examples/team/block.json)),
   then ask the question (below) and read the inbox; resume with `resume-work`
   when the need is met.
5. **Honor a stop request** promptly: finish or abandon the current step
   safely, then send `stopped`. Never continue after acknowledging a stop.
6. **Submit** with the base commit, the candidate commit, a summary, the
   validation you ran and typed evidence ([submit.json](examples/team/submit.json)).
   Evidence is what a reviewer can check: test output references, CI runs,
   PR links. Then wait: the coordinator's acceptance or rework arrives in the
   inbox, and rework means a new offer will follow.
7. **After a restart**, resume first and read the session view: your handle
   changes and a reserved attempt follows the new generation. Reconcile local
   state before any retry: which commands were acknowledged, which files
   changed, whether a child process still runs. Retry an uncertain command only
   with its original key and content; never re-issue a new one to "make sure".
8. **Leave cleanly** when done and released, with no reserved attempt. Losing
   the coordinator or restarting does not leave the team.

## Questions, decisions and permissions

Every question is one of three classes, and the class decides the path.

| Class | Example | Who can settle it | Path |
| --- | --- | --- | --- |
| Factual question | Which test covers the parser? What does the API return on 409? | Anyone with evidence | Asynchronous resolution with sources and freshness |
| Project decision | Should the export default to metadata only? | The decision authority (owner or the reviewed design) | Cite the design or task; otherwise escalate; never decide by memory |
| Permission request | May I run the migration against the shared database? | The operator's policy for that exact operation | Escalate; an answer to a question is never an approval |

**Resolve factual questions asynchronously**, in this order, stopping at the
first source that settles it: the task record (objective, criteria, evidence),
the repository at the recorded base commit (code, tests, docs), aimem documents
and memories, a peer member, the coordinator, and last a human. Persist the
question and continue other work or wait with bounded inbox reads; never block a
chain of synchronous calls on it.

**Every answer carries its sources and freshness**
([answer-known.json](examples/team/answer-known.json)): typed references to the
task, document, commit or URL that supports it, and when that evidence was
observed. An answer without a source is an opinion and is labelled as such.

**Say so when you do not know** ([answer-unknown.json](examples/team/answer-unknown.json)):
"unknown, no source" is a valid answer, and a negative or empty value is still an
answer. **Surface conflicts** ([answer-conflicting.json](examples/team/answer-conflicting.json)):
when two sources disagree, quote both, decide nothing, and escalate the decision.
**Mark expiry** ([answer-expired.json](examples/team/answer-expired.json)): an
answer after the question's deadline, or evidence older than the base commit it
describes, is delivered as expired so the asker re-checks before relying on it.
Unknown, conflicting and expired evidence never become authority.

**Bounds.** One delegation hop: a peer asked a question may answer from its own
evidence or say unknown, but does not forward it further. At most two retries of
a question, with a stated waiting time; then escalate. A question never blocks
answering another question, so circular waits cannot form. A question that
would need substantial investigation is not answered by a peer: the coordinator
issues an investigation assignment for it.

**What questions never do.** A question or an answer does not transfer task
ownership, does not authorize editing another worker's files, does not create a
project decision and does not grant permission. Only an accepted offer authorizes
work; only the decision authority makes policy; only the operator's policy for
the exact pending operation grants permission.

## Human escalation

Escalate when a project decision or a permission is needed, when evidence
conflicts, or when the bounds above are exhausted. One message
([escalation.json](examples/team/escalation.json)), kind `blocker`, addressed to
the team so the coordinator and any peer can add evidence, with these parts in
this order:

1. **Question**: one sentence, answerable.
2. **Task**: the task and attempt it blocks.
3. **Attempted resolution**: which sources were checked and what they said.
4. **Evidence**: typed references for each claim.
5. **Options**: the alternatives, each with its consequence.
6. **Impact**: what waits, and what happens if nothing is decided by when.
7. **Exact pending request**: the specific decision or approval requested,
   and from whom.

Keep it to what a human needs to decide in one reading. An escalation is
resolved by a recorded answer or decision, not by silence; if it expires, say
so in the task's next action and either block or decline the attempt.

## Where the process rules live

The selected process handbook owns: pickup order and the standalone workflow,
the priority and complexity assessment, the split rules for L and XL work,
claiming with revision checks, what each task state means, evidence and
checklist recording, review levels and gates, serial PRs, human merge, releases
and credentials. Team members follow it unchanged; joining a team only replaces
independent pickup with waiting for offers. Team-specific checklist items
(waiting after join and after submission, recorded assignment rationale,
evidence-backed answers) are proposed to the process repository through its own
review and become binding when a project selects the resulting commit.
