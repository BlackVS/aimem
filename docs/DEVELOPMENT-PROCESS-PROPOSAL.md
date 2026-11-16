# Development process proposal: goal-driven planning

Status: **proposal, not adopted.** Nothing in this document is a rule yet.
It proposes a change to how work is chosen and how progress is seen in the
development process that the aimem and aicrew projects follow
([the process handbook](../processes/aimem/handbook.md) and the pickup
policy in [the roadmap](ROADMAP.md)).

The owner fixed the order in which it may become one (2026-11-16):

1. This proposal is written, reviewed locally and externally, and corrected.
2. Then the people and agents who develop these projects work by it
   themselves, by hand, for about a week ([section 9](#9-the-trial)).
3. Only if that week shows that it works is anything built into the
   products ([section 10](#10-later-in-the-products)).

No code, no board field and no handbook rule changes with this document.

## 1. The problem

### 1.1 What the process says today

The handbook names an order: "the roadmap owns delivery order", and an
agent picks "eligible READY work with DONE dependencies in roadmap order,
then oldest task ID". It asks for a priority (P0 to P3) and a complexity
(XS to XL) on every task, to be put "in next_action" until structured
fields ship, and adds that "these estimates do not change the current
pickup order". The roadmap repeats the pickup rule and plans a later one in
which priority ranks READY work first.

The board knows two levels: epics, which "group work" and "do not rank
it", and tasks.

### 1.2 What happened

On 2026-11-16 the three repositories of this project merged eleven pull
requests and published three releases. The aicrew project's nearest goal
that day was the one its board states: a first cycle in which an aicrew
team on separate hosts delivers an aicrew task. None of the eleven was on
the way to that goal, and by the test this document proposes neither was
the one larger task in progress ([section 8](#8-a-worked-example)).

Every one of the eleven was justified. One fixed a defect a user had met
that morning. Others closed a risk accepted the same day, followed up a
review, prepared a release, or were a cleanup. The work was real and its
reviews found real faults in it. It was still not the work that moves the
goal.

Nobody saw this until the owner asked where the project stood on its
roadmap. The first answer, "at the end of step one of five", needed the
board's epics and several task lists read by hand, and it answered for a
longer path than the nearest goal without saying so.

The same remark had been made before, on 2026-10-29: many small pull
requests, and no closer to the pilot.

### 1.3 Why it happens

- **The roadmap cannot order a task.** It is prose about phases. It says
  that a pilot comes after onboarding; it does not say which of seventy
  backlog tasks the pilot needs. So "roadmap order" is decided in the head
  of whoever assigns the queue, each time anew.
- **Nothing stands between an epic and a task.** An epic is too large to
  be done or not done on a given day, and a task is too small to say what
  the goal needs. The outcomes in between (a member can hold its own
  credential, the hosts exist) are where a goal is reached or missed, and
  the board has no record for them. Today they exist only as an informal
  habit: a "parent" task with children.
- **A finding is queued when it is filed.** A defect found now is fresh,
  small and well understood, so it goes to the front. Filing it is right.
  Putting it in front of the goal's work is a separate decision that nobody
  takes on purpose.
- **Priority measures the item, not its contribution.** P1 on a task says
  the task matters by itself. It does not say whether the goal can be
  reached without it.
- **No figure states the distance.** The board counts tasks by state. It
  cannot say how much of the way to the goal is covered, so leaving the
  way costs nothing visible.
- **A person on the path is invisible.** When the next necessary step is
  one only a person can take, no task says so, and the agents fill the wait
  with whatever is at hand.

## 2. The proposal in one paragraph

Work has three levels, each kept where it fits. The **specification** is a
document. The **goal** is an epic with a check that passes or fails. Its
**features**, the intermediate outcomes the goal needs, are records on the
board like tasks, each with its own check, an owner and a state, and each
marked mandatory or optional **for that goal**. **Tasks** are all of one
kind. The levels are tied by links the board already has: a goal lists
its features, a feature depends on its tasks, a task on whatever it waits
for.
A task's place then follows from what depends on it, and work is picked in
three tiers: the path (everything a mandatory feature depends on), then
what comes next (the same for optional features), then support work, the
tasks nothing on the way depends on, which has a budget. A short course
check, once a working day, says how far the goal is, what it waits on, and
whether the picks followed the tiers. All of it is plain counting over the
board, so it can later be automated; first it is done by hand.

This is the owner's graph of goal and tasks in its smallest form. The
nodes are the goal, its features and their tasks. The edges are the
goal's list of features and the dependencies below it. The weight of the edge between a goal and a feature is its
mark; the weight of a task is its complexity. The algorithm is the three
tiers, and the criterion says when the goal counts as reached.

It is close to specification-driven development, with three differences.
The specification is not frozen: the features are what its analysis yields
today, and they are derived again when the plan is changed on purpose.
Whether a feature is needed is relative to a goal, not a property of the
feature. And work the specification never mentions (a defect, a cleanup)
has a defined place and a bound.

## 3. The model

### 3.1 Specification: a document

What is to be built, and why, is written as a document: a design, a
proposal, a contract. A person, an agent or both write it; it lives where
documents live, in the repository or as a shared document, with versions
and review. This proposal changes nothing about it. A feature refers to
the part of the specification it comes from.

### 3.2 Goal: an epic with a check

A goal is an outcome with a check that passes or fails, not a list of
functions. "A team on separate hosts delivers one task of its own project,
and the delivery is confirmed" is a goal. "Improve onboarding" is not.

On the board a goal is an epic. An epic already has an objective and a
target, and every task already names its epic. What the epic gains is the
check, the criterion ([4.2](#42-the-criterion)) and the ordered list of
its features.

### 3.3 Features: records on the board, each with a check

A feature is an intermediate outcome the goal needs: "every member can
hold its own forge credential". It is a record on the board, for the same
reasons a task is one: it has a state, an owner, a history and links, and
text in a document has none of these.

- Its **check** says when it is done: "the product's command writes a
  credential into a member's home". A feature is DONE when its check
  passes and the evidence is recorded, not when its tasks are closed.
- Its **owner** is a worker or a person.
- Its **dependencies** are its tasks.

Features are few, ten to fifteen for a goal; a goal that needs more is two
goals. The goal lists its features in the **order of delivery**, set on
purpose by the architect and the owner. A feature whose content is not yet
known (a design comes first) stands early, because its result changes the
rest.

### 3.4 Mandatory belongs to the link, and has two grounds

Each feature is marked **mandatory** or **optional for this goal**. The
mark belongs to the link between goal and feature, because the same
feature can be optional for the nearest goal and mandatory for the one
after it.

A feature is mandatory on one of two grounds, and the goal says which:

- **The check needs it.** The goal's check cannot pass without it by any
  means, a manual one included. A credential that an operator can place by
  hand makes "credentials are deposited through the product" optional for a
  first cycle.
- **The owner set it as a condition.** The goal's check could pass
  without it, but the owner will not attempt the goal before it holds:
  "the cycle is proven in a simulation first". Such a condition is recorded
  with its date and is exactly as wide as the owner stated it.

Everything else is optional.

### 3.5 A task's place comes from what depends on it

Tasks are all of one kind. A defect, a fix, a cleanup and a new function
are tasks alike: each has a priority, a complexity and dependencies. What
differs is where a task stands, and that follows from one thing the board
already records: **what depends on it.**

- A task that a mandatory feature depends on is on the path.
- A task that such a task cannot proceed without is on the path too, from
  the moment that dependency is recorded, whatever kind of work it is. This
  is what puts a defect or a fix first when later work rests on it: it is
  not an exception to the order, it is the order.
- The same holds one tier lower for optional features.

So a task's importance for the goal follows from where it stands, not
from a label chosen for it, and it changes by itself when a dependency is
added or a feature's mark changes. Nobody rates a task by hand.

A task is linked to a feature in two steps. Whoever files it names the
feature it claims. Whoever promotes it to READY confirms the claim against
the feature's check and adds the task to the feature's dependencies; a
task that does not move the check is not that feature's task. Only the
confirmed link counts.

Work that makes a person's step on the path shorter (a checklist for the
hosts, a runbook for bringing a team up) is a task of that feature.

Priority and complexity stay as they are. Priority orders the tasks of
one feature and orders support work. Complexity remains the size.

### 3.6 Support work

Support work is not a kind of task. It is the name for the tasks that
nothing on the way to the goal depends on: a review follow-up nobody waits
for, a cleanup, hardening, a convenience, a defect off the way. It is not
bad work. It is work that does not move the goal, and the process must
show that plainly and bound it. A support task stops being one the day
something on the way is found to depend on it.

### 3.7 A person's action is part of the plan

A step that only a person can take (provide hosts, decide between two
designs, issue a credential) is a feature or a task like any other, with
the person as its owner and a record on the board. Then the plan shows
when the path runs through a person, and since when.

### 3.8 Alternatives

Where a feature can be reached in more than one way, its record names the
ways. The cheaper way is what makes a feature optional under
[3.4](#34-mandatory-belongs-to-the-link-and-has-two-grounds); naming it
keeps that judgement open to review.

## 4. Choosing the next task

### 4.1 Three tiers

For a worker, among the tasks it may take that are READY with their
dependencies DONE:

1. **The path:** a task that a mandatory feature depends on, directly or
   through other tasks; the earliest feature in the goal's order first.
2. **Next:** the same for optional features, the earliest first.
3. **Support work,** within its budget ([4.3](#43-the-budget-for-support-work)).

Among the tasks of one feature and among support tasks, the order is
priority, then oldest task id, as today. A task with unfinished
dependencies is not eligible, so the task it waits for is the one that is
picked: the order among dependent tasks needs no rule of its own.

The rule binds the one who assigns the queue as much as the one who picks:
before support work is assigned to a worker, the next task of tier 1 or 2
that this worker could take is made READY, or the reason it cannot be is
recorded.

The handbook's rule of one active pull request per repository is not
changed. A worker whose pull request waits for reviewers takes a task that
needs no pull request in that repository, or work in another repository,
by the same tiers.

### 4.2 The criterion

The owner chooses one of two criteria for a goal:

- **Minimal:** the goal is reached when its mandatory features are done.
  Optional features left over become the material of the next goal.
- **Complete:** the goal is reached when all its features are done.

The criterion decides when the goal is declared reached and what the
distance counts. It does not change the order of the queue: under both,
the path comes first and optional features follow in the goal's order.
This is deliberate. Under the minimal criterion a worker who has nothing
on the path does not turn to support work; it starts on what the next goal
will need.

### 4.3 The budget for support work

Support work has a budget: **for every three tasks of tiers 1 and 2 that a
worker delivers, it may deliver one support task**, counted over a
calendar week. The figure is the owner's to set
([section 12](#12-open-questions-for-the-owner)). The budget may be spent
at any moment, also while path work is READY; it is a budget, not a place
at the end of the line. A support task taken while a pull request waits
for reviewers counts against it like any other.

A worker for whom no task of tier 1 or 2 exists at all is not bound by the
budget, and that fact is itself reported in the course check, because it
means the goal does not reach that worker
([7.5](#75-one-goal-several-projects-and-workers)).

Most of what used to jump the queue needs no exception under
[3.5](#35-a-tasks-place-comes-from-what-depends-on-it). A defect that
makes a feature's check fail again makes that feature not done, and its
fix is a task the feature depends on. A fault that later work rests on, a
release that a task on the way waits for, a fix that unblocks the next
step: each is recorded as a dependency of the task that waits, and is path
or next-tier work by that link.

One thing passes ahead of the tiers and outside the budget: a task of
priority **P0**, which the handbook already calls urgent. P0 is for work
that cannot wait whatever the goal is: a defect that stops a user's work
now and has no workaround, or a security finding that the project's own
rules class as blocking. The one who assigns the queue sets P0 and records
the ground on the task. Urgent tasks are counted and listed in every
course check, so their number is seen. Anything else over the budget needs
the owner's word.

## 5. Seeing progress

### 5.1 The course check

Six lines, counted from the board:

1. **The goal** and its criterion.
2. **The distance:** features that are DONE, out of the features the
   criterion counts, and how many features still wait for a design.
3. **The wait:** the earliest feature on the path that is not done, who
   holds it, and since when.
4. **The picks:** tasks delivered since the last check in each of four
   groups: path, next, support within the budget, urgent; and separately
   any support task over the budget, with whose word. A task that stood on
   the path or next only through another task's dependency is counted
   there and named, with the task that waited for it.
5. **Out of order:** tasks taken while a task of a higher tier was READY
   for the same worker without a recorded reason. The aim is none.
6. **The change proposed** to the features or the queue, or "none".

The distance is coarse on purpose: with six features of unequal size it
moves rarely. Lines 3 and 5 are the ones that change from day to day.

### 5.2 Where and when

The check is a comment on one board task kept in the goal's epic for this
purpose, the goal's log. So every session and the owner read one series,
and its history is the board's. It is written once per working day by
whoever assigns the queue, and after a release.

A feature on the path that has waited on a person for three working days
is put as the first line of the check until it moves.

### 5.3 Replanning

The features and their order change only on purpose, in a course check,
when one of these happens: a design is finished and its feature can be
filled in; a task is reassessed to a larger complexity; a feature's check
turns out to be wrong; three support tasks of one kind appear in a week
([7.6](#76-a-task-that-fits-no-feature-keeps-appearing)); the
specification changes; the goal's check or criterion changes. The change
is made on the board, whose revisions keep the history, with its reason in
the log's comment of that day.

## 6. The first version: what is done by hand

No tooling, no new board field, no change to a repository. The board's
existing records carry everything.

**The goal is its epic.** The epic's text holds the goal's check, the
criterion, and the ordered list of its features: for each, the id of its
record, its mark with the ground, and the alternative if there is one. It
holds no state. The project's roadmap gets one line pointing to the epic.

**A feature is a board record in that epic.** Until the board has a
record kind for it, it is a task whose title begins `[feature]`, the way
"parent" tasks are kept today. Its acceptance criteria are the feature's
check; its assignee is its owner, a person included; its dependencies are
its confirmed tasks; it is DONE when its check passes, with the evidence
on it. So "waiting on a person" is a record in a state with a date, and
the distance is a count of records.

**A task names the feature it claims in its title,** as
`[P1/M, credentials]`, or `[P2/S, support]`. The title is the claim and a
reading aid, because the board's lists and its search show titles; the
feature's dependencies are the record, added when the task is promoted to
READY, and where the two disagree the dependencies count. Both projects
already write priority and complexity in the title, although the handbook
says `next_action`; the difference from the handbook's wording is part of
what the trial tests. An existing task is claimed and confirmed when it is
promoted to READY, where the handbook already asks for old tasks to be
assessed; until then it is not picked. A task that another task waits for
is listed in that task's dependencies, as today.

**The goal's log** is one task in the epic; the course checks of
[5.1](#51-the-course-check) are its comments.

**The tiers of [4.1](#41-three-tiers)** are applied by whoever assigns the
queue. For the trial that is the architect session, which also tells the
workers the rule; no process set or handbook is changed for it.

Left out on purpose: numeric weights, estimates in days, a separate board
or a drawn graph for features, a computed ordering, charts. One of them is
added only if the manual version shows the need.

**What it costs.** Setting a goal up is one epic text and about ten
feature records, once. A course check lists the feature records of the
epic and the week's deliveries: five to ten minutes. One per working day
and one per release is about seven a week, so thirty-five to seventy
minutes a week for the one who assigns the queue. Filing a task costs one
more decision, and promoting one to READY one more confirmation and one
edit of a feature's dependencies. Whether that is acceptable is for the
owner to say, and the trial measures it.

## 7. Where the proposal is weak

### 7.1 Features multiply into a second backlog

They will, if a feature is allowed to be a large task. A feature is an
outcome with a check, and a goal has ten to fifteen. A feature that
collects more than about six open tasks is either badly cut or a goal of
its own; that is a reason to replan. During the trial the feature records
stand among the tasks on the board, marked only by their title, which is
clutter a later board view removes ([section 10](#10-later-in-the-products)).

### 7.2 Everything gets marked mandatory, or attached to a mandatory feature

The mark is set on features, by the architect and the owner, on one of two
stated grounds. Each course check prints the count of features on the
path, so its growth is seen, and adding one is a replanning event with a
recorded reason. A task is added to a mandatory feature's dependencies
only if it moves that feature's check, confirmed at READY.

This remains the weakest point. Both grounds have an honest answer only
when the goal's check is concrete; a vague goal makes everything look
necessary. And the first table drawn for this document made exactly this
mistake before its review ([section 8](#8-a-worked-example)).

### 7.3 Support work is starved, and quality drops

The budget can be spent while path work is READY, a fault that later
work rests on is path work by its dependency, P0 covers what cannot wait,
and a worker the goal does not reach is not bound at all. If the trial
shows defects piling up, the owner raises the figure. The trial counts
open support tasks at its start and its end for this reason.

### 7.4 Everything becomes a dependency, or urgent

A dependency is the cheapest way to move a favourite task onto the path,
and "stops a user's work now" is elastic where the users are the
developers themselves. The defences: a dependency is recorded on the task
that waits, which must itself be a confirmed task of a feature, and the
one who promotes that task to READY confirms that it cannot proceed
without the other; P0 is set by the one who assigns the queue, not by the
one who wants the task, and a workaround removes its ground; every check
names the tasks that stood on the way only through another task and lists
the urgent ones. If a week shows more of those two together than tasks
confirmed directly on features, the links are being used to go around the
order.

### 7.5 One goal, several projects and workers

The goal of 2026-11-16 belongs to aicrew, and the aimem roadmap makes the
same goal aimem's: "only aicrew-required aimem prerequisites are
eligible". A task of another project that a feature depends on is linked
the same way and counts in the same course check; the goal's epic and its
feature records stay on the board of the project that owns the goal.

The harder case is a worker the goal does not reach.
[Section 8](#8-a-worked-example) finds that for the nearest aicrew goal no
aimem task is on the path or next. Then the tiers alone change nothing for
the aimem worker. Either that worker takes goal work in another
repository, or its project has a goal of its own with its own epic and
check, or its work is support work by the owner's decision. The proposal
cannot choose among these; it is the first open question, and the trial
does not start for aimem until it is answered.

More than two goals for one group of workers is a sign that none of them
is the nearest.

### 7.6 A task that fits no feature keeps appearing

Then a feature is missing, or the goal is cut wrongly. Three such tasks of
one kind in a week are a reason to replan.

### 7.7 The plan is wrong

Features, marks and their order are judgements, and a confident figure
from wrong judgements is worse than no figure. The answers are the
replanning events of [5.3](#53-replanning), the count of features waiting
for a design, and the trial.

### 7.8 It conflicts with the pickup policy already planned

The roadmap plans task metadata and per-project pickup modes in which
"priority ranks eligible READY work ... before roadmap order and task ID",
with "priority-first" as the default. That plan ranks by the item's own
priority, which is a cause named in [1.3](#13-why-it-happens). If this
proposal is adopted, goal-first becomes the default mode and priority
ranks inside a tier; the planned modes remain as what ranks inside it. The
follow-up list in [section 10](#10-later-in-the-products) says so.

### 7.9 It costs more than it saves

The estimate is in [section 6](#6-the-first-version-what-is-done-by-hand)
and the trial measures it.

## 8. A worked example

The aicrew project's nearest goal, as its epic states it: **a first cycle
in which an aicrew team on separate hosts delivers an aicrew task.** The
check: verify-delivery confirms the delivery of that task.

The features as the epic would list them. Each row would be a record on
the board; the table holds what the goal says about a feature, not its
state.

| # | Feature and its check | For this goal | Owner | Alternative |
| --- | --- | --- | --- | --- |
| 1 | The cycle is proven in a simulation: the nightly simulation passes its cycle from a task to the delivery record | mandatory, the owner's condition of 2026-10-29 (increments I2, I3 and I5) | worker | none |
| 2 | Turns start fresh from a brief: no turn resumes a conversation | mandatory, the owner's condition of 2026-10-15 | worker | none |
| 3 | A member can hold its own forge credential: the product's command writes one into a member's home | mandatory, the check needs it | worker | none |
| 4 | The members' hosts exist: one user and one client login each | mandatory, the check needs it | the operator | none |
| 5 | The team is brought up on those hosts: each member joined with its credential, its service running | mandatory, the check needs it | the operator, with the architect | none |
| 6 | The cycle is run: one task offered, delivered and confirmed, with the turn's cost measured | mandatory, it is the goal's check | the team | none |
| 7 | The architect is a role in the product | optional | worker | an architect session does it by hand |
| 8 | The operator has a seat in the product | optional | worker | decisions are recorded on the task by hand |
| 9 | Ordinary pull requests are merged by the product's rule | optional | worker | done by hand |
| 10 | Credentials are deposited through the product | optional | worker | feature 3, placed by hand |
| 11 | An upgrade between releases is proven in the simulation | optional | worker | none; a first cycle installs fresh |

Features 7 and 8 stand first among the optional ones because each begins
with a design whose result is not known.

Three of these already exist on the board as the informal kind of record
the proposal makes regular: the simulation's design task with its
increments, the turn model's task with its five parts, and the credential
task that calls itself the parent of four others. Features 4 and 5 have no
record of any kind.

**One course check, as read on the board on 2026-11-16:**

1. Goal: the first cycle on separate hosts. Criterion: minimal.
2. Distance: 3 of 6. Features 1, 2 and 3 are done: the simulation's
   increments through I5, the turn increments 0, 1 and 3a to 3d, and the
   credential tasks `354c-1a`, `1b` and `2` are DONE. No feature on the
   path waits for a design.
3. The wait: feature 4, held by the operator. Two searches of the board
   found no task for it and none for feature 5, so "since when" has no
   date. That absence is the finding.
4. The picks that day: path 0, next 0, support 11, urgent none; nothing a
   feature depends on waited for any of the eleven.
5. Out of order: three in aicrew ([8.1](#81-the-rule-replayed-on-that-day)).
6. Change proposed: create the records of features 4 and 5; a worker
   prepares what shortens them (a host checklist and a bring-up runbook are
   tasks of those features); the design of feature 7 is made READY as the
   next work.

Under the **complete** criterion the same check reads: distance 3 of 11,
two features waiting for a design (7 and 8), the queue unchanged. The next
tasks are the same under both criteria; what differs is that under the
complete one the goal is not reached until features 7 to 11 are done,
which is the path the board's epic lists as five steps.

What the example shows:

- The answer given on 2026-11-16, "at the end of step one of five", was
  the complete criterion's distance, given without naming the criterion.
  For the nearest goal the path waited on steps of a person that were not
  on the board.
- The first table drawn for this document marked the simulation as a
  whole mandatory and counted its upgrade scenario, the one larger task in
  progress that day, as path work. The review of the document applied the
  test of [3.4](#34-mandatory-belongs-to-the-link-and-has-two-grounds) and
  found that wrong: the owner's condition named three increments, and they
  were done. Feature 11 is the result. The mistake is recorded here because
  it is the one [7.2](#72-everything-gets-marked-mandatory-or-attached-to-a-mandatory-feature)
  predicts.
- Building the list found four stale records: two tasks BLOCKED since
  September and October, and two left in REVIEW since early October.

What the example does not establish: that a first cycle on real hosts
will meet no fault the simulation missed. The features say what is known
to be needed, not that it is sufficient.

### 8.1 The rule replayed on that day

Of the eleven pull requests merged on 2026-11-16:

- **Three were in aicrew** (two dependency updates and a workflow change).
  Optional features 7 to 11 existed and none of their tasks had been made
  READY. Under [4.1](#41-three-tiers) the assigner makes the next such
  task READY first, and the worker had delivered none that week, so its
  budget was empty: all three would have waited behind next-tier work.
- **Eight were in aimem and its skills repository** (two fixes after a
  user's report, a hook, a cleanup, a check, three release preparations).
  One of the fixes might have been urgent, but the user had a workaround
  within minutes, so by [4.3](#43-the-budget-for-support-work) it was
  not, and nothing a feature depends on waited for either fix. For that
  worker no task of tier 1 or 2 existed, so the budget did not bind, and
  the rule would have deferred none of the eight.

So the rule by itself would have changed three of eleven. The other eight
show the limit described in
[7.5](#75-one-goal-several-projects-and-workers): until the goal reaches
every worker, the larger part of that day stays as it was.

## 9. The trial

**What is done, for five working days:** the first version of
[section 6](#6-the-first-version-what-is-done-by-hand) on the aicrew goal,
starting from the features of [section 8](#8-a-worked-example) as its
reviews leave them. aimem joins once the first open question is answered.

**What is measured,** all from the goal's log and from the board:

| Measure | Read as |
| --- | --- |
| Out-of-order picks per day (line 5) | the rule's purpose; the day replayed in 8.1 had three |
| Picks by group per day (line 4) | where the work went |
| Urgent tasks and tasks on the way only through another task, against tasks confirmed on features | whether the links were used to go around the order |
| Minutes to set the goal up, and to write each check | the cost |
| Working days a path feature waited on a person | whether a person on the path became visible and was acted on |
| Open support tasks at the start and at the end | whether support work was starved |
| Changes to the features, with their reasons | whether the plan was changed on purpose |

**The owner judges the result,** on that record, against these readings:

- **Stop** if the checks took more than ten minutes on three of the five
  days, or if any of the last three days had three or more out-of-order
  picks, as the replayed day did, or if urgent tasks and tasks on the way
  only through another task together outnumbered the tasks confirmed on
  features.
- **Build** if it did not stop and the minutes went mostly into the same
  mechanical steps: finding the tasks of a feature, reading their states,
  sorting the week's deliveries into groups. Those steps are then what is
  built first.
- **Continue by hand** otherwise, for another stated period.

A week in which the path waits on a person the whole time is a valid
result, not a failed trial: line 3 then shows it every day, and the picks
show what the workers did instead.

## 10. Later, in the products

A sketch only; nothing here is designed.

**aimem** owns tasks and the board, so it would hold what the first
version writes by hand:

- a feature as a record kind of its own, beside the task and the epic,
  with its check, owner and state;
- the link between a goal and a feature carrying the mark, its ground and
  the order, since the same feature can belong to two goals with different
  marks;
- the feature and the complexity as fields of a task, in place of the
  title's bracket, and a reference from a feature to the part of the
  specification it comes from;
- on the board: features shown in a colour of their own; a view of the
  features alone; a view of the tasks of one chosen feature; and the
  dependencies between goal, features and tasks drawn as a graph;
- computed views of the next task per assignee and of the course check.

**aicrew** owns teams and roles, so they would use those views: the
coordinator takes the next task from the computed order, the architect
keeps the features and writes the course check, and the owner sets the
goal, the criterion and the budget, and reads the check.

Follow-up tasks, in order, to be filed only after the trial and only on
the owner's word:

1. The process set and the roadmap's pickup policy carry the three levels,
   the tiers, the budget and the course check; the repository instructions
   that point to the pickup policy follow.
2. The planned task metadata and pickup modes (the two backlog tasks the
   roadmap names) are revised: the feature becomes a field beside priority
   and complexity, goal-first becomes the default mode, and the planned
   modes rank inside a tier.
3. aimem: the feature record and its link to a goal.
4. aimem: the board views and the computed views.
5. aicrew: the coordinator's selection and the architect's course check.

## 11. What is not proposed

- A scheduling tool, or estimates in days.
- A change to how a specification is written or reviewed.
- A change to how work is reviewed, merged or released, or to the rule of
  one active pull request.
- A new priority scale. P0 to P3 and XS to XL stay.
- Reclassifying the backlog. A task is linked to a feature when it is
  promoted to READY.
- Stopping support work.

## 12. Open questions for the owner

1. **A worker the goal does not reach**
   ([7.5](#75-one-goal-several-projects-and-workers)). For the aimem
   worker today: goal work in another repository, a goal of aimem's own
   (closing its security audit is the candidate), or support work by
   decision? The trial needs this answered for aimem.
2. **The budget.** One support task for three of tiers 1 and 2 is
   suggested. What figure?
3. **The cost.** One setup per goal, then thirty-five to seventy minutes
   a week for the one who assigns the queue. Acceptable?
4. **Who sets a feature's mark.** The architect and the owner together is
   proposed. Is the owner's word needed for every mark, or only where the
   two disagree?
5. **Features as tasks during the trial.** About ten records per goal
   stand among the tasks on the board, told apart only by their title,
   until the board has a kind and a view for them. Acceptable for a week?
6. **The feature in a task's title.** It departs from the handbook's
   wording and lengthens titles. Acceptable for the trial?
7. **Three working days** before a person's step is raised to the first
   line of the check. Too long?

## Appendix: the wording this would become

Not applied by this document. If the trial ends in "continue" or "build",
the handbook's section "Scope, pickup and decomposition" and the roadmap's
pickup policy would gain text of this kind:

> A goal is an epic with a check, a criterion and an ordered list of
> features. A feature is a record with its own check and owner, marked
> mandatory or optional for that goal; its dependencies are its tasks. A
> task claims a feature when it is filed, and the claim is confirmed, by
> adding it to the feature's dependencies, when the task is promoted to
> READY; a task that another task waits for stands where that task
> stands; a task nothing on the way depends on is support work. Pick
> eligible READY work in this order: what a mandatory feature depends on,
> the earliest feature in the goal's order first; then the same for
> optional features; then support work within the budget the owner set.
> Before assigning support work, make the next task of the first two
> kinds READY. A task of priority P0 passes ahead, with its ground
> recorded by the assigner. Once per working day and after each release,
> write the course check as a comment on the goal's log: the goal, the
> distance, the wait, the picks, the picks out of order, and the change
> proposed.
