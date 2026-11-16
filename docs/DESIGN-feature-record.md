# The feature record and its link to a goal

Status: **proposal**, for review before any code. It implements the first
step of the model in [DEVELOPMENT-PROCESS-PROPOSAL.md](DEVELOPMENT-PROCESS-PROPOSAL.md)
(sections 3 and 6, and the sketch of section 10): the feature level on the
board, in the smallest extent an agent needs through the HTTP API and the
MCP tools. Computed views, helpers and the board's page are later features
of the same goal and are left out here ([section 11](#11-left-out)).

## 1. The test this design is held against

The goal's check, as its epic states it: through the MCP tools alone, a
standalone agent session

1. sets a goal's features up, each with its check, owner and state, its
   mark with the ground and its place in the order;
2. confirms a task on a feature;
3. reads the features of a goal back;
4. reads the tasks of a feature back;
5. changes a feature's state with evidence;

and the records the trial keeps by hand, tasks titled `[feature]`, are
carried over without loss. Every decision below is chosen against these
five operations and the carry-over, and the surface in
[section 6](#6-the-surface) has nothing beyond them.

## 2. What a feature is in the store

A feature needs almost everything a task already has: a title, a text, a
check (the task's acceptance criteria), an owner (its assignee), a state,
dependencies (its confirmed tasks), evidence, comments, a revision history
and the grants that guard all of these. What it needs in addition is small:
to be told apart from work, and its link to a goal.

Three shapes were weighed.

**(a) A kind on the task record, with the link beside it.** A feature is a
task row with `kind = feature`. The kind is a column, so lists, counts and
the feed can filter on it in SQL. The link to the goal (mark, ground,
alternative, place in the order) is stored with the row, but outside the
task's editable content.
- Schema: one column and one index (`tasks.kind`), plus the link's fields.
  No new table.
- Routes and MCP: every existing task route and tool serves features as
  they are. Two new writes (set the link, confirm a task) and one new read
  (the features of a goal), plus a `feature` link on creation and a `kind`
  filter on the existing reads.
- Readers of tasks: each one decides once whether it sees features
  ([section 7](#7-readers-of-tasks-and-what-each-sees)). Most need no change,
  because a feature is a board record like a task. The ones that do (lists,
  counts, the board feed and the pickup paths) need a filter of one line or
  a refusal.

**(b) A record kind of its own** (`features` table, its own history,
comments, routes and tools), as the proposal's section 10 sketches.
- Schema: a table, a history table, a comments table, and the full-text
  index widened to a third kind.
- Routes and MCP: a second copy of create, get, update, list, history and
  comments, with their grants, retries, conflict handling, OpenAPI entries
  and MCP tools. That is roughly the task surface again.
- Readers of tasks: none changes, which is its one advantage.
- Carry-over: each trial record is copied into the new table and its task
  retired. The copy loses its task id, which other tasks and comments
  already cite, unless the id is reused across kinds. Its history and
  comments must be copied too, or they are lost.

**(c) Plain tasks with a link table and no kind:** a task is a feature when
it has a row in `goal_features(epic, task_id, …)`. This is (a) without the
column. Every reader that must not see features then needs a join
or a `NOT EXISTS` instead of a column filter, which costs more in each
query and is easier to forget.

**Chosen: (a).** It reuses the whole task surface, which is what the owner's
"must not complicate the board" asks, and it carries the trial's records
over in place: the record keeps its id, its history, its comments, its
dependencies and its evidence, so the carry-over cannot lose anything
([section 9](#9-migration-and-the-carry-over)). (b) is rejected because it
duplicates the task surface for the sake of a few fields, and its carry-over
is a copy that can lose data. (c) is rejected because it moves a cheap
column filter into a join that every excluding reader must remember.

Four rules make (a) safe to share a table with work:

- **The kind and the link are not task content.** `update_task` replaces
  the whole content, and a client that does not know features (one release
  older, or the board's page) sends content without them. So neither is in
  `TaskContent`. The kind is set only by a feature's creation or by the
  link write ([section 6](#6-the-surface)), never by a task write, and a task
  write keeps the stored link as it is. A full update of a feature by an
  older client therefore cannot turn it back into a task or drop its link.
- **A feature is never work.** It cannot be reserved, offered to a team
  member or put under team management.
- **No task depends on a feature, and the readers carry that guarantee.**
  Dependencies cross projects and a task write checks only each id's shape
  today, so a write cannot know about every project. The guarantee
  therefore sits where a dependency is acted on:
  - a reservation claim refuses a task whose dependency is a feature, and so
    does the reconciliation of a held reservation (the walker in
    `store/task_reservation_dependencies.go` reads every dependency already);
  - a team offer refuses one too: `assignmentReady` reads every dependency
    already, and refuses a dependency that is a feature.

  The walker names the feature in its refusal. The offer reads
  dependencies only inside the offered task's own project (`readTask` on
  the offer's transaction), so a dependency in another project, feature or
  not, already fails the offer today. Across projects, the offer's
  guarantee is that failure, without the feature's name. The writes add a cheap check
  inside their own project, in their transaction:
  - a task write refuses a dependency on a feature of the same project;
  - `set_feature` refuses to convert a task that a task of the same project
    depends on, and names those tasks.

  A task in another project can still list a feature, before or after the
  conversion. That is why the readers, not the writes, are the guarantee.
- **A feature never enters the board feed.** The feed
  ([section 7](#7-readers-of-tasks-and-what-each-sees)) is how a team learns
  of work, and its consumer keeps each task's last state. So a feature born
  as one is created in a single write and never appears in the feed. A task
  turned into a feature gets one closing entry, which ends its life as
  work for every consumer ([section 9](#9-migration-and-the-carry-over)).

### What a plain list of the aicrew goal returns, before and after

Today, `list_tasks` for the aicrew epic `01a186a3-12c3-7000-b23d-83d1cad3dad1`
(read on 2026-11-16) returns 21 rows in one list:

- 11 feature records (`[feature] 1.` to `[feature] 11.`);
- the goal's log (`[goal's log] …`);
- 9 tasks (pilot simulation I2a and I2b, merge rule A, the coordinator's
  member read and its cancelled first version, the stalled worker, the
  forge watch, the offer's process pin, the hosts and bring-up task).

The features and the work are told apart only by the title. The page's
counts say 7 BACKLOG, 2 READY and 9 DONE. Of those, six BACKLOG rows, one
READY row and three DONE rows are features, so the counts say there is
seven times more waiting work than there is.

After the carry-over:

- `list_tasks` (default `kind=task`) returns the 10 rows of work: the 9
  tasks and the goal's log. Its counts count only work.
- `list_features` for the epic returns the 11 features in their order
  (1 to 11), each with its mark, ground, state and owner.
- `list_tasks` with `kind=feature` or `kind=any` is available to a reader
  that wants the old mixed view.

## 3. The goal's own fields

The goal is the epic, as in the trial. Its check, its criterion and the
ordered list of its features are prose in the epic's objective today.

- **The ordered feature list becomes data, but on the features**, not on
  the epic: each feature's link carries its place in the order, and
  `list_features` returns them sorted. Keeping the list only in the
  features means the epic's prose and the records cannot disagree about
  which features exist or in what order. This is the same reason the task
  claim lives in one place ([section 5](#5-confirming-a-task-on-a-feature)).
- **The check and the criterion stay prose in the epic's objective for
  now.** Nothing in the goal's check reads them mechanically. Their first
  reader is the computed course check (feature 2, optional), which can add
  them as epic fields when it needs them: one additive epic field each,
  omitted on the wire when empty, as `epic` was added to the task.

The epic schema does not change in this step.

## 4. The link between a goal and a feature

The link carries:

| Field | Values | Meaning |
| --- | --- | --- |
| `goal` | an epic id | The goal. Equal to the feature's `epic`. |
| `position` | 1, 2, … | Its place in the goal's order of delivery. |
| `mark` | `mandatory`, `optional` | For this goal. |
| `ground` | `check`, `owner_condition`, empty | Why mandatory: the goal's check needs it, or the owner set it as a condition. Empty for an optional feature, required for a mandatory one. |
| `ground_note` | text | The condition's words and date, or why the check needs it. |
| `alternative` | text | The cheaper way, if one exists ("done by hand, as in the trial"), or empty. |

**One goal per feature in this step.** The proposal puts the mark on the
link because one feature can be optional for one goal and mandatory for the
next. Neither trial goal needs that today: every feature in both epics
belongs to one goal. So the link is a single object on the feature, and its
`goal` must equal the feature's `epic`; the task write refuses a change of
`epic` on a feature while its link names another goal.

**What widening it later costs.** The link is already an object with its own
`goal` field, and it is read and written only through the link routes. A
second goal per feature turns that object into a list:

- one schema step that moves the stored links into a table keyed by
  (feature, goal);
- `set_feature` gains the goal as part of its key;
- `list_features` reads the table.

The task routes and the readers of section 7 do not change, because none of
them reads the link.

Positions are not renumbered by the hub. Two features with the same
position in one goal are listed in id order and the response marks the tie;
the architect fixes the order on purpose, as section 5.3 of the proposal
asks.

## 5. Confirming a task on a feature

The proposal links a task to a feature in two steps: the task's title
claims a feature, and whoever promotes it to READY confirms the claim. The
confirmed link is the one that counts.

**The one recorded place is the feature's `dependencies`.** That is where
the trial keeps it already, and it is what the tiers of the proposal read
("a task that a mandatory feature depends on"). The title bracket stays a
reading aid with no meaning to the hub. Where the two disagree the
dependencies count, and no field anywhere repeats the claim, so there is
nothing else to disagree with.

Confirming is a partial write, `confirm_feature_task`, which adds one task
to a feature's dependencies (or, with `remove`, takes one out) under the
feature's revision. A full `update_task` with a new dependency list still
works, but it makes the writer resend every field, which is how task
fields were lost before triage existed (DESIGN-AIFORGE-PILOT-1, section 4).
A confirmation is the step repeated most in the trial, so it gets the
partial write.

A task may be confirmed on more than one feature. The aicrew task
`01a1e571-73ed` claims "hosts + bring-up" and supports both features 4 and
5. A feature's dependencies may name a task of another project, as task
dependencies may today: feature 3's tasks are in another aicrew epic. A
feature may not name a feature.

## 6. The surface

Every route below sits beside the task routes and is guarded as they are:

- a read needs a grant on the project, and a project or record without one
  answers as not found (`taskReadProject`, `locateTask`);
- a write needs the task write authorization (`authorizeTaskWrite`), the
  idempotency key and the expected revision of task writes;
- in team mode, the team profile's grant decides, as for tasks.

**New:**

| MCP tool | HTTP | Inputs | Result |
| --- | --- | --- | --- |
| `set_feature` | `PUT /v1/tasks/{id}/feature` | `id` (a task or a feature), `goal`, `position`, `mark`, `ground`, `ground_note`, `alternative`, `remove` (optional), `expected_revision`, `idempotency_key` | The record, with `kind` and `feature`. On a task, it turns the task into a feature and appends the feed's closing entry in the same write. It is refused while the task is reserved, team-managed, or depended on by a task of its own project; the refusal names those tasks. On a feature, it changes the link. With `remove`, it turns a feature back into a task (below). |
| `confirm_feature_task` | `POST /v1/tasks/{id}/feature/tasks` | `id` (the feature), `task`, `remove` (optional), `expected_revision`, `idempotency_key` | The feature record. Refuses a task that is a feature, and a task that does not exist or that the caller cannot read. |
| `list_features` | `GET /v1/projects/{p}/epics/{epic}/features` | `epic`, `project` (optional), `include_archived` (optional) | The goal's features in position order. Each row has id, title, state, assignee, revision, `feature` (the link) and the number of confirmed tasks. There is no paging: a goal has 10 to 15 features, and the route caps the list at 100. |

**Changed:**

| MCP tool | HTTP | Change |
| --- | --- | --- |
| `list_tasks`, `search_tasks` | `GET /v1/projects/{p}/tasks` | New `kind`: `task` (the default for a list), `feature` or `any`. New `feature`: only the tasks that feature's dependencies name in this project. Each row carries `kind` when it is a feature. `search_tasks` defaults to `any`: a search finds a record by its words, whatever it is. |
| (counts in `list_tasks`) | `GET /v1/projects/{p}/tasks/counts` | New `kind`, default `task`. |
| `create_task` | `POST /v1/projects/{p}/tasks` | New optional `feature`, the link as `set_feature` takes it. With it, the record is created as a feature in one write: no feed entry, and no moment in which a reader sees it as work. Without it, the record is a task, as today. |
| `get_task` | `GET /v1/tasks/{id}` | A feature's record carries `kind: "feature"` and its `feature` link. For a task, both are omitted, so a task reads exactly as it does today. |
| `update_task`, `triage_task` | `PUT /v1/tasks/{id}`, `POST /v1/tasks/{id}/triage` | Unchanged inputs. On a feature they keep the kind and the link. A feature moves to DONE only with at least one evidence reference: "DONE when its check passes and the evidence is recorded" (proposal, 3.3). |

**The way back.** `set_feature` with `remove` turns a feature back into a
task:
- it drops the link and appends a feed entry that reopens the record with
  its current state;
- it is allowed only while the feature has no confirmed tasks, so a
  mistaken conversion of a real task is undone in one call;
- a feature that already has confirmed tasks has them taken out first with
  `confirm_feature_task` and `remove`, one by one. That is deliberate:
  turning a feature with work under it into a task is a decision, not an
  accident to be undone silently.

Like every write, the removal is a revision in the record's history.

How each operation of the goal's check is done:

1. **Set features up:** `create_task` with the `feature` link (title, check
   as acceptance criteria, owner as assignee, state, `epic` = the goal).
   An existing task is turned into one with `set_feature`.
2. **Confirm a task:** `confirm_feature_task`.
3. **Read the features of a goal:** `list_features`.
4. **Read the tasks of a feature:** `list_tasks` with `feature`, plus
   `get_task` for a dependency in another project.
5. **Change a feature's state with evidence:** `update_task` (or
   `triage_task` between BACKLOG and READY).

The OpenAPI document gains the three routes and the new parameters, and
its parity test pins them, as for every route.

## 7. Readers of tasks and what each sees

This list was built from the code at master `8771928`, starting from every
query on `tasks` in `internal/store` and following each function to its
callers.

| Reader | File and function | Sees features? | Why, and the change |
| --- | --- | --- | --- |
| Task list | `store/tasks.go` `ListTasks`; `server/tasks.go` `listTasks`; MCP `list_tasks` | Only with `kind=feature` or `any` | A list of work is what its callers want, including the page and pickup by hand. A filter on the new column. |
| Search | `store/tasks.go` `ListTasks` with `Query` (`taskSearchSQL`); MCP `search_tasks` | Yes, by default | Finding a record by its words. Rows carry `kind`. |
| Counts | `store/tasks.go` `CountTasks`; `server/tasks.go` `countTasks` | Only with `kind` | Counts are the workload; features would double-count it. A filter. |
| One record | `store/tasks.go` `GetTask`, `readTask`; `server/tasks.go` `getTask` | Yes | A feature is read by its id. It adds `kind` and `feature`. |
| History, comments | `store/tasks.go` `TaskHistory`, `TaskCommentsOrdered`, `AddTaskComment`; their server handlers | Yes | Unchanged: a feature's history and comments are those of its record. A link write is a revision in the history like any other. |
| Write, triage | `store/tasks.go` `CreateTask`, `UpdateTask`; `store/task_triage.go` `TriageTask`; `server/textcheck.go` `checkTaskChange`; `server/triage.go` `triageTask` | Yes | They keep the stored kind and link. A write of a feature records no feed entry (`recordStateChange`). They refuse a dependency on a feature of the same project, a feature's DONE without evidence, and a change of `epic` away from the link's goal. |
| Id prefix and location | `store/taskprefix.go` (prefix lookup); `store/taskloc.go` `LocateTask`, `scanForTask`; `server/tasks.go` `locateTask` | Yes | Unchanged: a feature id resolves like a task id. |
| Full-text index | `store/store.go` (the `task_fts` rebuild) and the task writes | Yes | Unchanged: a feature is indexed as a task. |
| Typed references migration | `store/taskrefs.go` `migrateTypedRefs` | Yes | Unchanged: a past migration that rewrites every body. |
| Project presence | `store/tasks.go` `HasTasks`, `fileHasTasks`, `EnableTasksWherePresent`; `store/store.go` `Drop`, `MergeProject` | Yes | Unchanged: a project holding only features still holds board records. Dropping or merging it is refused exactly as with tasks. |
| Board feed (aicrew's `board.read`) | `store/task_state_changes.go` `recordStateChange`, `TaskStateChanges`; schema 24 in `store/store.go`; `server/boardread.go` `readBoard` | Once, to close | The feed exists for pickup, and its consumer keeps each task's last state. A feature created as one never enters it. A task turned into one gets a single closing entry, `to: "FEATURE"`, and nothing after it. A feature turned back gets an entry from `FEATURE` to its current state. Section 9 says what a consumer does with them. |
| Team task read for an offer | `server/taskread.go` `readTaskText` | No | Answers a feature as not found, so a member's offer never reads one. |
| Reservation claim, dependencies | `store/task_reservation_dependencies.go` `ClaimTaskReservation`, `locateDependencyGraph`, `ReconcileReservationDependencies`; `store/task_reservations.go` `ApplyTaskReservation` | No | A claim on a feature is refused (a feature is not work). The walker refuses a claim, and fails the reconciliation, when a dependency it reads is a feature, and names it. This is the guarantee of section 2: the walker resolves dependencies across projects, and the writes cannot. |
| Team offer and management | `store/team_assignments.go` `OfferTeamAssignment` (`assignmentReady`); `store/team_managed.go` `EditManagedTask`, `FinalizeManagedTask`, `UnmanageTask` | No | An offer of a feature is refused, and so is an offer of a task with a feature of its own project among its dependencies (`assignmentReady` reads each one in that project). A dependency in another project fails the offer already today. `set_feature` refuses a managed task. |
| Reservation routes and recovery | `server/reservations.go` `reservationTarget`, `reserve`, `reservationStatus`, `reservationReceipt`; `server/reservation_routes.go`; `server/reservation_readscope.go`; `server/recovery.go` `recoverReservation` | No | They locate a task by id, and recovery writes task content, but each acts on a reservation. A feature has no active one: a claim on a feature is refused, and the conversion refuses a reserved record. A released reservation's ledger may remain readable for a converted record, as a read of history. |
| Board page | `server/tasks.html` (list, counts, detail, edit) via the routes above | Only by direct link | The page lists through `listTasks` and `countTasks` with no `kind`, so it shows work only and its counts stop counting features. A feature's detail opens by its id, and its edit keeps the link. Showing features is feature 3. |

**A client or hub one release older.**

- *An old client on a new hub* lists work only and does not know features
  exist. It reads a feature by id with two fields it ignores, and its full
  update of a feature keeps the kind and the link (section 2). It cannot
  reach features through `list_tasks`, which is the intended separation.
- *A new client on an old hub* gets 404 for the three new routes; the MCP
  tool says the hub must be upgraded. The client sends `kind` and `feature`
  only when asked. An old hub ignores the unknown query parameters and
  answers with the mixed list it always gave, which is today's behavior.
  A `create_task` with a `feature` link is refused by an old hub, whose
  task body decoder rejects unknown fields (`decodeTaskBody`). So a feature
  can never be created there as plain work by mistake.
- *An old binary on a migrated database* refuses it on the schema version,
  as after every schema step.

## 8. The standalone condition

The owner's condition of 2026-11-16: the process works for one session
with a project credential, and no step requires aicrewd, a team, a
coordinator or a member's launcher.

- Every route in section 6 is an ordinary project route, reached with an
  ordinary token that holds a grant on the project. None is a team route,
  and none needs a team context, a peer service or a reservation.
- The owner is the record's `assignee`. Its kinds are `user` and `group`
  (`TaskAssignee`). A person is a user. A standalone session acts as its
  token's user. A team member acts as its own user. So any of them can own
  a feature, and nothing in the link names a team.

**Where several sessions share one user, the owner is a group per role.**
On the trial's board, the architect session and the aimem worker write
with the same user and token, so a `user` assignee cannot tell them, or
the operator, apart. The trial keeps owners as prose ("the operator", "the
operator, with the architect", "worker", "the team"). In this model each
role is an access group: `operator`, `architect`, `worker`, and so on,
with the people or users that act in it. A feature's assignee names the
group.
- Groups already exist and are already an assignee kind, so this needs no
  new field.
- A hub admin creates them, so the operator does, once per project.
- A team member can also be named by its own user, where the team has
  one per member.

Until the operator has created the groups, the carry-over sets no
assignee, and the owner stays prose in the record's objective, as in the
trial. The course check's "who holds it" then reads that prose, as it does
today. Computing it from the assignee is left to feature 2, and is listed
in [section 11](#11-left-out).

**The goal's check, performed by one standalone session** with a token
holding write access to the `aimem` project, no team running. The epic is
the aimem goal `01a1e571-73fc-7000-a652-c99832a9dde1`.

1. `create_task` with a `feature` link for each of features 1 to 4 (or
   `set_feature` on the trial's records):
   - feature 1: `position 1, mark mandatory, ground check`, with the
     ground note "it is the goal's check";
   - features 2, 3 and 4: positions 2 to 4, `mark optional`, each with the
     alternative from the epic's text.
2. `confirm_feature_task` with feature `01a1e572-19c0` and task
   `01a1e572-1a04` (this design).
3. `list_features` for the epic returns the four features in order.
4. `list_tasks` with `feature=01a1e572-19c0` returns this design task.
5. When the implementation is merged, `update_task` moves feature 1 to DONE
   with the evidence of its check (the merge and the run of steps 1 to 4).
   Without the evidence, the write is refused.

The implementation's acceptance test runs exactly these steps through the
MCP facade against a test hub, with one ordinary token and no team.

## 9. Migration and the carry-over

**The schema step** (version 26, one transaction with the version bump):

```sql
ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'task'
  CHECK(kind IN ('task','feature'));
CREATE INDEX idx_tasks_kind ON tasks(kind, archived, state, id);
```

- The link is stored in the task's body as a top-level `feature` object
  beside the content, as `id`, `revision` and the timestamps are. It is not
  inside `TaskContent`, so it is in every history snapshot but never in the
  content a task write replaces. (The record's `coordination` is different:
  it is read from the team tables by a join, and is not in the body.)
- Existing rows default to `task`, so nothing changes until a link is set.
- `recordStateChange` writes nothing for a row whose kind is `feature`.
  The conversion writes the one closing entry itself (below).

**The feed's closing entry.** The board feed is a sequence of state
changes, and its consumer keeps each task's last state. Its consumer is
aicrewd, the only caller of the peer route `board.read`. As read in
aicrew's `internal/store/readytasks.go` on 2026-11-16 (`recordBoardTask`,
`noticeReadyTasks`), aicrewd stores the last state per task, tells a team's
coordinators at each session start which tasks are READY with no open
attempt, and announces a task when it becomes READY. A record turned into
a feature has its earlier states in the feed already: aicrew's feature 4
was created READY. If the feed simply went silent, the consumer would hold
that READY for ever and keep naming the feature as work.

So the write that turns a task into a feature appends one entry:

- `from` is the record's state, `to` is `FEATURE` at the new revision, and
  the entry carries no content, like every feed entry;
- `FEATURE` is not one of the task states, so no reader can take it for
  one;
- the value has the shape of a state name: an upper-case letter, then up
  to 31 upper-case letters or underscores. aicrew's hub client checks every
  entry's `to`, and a non-empty `from`, against that shape (aicrew
  `internal/hubteams/hubteams.go`, `stateShape` and `BoardPage.valid`). It
  rejects the whole page when one entry fails, which would stop that hub's
  feed. `FEATURE` passes in both directions. Any later closing value must
  keep the shape;
- after that entry, nothing more about the record enters the feed;
- `set_feature` with `remove` appends the reverse entry, from `FEATURE` to
  the record's state, so a consumer sees it as work again from that
  revision.

What a consumer does with it:

- *aicrewd as it is today* accepts the entry, since `FEATURE` has a state
  name's shape, and it acts only on READY. It stores `FEATURE` as the last
  state, so the record is neither named to the coordinators nor announced,
  and no offer is made. It needs no change.
- *A consumer that checks `to` against the list of task states* would
  refuse the entry. aicrewd checks the shape, not the list. The
  implementation confirms this against aicrew's tests before it merges.
  The feed's description in `server/boardread.go`
  and in the OpenAPI document gains one line: `FEATURE` means "no longer
  work", and a consumer drops the record.

A feature created with `create_task` and its `feature` link is a feature
from its first revision, so it never enters the feed: there is no moment
between two writes in which a reader could announce it.

**The carry-over of the trial's records is a documented procedure, not a
command.** Each record's position is in its title, but its mark, ground and
alternative are only in the epic's prose. A command would have to parse
that prose, for about fifteen records in two epics, once. For each epic
(aicrew `01a186a3-12c3-7000-b23d-83d1cad3dad1`, aimem
`01a1e571-73fc-7000-a652-c99832a9dde1`), a session with write access:

1. Lists the epic's records whose title begins `[feature]`.
2. Checks that no task lists one of them among its dependencies. Within
   the project, `set_feature` refuses such a record and names the tasks;
   the dependency is then moved onto the feature the other way round before
   the record is retried. A task in another project that depends on the
   record is not visible to this write, and the full-text search does not
   index dependencies. Such a task is found when its claim or offer is
   refused with the feature named (section 2), and its dependency is moved
   then.
3. Releases any reservation or team attempt on a record. `set_feature`
   refuses a held or managed record, and a feature is never work.
4. Calls `set_feature` on each record with the position from its title and
   the mark, ground, ground note and alternative from the epic's list. A
   record whose last state is READY or IN_PROGRESS needs no extra step:
   the conversion's closing entry ends it in the feed, whatever its state.
   Where a team is running, the check is that the coordinators' list of
   READY work at the next session start no longer names it.
5. Compares `list_features` with the epic's list: the same ids, the same
   order, the same marks. Any difference is fixed with `set_feature`
   before the next epic.
6. Records the result as a comment on the goal's log.

**Why nothing is lost:** the records keep their ids, revisions, history,
comments, dependencies, evidence and assignees. The `[feature]` title
prefix may stay; it means nothing to the hub. The goal's log stays a task.
The epic's prose list may be shortened to a pointer once `list_features`
agrees with it, which is the architect's decision, not this step's.

The procedure goes into `docs/` with the implementation, beside the
user-facing description of features.

## 10. The aicrew goal in this model

The goal `01a186a3-12c3-7000-b23d-83d1cad3dad1`, "a first dogfood cycle".
Its check and criterion (minimal) stay in the epic's objective. Its
features, as `list_features` would return them after the carry-over:

| Position | Feature | Mark | Ground | Alternative | State |
| --- | --- | --- | --- | --- | --- |
| 1 | `01a1e56e-4bc8` The cycle is proven in a simulation | mandatory | `owner_condition`: "owner, 2026-10-29 (increments I2, I3 and I5)" | | DONE |
| 2 | `01a1e56e-4bdb` Turns start fresh from a brief | mandatory | `owner_condition`: "owner, 2026-10-15" | | DONE |
| 3 | `01a1e56e-4bed` A member can hold its own forge credential | mandatory | `check` | | DONE |
| 4 | `01a1e56e-4c01` The members' hosts exist | mandatory | `check` | | READY |
| 5 | `01a1e56e-4c10` The team is brought up on those hosts | mandatory | `check` | | BACKLOG |
| 6 | `01a1e56e-4c1f` The cycle is run | mandatory | `check`: "it is the goal's check" | | BACKLOG |
| 7 | `01a1e56f-d5ff` The architect is a role in the product | optional | | an architect session does it by hand | BACKLOG |
| 8 | `01a1e56f-d60f` The operator has a seat in the product | optional | | decisions are recorded on the task by hand | BACKLOG |
| 9 | `01a1e56f-d647` Ordinary pull requests are merged by the product's rule | optional | | done by hand | BACKLOG |
| 10 | `01a1e56f-d65b` Credentials are deposited through the product | optional | | feature 3, placed by hand | BACKLOG |
| 11 | `01a1e56f-d665` An upgrade between releases is proven in the simulation | optional | | none; a first cycle installs fresh | IN_PROGRESS |

Feature 4's owner is the operator and feature 5's the operator with the
architect. Both are assignees, a user or a group, like any other owner.

**One confirmed task, feature 3:**

- the record `01a1e56e-4bed-7000-ab2f-47753e8623bf`, kind `feature`;
- its check is its acceptance criteria: "the product's command (`join
  --cred`) writes a forge credential into a member's home, and the member's
  clone uses it";
- its dependencies, the confirmed tasks: `01a105fa-336e`, `01a105fa-337e`
  and `01a105fa-338e`, all in another epic of the same project;
- its evidence: task `01a105fa-338e`, "354c-2 DONE";
- its state: DONE.

`list_tasks` with `feature=01a1e56e-4bed` returns those three tasks.
Confirming a fourth would be one `confirm_feature_task` call.

## 11. Left out

By name, and each for a later step if the trial shows the need:

- computed views: the next task per assignee, the tiers, the course
  check's counted lines, the goal's distance (feature 2);
- helpers for the mechanical steps the trial repeats (feature 2);
- the board's page showing features (feature 3);
- the process set and the pickup policy carrying the tiers (feature 4);
- the goal's check and criterion as epic fields (section 3);
- the owner computed from the assignee for the course check, and the role
  groups themselves, which the operator creates (section 8);
- a feature on more than one goal (section 4);
- the goal's log as a record kind (it stays a task);
- numeric weights, estimates in days and a drawn graph;
- aicrew's coordinator taking work from a computed order.

## 12. The implementation's split

Two tasks, each readable whole, both on a sensitive surface (the task
schema and the HTTP API), so each gets the architect's high review and is
merged by the operator:

1. **[P1/M, feature record] The feature kind in the store.** Covers:
   - the schema step and the link in the body;
   - the rules: a kind outside content, never work, DONE with evidence,
     the goal equals the epic;
   - the dependency guarantee: the walker and the offer refuse a feature
     dependency, and the writes check their own project;
   - `kind` and `feature` in `ListTasks` and `CountTasks`;
   - the feed: no entry for a feature, the closing entry on conversion and
     the reverse entry on removal;
   - the refusals in reservations, offers and management;
   - the store's part of creation as a feature, `set_feature` (with
     `remove`) and `confirm_feature_task`.

   Its tests include:
   - an older client's full update of a feature;
   - a READY task converted to a feature, read back through the feed as
     closed;
   - a task in another project that depends on a feature, refused at the
     claim with the feature named;
   - a task that depends on a feature of its own project, refused at the
     offer;
   - each reader of section 7.
2. **[P1/M, feature record] The feature routes and MCP tools.** Covers:
   - the three new routes and the changed parameters, with OpenAPI and its
     parity test;
   - the MCP tools;
   - the carry-over procedure in `docs/`;
   - the acceptance test of section 8: the goal's check by one standalone
     session with one ordinary token.

   It depends on task 1.

After both merge and a release reaches the hubs, the carry-over is run on
the two trial epics, and feature 1's check is recorded as its evidence.
