# Features of a goal

A goal is an epic: a release or milestone with a check that says when it is
reached. A **feature** is a record on the task board that names one thing
the goal needs, such as "the members' hosts exist". It is not work. Nobody
claims a feature or is offered one; the work that delivers it is ordinary
tasks, and the feature lists those tasks as its **confirmed tasks**.

This page is for a session that plans and tracks a goal: an architect, an
operator, or one agent working alone. Everything here works with one
ordinary task token that holds a grant on the project. It needs no team, no
aicrew, no coordinator and no member's launcher. The design is
[DESIGN-feature-record.md](DESIGN-feature-record.md).

## What a feature carries

A feature is a task record with `kind: "feature"` and a `feature` link to
its goal. It keeps everything a task has: its id, title, acceptance criteria
(the feature's check), assignee (its owner), state, history, comments and
evidence. The link carries:

| Field | Values | Meaning |
| --- | --- | --- |
| `goal` | an epic id | The goal. It must be the record's `epic`. |
| `position` | 1 to 10000 | Its place in the goal's order of delivery. |
| `mark` | `mandatory` or `optional` | Whether the goal needs it. |
| `ground` | `check` or `owner_condition` | Why it is mandatory: the goal's check needs it, or the owner set it as a condition. Required for a mandatory feature; an optional one has none. |
| `ground_note` | text | The condition's words and date, or why the check needs it. |
| `alternative` | text | The cheaper way, if one exists, such as "done by hand". |

The hub does not renumber positions. Two features that share a position are
listed in id order, and both rows say `tie: true`; fix the order on purpose
with `set_feature`.

A feature's dependencies are its confirmed tasks. A task may be confirmed on
more than one feature, and may be in another project you can read. A
feature never confirms a feature, and no task may depend on a feature.

## The tools

| MCP tool | HTTP | What it does |
| --- | --- | --- |
| `create_task` with `feature` | `POST /v1/projects/{p}/tasks` with a `feature` object | Creates the record as a feature in one write. Set its `epic` to the goal. |
| `set_feature` | `PUT /v1/tasks/{id}/feature` | Turns a task into a feature, or changes a feature's link. With `remove: true`, turns a feature back into a task. |
| `confirm_feature_task` | `POST /v1/tasks/{id}/feature/tasks` | Adds one task to a feature's confirmed tasks, or with `remove: true` takes it out. Nothing else changes. |
| `list_features` | `GET /v1/projects/{p}/epics/{e}/features` | The goal's features in position order, each with its link and the number of confirmed tasks. At most 100, no paging. |
| `list_tasks` with `feature` | `GET /v1/projects/{p}/tasks?feature=<id>` | The tasks of this project that the feature confirms. Read a confirmed task of another project with `get_task`. |
| `list_tasks`, `search_tasks` with `kind` | `?kind=task`, `feature` or `any` | A list shows work only (`task`) unless asked; a search finds any record by its words. The counts follow the list's `kind`. |

Every write takes the `expected_revision` you read and an `idempotency_key`.
A stale revision returns the current record instead, and retrying with the
same key returns the first answer.

A feature changes state with `update_task` (or `triage_task` between
BACKLOG and READY), as a task does. It moves to DONE only with at least one
evidence reference: the record of the check that passed.

## Rules that refuse a write

- `set_feature` on a task that is reserved or team-managed is refused:
  release the reservation or end the attempt first.
- `set_feature` on a task that another task of its project depends on is
  refused, and the refusal names those tasks. Move the dependency the other
  way round: take it off the dependent task, convert the record, then
  confirm the task on the feature.
- `set_feature` with `remove` is refused while the feature has confirmed
  tasks. Take them out one by one with `confirm_feature_task` and `remove`
  first. Turning a feature with work under it back into a task is a
  decision, not an accident to undo silently.
- A claim, a team offer or team management of a feature is refused, and so
  is a claim of a task that depends on one; the refusal names the feature.

## What the board feed says

The board feed (`board.read`, read by aicrewd) lists task state changes for
pickup. A feature created as one never appears in it. A task turned into a
feature gets one last entry, to `FEATURE`: the record is no longer work, and
a consumer drops it. A feature turned back into a task gets an entry from
`FEATURE` to its state, and is work again from that revision.

## A hub or a client one release older

A client one release older lists work only, reads a feature by its id, and
its full update of a feature keeps the kind and the link. A client on a hub
one release older is told to upgrade the hub when it calls a feature tool,
and a `create_task` with a link is refused there, so a feature is never
stored as work by mistake.

## The carry-over of a goal's trial records

Before features existed, a goal's features were tasks titled `[feature]`,
with their marks, grounds and alternatives written in the epic's text. This
procedure turns them into features once, for each goal. It is a procedure,
not a command: a session with write access to the project runs it with the
tools above.

1. **List the records.** `list_tasks` with the goal's `epic` (and a `limit`
   of 100), and keep the records whose title begins `[feature]`. Each title
   carries the record's position.
2. **Check that no task depends on one.** Within the project, `set_feature`
   refuses such a record and names the tasks. Take the dependency off each
   named task with `update_task`, convert the record, then confirm the task
   on the feature with `confirm_feature_task`. A task in another project
   that depends on the record is not visible to this write, and the search
   does not index dependencies; such a task is found when its claim or offer
   is refused with the feature named, and its dependency is moved then.
3. **Release what holds a record.** A reservation or a team attempt on a
   record makes `set_feature` refuse it, and a feature is never work.
   Release the reservation, or end the attempt, first.
4. **Convert each record.** `set_feature` on it with the position from its
   title and the mark, ground, ground note and alternative from the epic's
   list. The record keeps its state; a record that is DONE already stays
   DONE. A record whose last state is READY or IN_PROGRESS needs nothing
   more: the conversion's feed entry ends it as work. Where a team is
   running, check that the coordinators' list of READY work at the next
   session start no longer names it.
5. **Compare.** `list_features` for the goal must give the epic's list: the
   same ids, in the same order, with the same marks, grounds and
   alternatives. Fix any difference with `set_feature` before the next goal.
6. **Record the result** as a comment on the goal's log.

Nothing is lost: each record keeps its id, revisions, history, comments,
dependencies, evidence and assignee. The `[feature]` prefix may stay in the
title; it means nothing to the hub. The goal's log stays a task. Once
`list_features` agrees with the epic's text, the list in that text may be
shortened to a pointer; that is the goal owner's decision, not part of this
procedure.

The procedure is tried in the test suite on the aicrew goal's eleven records
as the design's section 10 shows them
(`internal/server/features_carryover_test.go`).
