# The course check's draft and the check of the plan

Status: **proposal**, for the owner's review of the process trial before
any code. It designs the first part of the goal's feature "Computed views
and helpers for the process, through the API and MCP" (sketched in
[DEVELOPMENT-PROCESS-PROPOSAL.md](DEVELOPMENT-PROCESS-PROPOSAL.md),
section 10). That part is two reads a session can call:

- **the course check's draft:** the counted lines of
  [the course check](DEVELOPMENT-PROCESS-PROPOSAL.md#51-the-course-check),
  for one goal and one period;
- **the check of the plan:** the places where the board contradicts the
  plan.

Both are composed by the client from reads the hub already serves. The next
task for an assignee, line 5 of the check and the budget per worker are
left as questions for the review ([section 8](#8-questions-for-the-owners-review)).

## 1. Why these two

The trial keeps its course checks by hand, as comments on each goal's log
task. Its first three days cost about 50 minutes of a session's time, almost
all of it in the daily checks: 3 minutes on the first day, 10 for both goals
on the second, 10 at a release, and 15 for both on the third. A check's lines
are of two kinds:

| Line of the check | Counting, ordering or checking | Judgement |
| --- | --- | --- |
| 1. The goal | copied from the epic | — |
| 2. The distance | all of it | — |
| 3. The wait | the earliest feature not done, and since when | who holds it, and why |
| 4. The picks | the delivered tasks by tier | whether a support task over the budget had the owner's word |
| 5. Out of order | which tasks were taken while a higher tier was READY | the recorded reason |
| 6. The change proposed | — | all of it |
| Support filed since the last check | the count | fault or tidying |
| Dated tasks due within three days | all of it | — |
| Open tasks by state | all of it | — |
| Notes for the owner's review | — | all of it |

The counting half takes the gathering time: reading the features, finding what
was delivered since the last check, and sorting it by tier. The judgement half
is most of each check's text. The draft does the first half and leaves marked
places for the second. The check of the plan finds, from the same reads,
the records that make a count wrong before anyone counts.

### The feature's check after this design

**The check on the feature's record:** an agent gets the next task for an
assignee, and a draft of the course check's lines 2 to 5, from one call each,
and the results equal what the architect counted by hand.

**After this design the check is:**
- `aimem process draft` gives lines 2 to 4, with the support filed, the dated
  tasks and the open counts, in one call;
- `aimem process plan` gives the plan's findings in one call;
- for the trial's days, the draft equals the hand-written check, or the
  difference is explained and one side named right, as in section 7.

**The rest of the old check waits for the owner's review:**
- the next task for an assignee waits for [Q1](#q1-the-next-task-for-an-assignee);
- line 5 waits for [Q2](#q2-line-5-out-of-order), which follows Q1;
- the budget per worker waits for [Q3](#q3-the-budget-per-worker).

## 2. The frame: composed in the client

Both reads are computed by the `aimem` client from routes the hub serves
today. Nothing on the hub changes: no route, no store field, no schema step.

| Read | Route | MCP tool today | Gives |
| --- | --- | --- | --- |
| The goal | `GET /v1/projects/{p}/epics/{e}` | `get_epic` | title, state, target, `created_at` |
| The goal's features | `GET /v1/projects/{p}/epics/{e}/features?include_archived=true` | `list_features` | each feature's id, title, state, position, mark, ground, `confirmed_tasks`, `tie` |
| A record | `GET /v1/tasks/{id}` | `get_task` | its `dependencies` (a feature's confirmed tasks), `next_action`, `blocker`, `created_at`, `updated_at`, `project` |
| A record's revisions | `GET /v1/tasks/{id}/history` | `get_task_history` | every accepted revision in order, each with its `state` and `updated_at` |
| A project's tasks | `GET /v1/projects/{p}/tasks?updated_since=T&include_archived=true` (also `state`, `epic`, `feature`, `assignee`, `q`) | `list_tasks` | the summaries changed since T: id, kind, title, state, assignee, epic, archived, revision, `updated_at` |
| A project's counts | `GET /v1/projects/{p}/tasks/counts` (also `epic`, `assignee`, `kind`) | the first page of `list_tasks` | the active tasks per state, the archived and the total |
| The goal log's comments | `GET /v1/tasks/{log}/comments?order=desc` | `list_task_comments` | the newest comments first, each with its `created_at`, for the window's start (section 3) |

**Calls.** For one goal and one day:
- the goal and its features: 2 calls;
- one record per feature with confirmed tasks;
- one or two pages of the project's tasks changed since T;
- one call for the counts per state;
- with `--log` and without `--since`, one page of the log's comments, newest
  first;
- one history per task whose state changed in the window.

The creation time of a filed task costs no call (section 4).

For the trial's days that is 15 to 30 calls, the same order as the board
page's features view.

**Why not on the hub.** A route and an MCP tool on the hub are a wire
protocol, which here is a sensitive surface merged by the owner. The reads
are not slow enough to need one, and the rules they compute are still being
tried. A route can follow once the rules have stood the trial and a client
other than `aimem` needs them ([section 9](#9-implementation-tasks)).

## 3. The rules both reads share

**The window.** A draft covers the period from T0 to T1:
- T1 is the time of the run, or `--until`.
- T0 is the time of the previous check, or `--since`. Without `--since` it is
  the `created_at` of the goal log's latest comment that starts with
  `**Course check`, if the log is named with `--log`; otherwise it is 24
  hours before T1.

**When a task was delivered.** It is the `updated_at` of the first revision in
its history whose state is DONE. The summary's `updated_at` is not used:
any later edit moves it. A task whose summary says DONE is a candidate only
if `updated_at >= T0`, which the list's `updated_since` filter gives. Its
history then decides whether DONE fell inside the window.
- **Promoted** and **taken** are changes of state, not states. A task was
  promoted in the window when a revision with `updated_at` in the window has
  state READY and the revision before it had another state. It was taken when
  such a revision has state IN_PROGRESS. A revision that keeps the state, such
  as an edit of the title of a READY task, is not a promotion. A task promoted
  again after it left READY (for a block, say) is counted again. A task's
  first revision counts as entering its state: a task created READY in the
  window was promoted in it, and one created IN_PROGRESS was taken.

**The tier of a task,** for a goal:
1. **Urgent** if its title's bracket starts with `P0`.
2. **Path** if a mandatory feature of the goal lists it among its confirmed
   tasks, directly or through dependencies: task X is on feature F's way
   when F's confirmed tasks, or a task they depend on (followed to any
   depth), depend on X.
3. **Next**: the same for an optional feature.
4. **Support**: otherwise.

The tier comes first and the position second. A task on the way of a
mandatory feature is path wherever the optional features that also wait for
it stand, because the proposal puts every mandatory feature's work on the
path ([section 4.1](DEVELOPMENT-PROCESS-PROPOSAL.md#41-three-tiers)).
Within its tier, the earliest feature by position is named.

Dependencies are read from each record (`GET /v1/tasks/{id}`); a dependency
in another project is followed if the caller may read it. One the hub
answers "not found" for is reported as such and not followed.

**Priority** is read from the title's bracket (`[P1/M, surface]`), because the
board has no priority field. A title without one has no priority, and the
draft says so.

**A dated task** is one whose title contains `DATED YYYY-MM-DD`, the trial's
own convention. It is due on that date.

## 4. The course check's draft

The command prints the six lines in the trial's form. Every line names the
records it was counted from, and every judgement is a marked gap
(`[architect: ...]`) for the writer to fill or delete.

1. **The goal.** The epic's title and target, copied. The criterion is not
   on the record (the epic's text holds it), so it is a gap.
2. **The distance.** From the goal's features:
   - DONE mandatory features, out of all mandatory features;
   - DONE optional features, out of all optional ones;
   - the count of not-DONE features with no confirmed task.

   An archived feature does not count. The criterion decides which pair is
   "the distance"; the draft prints both. The trial's "features that still
   wait for a design" is a judgement; the count of features without a
   confirmed task stands in for it and is labelled as such.
3. **The wait.** The earliest feature by position that is mandatory and not
   DONE; if there is none, "nothing on the path". After it comes the earliest
   not-DONE feature of all, with:
   - its state, and the `updated_at` of the revision that set it, from history;
   - its `blocker` and its `next_action`, quoted as they stand.

   Who holds it and why is a gap: the record's `next_action` is often the
   answer, and the writer confirms it.
4. **The picks.** The tasks delivered in the window, each with its tier, its
   id, its title and its delivery time, grouped as path, next, support and
   urgent. Then:
   - the tasks **promoted** to READY and **taken** in the window, by tier;
   - for a task on a feature's way only through another task's dependency,
     the feature and the task that waited for it, named.

   Whether a support task was within the budget is a gap
   ([question 3](#q3-the-budget-per-worker)).
5. **Out of order:** a gap, with the tasks taken in the window listed beside
   the READY tasks of a higher tier at that moment, if there were any. The
   draft does not judge it ([question 2](#q2-line-5-out-of-order)).
6. **The change proposed:** a gap.

After the six lines come:

- **Support filed in the window:** the project's tasks created in the window
  with tier support. The candidates come from `updated_since=T0`, because
  creation sets `updated_at`.
  - The list's summaries carry no `created_at`, so the creation time is read
    from the task's id instead. The store makes a task's id as a UUIDv7 at
    the moment it sets `created_at` (`internal/store/tasks.go`, `CreateTask`),
    and the id's first 48 bits are that moment in milliseconds. This costs no
    call.
  - An id whose version is not 7 is read as a record (`GET /v1/tasks/{id}`),
    one call each.
  - The implementation pins the property with a test, since it is the
    store's practice and not a documented contract.

  Fault or tidying is a gap per task.
- **Dated tasks due within three days of T1,** and any overdue dated task
  that is not DONE, first.
- **Open tasks by state:** the counts route's answer, as at the run.
- **The plan's findings** from section 5, if there are any.

**Output.** Markdown on standard output, ready to paste as the log's comment
after the gaps are filled. Its header names the goal, the window (T0 and T1)
and the time of the run, and says that only this project's board tasks are
counted. `--json` gives the same content as data
(features, tasks with tier and times, gaps) for a session that writes the
text itself. The command never writes a comment: posting is the writer's
act.

## 5. The check of the plan

The same reads, with every finding naming its records:

| Finding | Rule |
| --- | --- |
| A feature without a confirmed task | a not-DONE, not archived feature whose `confirmed_tasks` is 0 |
| A feature waiting on nothing | a not-DONE feature whose confirmed tasks are all DONE (the board page's "waits on nothing") |
| A DONE feature waiting on something | a DONE feature with a confirmed task that is not DONE |
| A tie | two features of the goal at one position (`tie` in the features list) |
| A feature record in a working state | a feature in READY, IN_PROGRESS or REVIEW, which a team's feed reads as work |
| Undeclared work in the goal's epic | a task in the goal's epic, DONE in the window or READY, that is on no feature's way and whose title's bracket does not declare it support (`support` among the bracket's terms, as in `[P1/S, support]`; [Q4](#q4-a-support-tasks-kind)) |
| A record standing still | a task or feature not DONE and not CANCELLED, unchanged for more than N days (`updated_at`; default 14) |
| A dated task due soon or overdue | as in the draft |
| Support delivered against the budget | support tasks delivered in the calendar week to T1, beside the path and next tasks, per project; per worker is [question 3](#q3-the-budget-per-worker) |
| An unreadable confirmed task | a confirmed task the hub answers "not found" for |

A finding is a fact, not a verdict: a feature may wait on nothing because its
check needs a person's action. The command prints the findings and exits 0;
`--strict` exits 1 when there are any, for a scheduled run.

## 6. How the result is offered

**A command of the client** (recommended, and the first step):

```
aimem process draft --goal <epic> [--project p] [--since T] [--until T] [--log <task>] [--json]
aimem process plan --goal <epic> [--project p] [--stale-days N] [--strict] [--json]
```

- `process` is the client's command for the project's process set already
  (`process show`, `process pin`).
- A session runs either command in one call through its shell.
- The cost is two subcommands and their tests, on an ordinary surface.

**A tool of the client's MCP server, as well** (not recommended now). It adds
no route on the hub, but it adds a definition to what every session lists:
- A standalone session's set is 37 tools and 43,469 bytes today, against the
  limit of 48,534 that [DESIGN-tool-sets.md](DESIGN-tool-sets.md) set and its
  inventory test enforces.
- One tool with a `view` parameter (`draft` or `plan`) and the options above
  would take about 1,200 to 1,600 bytes, a quarter to a third of what is left
  for every later tool of the goal.
- The tool would have to be placed in a mode's set, and the inventory test
  updated.

A session that has a shell gets the same result from the command. The tool
is worth its bytes only if sessions without a shell need the reads; the
trial has none.

## 7. Worked example: the aimem goal's check of 2026-11-18

**The day.** The check written by hand on the goal's log (comment seq 579,
created 2026-11-18 05:11Z) covers 2026-11-17 14:48Z to 2026-11-18 05:15Z.

**How the draft was computed.** By hand, with the reads and rules above,
read on 2026-11-18 at about 08:30Z. That time matters for two lines
(section 7.3).

### 7.1 The draft beside the check

| Line | The draft | The check by hand | Same? |
| --- | --- | --- | --- |
| 2 | mandatory 1 of 1 DONE (`01a1e572-19c0`); optional 1 of 4 DONE (`01a1e599-5d9a`); not-DONE features without a confirmed task: 1 at T1 (position 3, whose first confirmed task was filed at 08:14Z, after the check), 0 when read at 08:30Z | mandatory 1 of 1, optional 1 of 4; the records titled 2, 3 and 4 are BACKLOG | yes |
| 3 | nothing on the path. Earliest not done: position 3, `01a1e572-19cf`, BACKLOG; `next_action`: "After feature 1, and after the trial's review of 2026-11-22 or 2026-11-23." | nothing on the path; the optional features wait for the review by their own records | yes |
| 4, delivered | path 0, next 0, urgent 0. Support 2: `01a1e9b0-4134` (DONE 2026-11-17 16:17:35Z) and `01a150e8-6baf` (DONE 16:33:21Z) | path 0, next 0, support 2 delivered (#390, #391), urgent 0 | yes |
| 4, promoted and taken | support `01a150e8-6af7`, promoted 05:09:36Z and taken 05:10:38Z | "1 promoted today" (`01a150e8-6af7`) | yes |
| Support filed | `01a1ed0f-b983` (created 05:09:53Z), fault or tidying a gap | faults: `01a1ed0f-b983`; tidying: none | yes |
| Dated, three days | none; `01a1e579-6f2a` is due 2026-11-22, four days on | none; the review four days away | yes |

At the line level, the draft for this day equals the check. The check's other
text is judgement:
- line 5's ground;
- the finding about the budget in line 6;
- the records "added to existing tasks";
- the six notes for the review.

### 7.2 The week's budget, line 5 of the same check

The check reads: "the worker's week stands at four support tasks taken
against seven tasks of the upper tiers and two releases".

The board, from the goal's creation (2026-11-16 17:39:40Z) to 2026-11-18
05:15Z, gives:

| Tier | Tasks delivered |
| --- | --- |
| path | `01a1e572-1a04` (the design), `01a1e591-771f` (the store), `01a1e591-b297` (the routes), `01a1e7f0-8cf8` (release v0.11.0, confirmed on feature 1) |
| next | `01a1e599-b414` (the tool set's design), `01a1e929-fe13` (the sets), `01a1e949-902e` (the setting) |
| support | `01a1cf09-da31`, `01a1e800-028a`, `01a1e9b0-4134`, `01a150e8-6baf`, and `01a1e9d6-6573` (release v0.11.1, see 7.3) |

**The check's own picks add up to eight upper-tier tasks,** two of them
releases:
- path 2 and path 2 in the checks of 2026-11-17 (seq 537 and 565);
- next 4 in seq 565.

**"Seven tasks … and two releases" counts nine.** The figure that agrees with
the check's own picks is six tasks and two releases.

**The board's links give seven upper-tier tasks against five support tasks,**
because the release v0.11.1 is on no feature (7.3). With that link recorded,
they give eight against four, the checks' own figures. The conclusion is not
affected: the budget allows one support task for three, and either count is
over it. **The draft is right; the check's sentence is off by one.**

### 7.3 Where the draft and the checks differ, and which is right

**1. Release v0.11.1 (`01a1e9d6-6573`).**
- **The checks:** seq 565 counts it as next, as "the release v0.11.1 that
  carries them".
- **The draft:** classes it support, because no feature confirms it. Feature 5
  was recorded DONE by 13:31Z without it; v0.11.0, by contrast, is confirmed
  on feature 1.
- **Which is right:** the check is right about what the release is. The board
  is missing the link, and the rule of the proposal's
  [section 3.5](DEVELOPMENT-PROCESS-PROPOSAL.md#35-a-tasks-place-comes-from-what-depends-on-it)
  classes work by its links.
- **The fix is in the record, not in the draft:** confirm the release on the
  feature it carries, as v0.11.0 was. The check of the plan would have
  flagged it on the day, as undeclared work in the goal's epic: its title's
  bracket says `release`, not `support`. The link was added on 2026-11-18,
  after this design found it.

**2. The design proposal (`01a1e51a-00f4`, pull request #380).**
- **The draft:** puts it in the second day's window (seq 537), because its
  task went DONE at 2026-11-16 17:42:27Z, after the first check (17:41Z).
- **The check:** counted it with the first day, "before the goal was set".
- **Which is right:** the check. The pull request was merged at 17:32:26Z,
  before the goal existed. The board's DONE came ten minutes after the merge,
  when its evidence was recorded.
- **What follows:** the draft's time is the board's time, and a task recorded
  late lands in the next window. The draft lists every delivered task with its
  time, so a boundary case is seen. A task recorded DONE at delivery leaves
  none. Reading merge times from the forge would make the client depend on
  the forge, which this design does not.

**3. The check's lines that count pull requests.** The first day's check counts
"nine pull requests merged in aimem and its skills repository", and part of
that is work kept outside this project's board. The draft counts this
project's board tasks only. Under the rule one task, one pull request, the
counts agree for work kept on the board. Work kept elsewhere is outside the
draft, and the draft's header says so.

**4. Counts as of the run.** "Open tasks by state" and the feature lines are
read when the command runs. A draft for a past day, as here, shows today's
open counts and today's confirmed tasks. For example, feature 3 has one
confirmed task now, created at 08:14Z, after the check. A record's state at
T1 can be rebuilt from its history, but the counts per state cannot without
one history per task in the project. The draft is designed for the day it is
run, and prints the run's time; a replay states the lines it cannot rebuild.

## 8. Questions for the owner's review

### Q1. The next task for an assignee

The store, the API and the MCP tools have a task `assignee` (a user or an
access group) and a list filter for it. None of the trial's tasks sets one: a
task's taker is named in its `next_action` text, and its priority in its
title.

| Alternative | What it costs | What it gives |
| --- | --- | --- |
| A. Set `assignee` on every READY task from now on; read priority from the title | a step for whoever promotes a task; no code beyond the read | the read is exact for tasks so assigned; the title convention becomes load-bearing |
| B. A per-project order instead of per assignee | nothing new | right while each project has one worker, as in the trial; wrong when a project gets two |
| C. Wait for priority and complexity as fields (the proposal's follow-up 2) | a store change on a sensitive surface | the read without title parsing |

**Recommendation: A now, C later.** A costs one field per promotion and makes
the read possible for the second week of use. C replaces the title parsing
when it lands.

### Q2. Line 5, out of order

Counting it needs to know who took each task (an assignee, Q1) and which
tasks were READY for that taker at that moment. The second is a history read
per READY task of the project.

| Alternative | Cost |
| --- | --- |
| A. The draft lists the taken tasks beside the higher-tier READY tasks at that moment, and the writer judges | one history read per READY task in the window; with Q1-A, per assignee |
| B. Leave line 5 to the writer entirely | none; the line stays manual |
| C. A history query across a project on the hub | a new route, a wire change |

**Recommendation: A, after Q1.** In the trial no task of a feature was READY
on the days support was taken, so the line has been "none" every day. The
rows are cheap to show and the judgement stays with the writer.

### Q3. The budget per worker

The budget counts, per worker and calendar week, the support tasks against
the tasks of tiers 1 and 2. Per worker needs Q1. The trial's checks counted
per project, which is per worker while each project has one.

**Recommendation:** count per project now, as the check of the plan does, and
per assignee once Q1-A is in use. Two figures for the owner, separately:
- **The ratio itself.** The checks show the budget never holding when a goal
  is reached days before its review (seq 579, line 6).
- **The week's start.** Calendar week, or the goal's start when it is younger.

### Q4. A support task's kind

The checks sort filed support work into faults and tidying by hand, and the
aicrew goal's log names a field as a finding.

**Recommendation:** a title convention for the trial's remaining days
(`[P2/S, support, fault]`), read like priority, and the field with the
metadata of Q1-C.

### Q5. Dated tasks

A title convention (`DATED YYYY-MM-DD`) is read today. The owner's idea of
not-before and due dates, and of other triggers (the feature's record,
comments seq 520, 521 and 524), needs fields and a design of its own.

**Recommendation:** keep the title convention for the draft, and file the
dates as a design when the review places triggers.

## 9. Implementation tasks

To be filed by the architect after the review, in this order:

1. **The shared reader and the draft.** A client package that reads a goal's
   snapshot:
   - the features, their confirmed tasks and their dependency closure;
   - the window's changes and the times from history;
   - the tier of each task.

   `aimem process draft` on top of it. M, an ordinary surface (the client
   only), one pull request. Tests against a stand-in hub with a goal like the
   worked example's, including a late DONE and a release on no feature.
2. **The check of the plan.** `aimem process plan` on the same reader. S, an
   ordinary surface.
3. **Only if the review answers Q1 with A or B: the next task.**
   `aimem process next --assignee` (or `--project`). M, an ordinary surface.
4. **Not filed unless a session without a shell needs it: the MCP tool** of
   section 6. S, but it changes the tool sets and their inventory test, so it
   is reviewed as the tool sets were.
5. **Not filed by this design:**
   - priority, complexity and kind as fields;
   - dates and triggers;
   - a route on the hub.

   Each belongs to its own design.

## 10. Not in this design

- Any code, a route on the hub, a change of the store, or priority as a field.
- A change to the process set, or to how the course check is worded: that
  wording is the owner's, at the review.
- Writing the check: the draft is printed, never posted.
- The forge's records (merge times, pull requests of repositories without
  board tasks).
- The board's page, which shows a goal's features in its own view.
