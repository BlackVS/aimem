# Tool sets per mode

Status: **proposal**, for review before any code. It is the first task of
the goal's optional feature 5: one MCP tool set per mode, and the present
tools analysed and reorganised in the same step. No tool changes what it
does or who may call it; only which list offers it changes.

## 1. The problem and the test

A session sends the definitions of every tool it is offered with every
request, unless its client defers them. A personal session in a project
checkout is offered 70 tools in 97,069 bytes of definitions, about 24
thousand tokens per request. One provider's API is widely reported to cap a
request at 128 tools across all connected servers, and some clients cap
lower. Most of these bytes describe work the session never does: 30 of the
70 tools, 48,810 bytes, belong to aimem's own team protocol, which no team
has run since the pilot was paused.

The design's first test: **a standalone session's set is at most half of
today's bytes**, that is at most 48,534 bytes. The set proposed in
section 4 is 33 tools and 36,027 bytes (37%).

## 2. The inventory

### 2.1 How the figures are made

`internal/mcp/toolinventory_test.go` asks each configuration of the facade
for its own `tools/list` answer and measures each definition as it is sent
(compact JSON). On every run it fails when a listed tool has no group, so a
new tool is classified when it is added. To print the tables:

    AIMEM_TOOL_INVENTORY=1 go test ./internal/mcp/ -run TestToolInventory -v

The figures below are from master 3d93e19 (v0.11.0) and include the three
feature tools of #382 and #383.

| configuration | tools | bytes |
| --- | ---: | ---: |
| personal, checkout-bound | 70 | 97,069 |
| personal, no checkout | 67 | 89,646 |
| personal, tasks disabled | 15 | 11,041 |
| tasks-only (the hub's MCP, per credential) | 57 | 82,037 |
| team member (team-mode facade) | 25 | 27,405 |

| group | tools | bytes |
| --- | ---: | ---: |
| team | 27 | 42,039 |
| tasks | 18 | 24,334 |
| reservation | 7 | 12,232 |
| knowledge | 13 | 9,398 |
| onboarding (`team_setup`, `team_continue`) | 2 | 5,670 |
| process (`process_context`) | 1 | 1,753 |
| guidance (`team_context`, `writing_rule`) | 2 | 1,643 |
| team-mode (`write_report`, `session_context`) | 2 | 1,377 |

72 distinct names exist across the facades.

### 2.2 Every tool

"Named by" counts where a tool's name appears outside `internal/mcp`:
aicrew's code (v0.11.0, read through the forge's API), the skills set
(aiskills v1.43.0, the same way), and aimem's own documents (outside the
archive and the fixtures). aicrew reaches the reservation tools through a
CLI command (3.2), so its code does not name them. "Set" is section 4's
proposal.

| tool | group | bytes | named by | set |
| --- | --- | ---: | --- | --- |
| `team_context` | guidance | 1,101 | 6 docs | teams |
| `writing_rule` | guidance | 542 | aicrew, aiskills, 2 docs | standalone, member, tasks-only |
| `confirm_memory` | knowledge | 619 | 3 docs | standalone |
| `forget_memory` | knowledge | 468 | 2 docs | standalone |
| `get_design_doc` | knowledge | 441 | aicrew, 2 docs | standalone |
| `get_record` | knowledge | 487 | 7 docs | standalone |
| `list_docs` | knowledge | 386 | aicrew, 7 docs | standalone, member, tasks-only |
| `list_records` | knowledge | 674 | 7 docs | standalone |
| `put_record` | knowledge | 1,082 | 7 docs | standalone |
| `read_doc` | knowledge | 393 | aicrew, 7 docs | standalone, member, tasks-only |
| `recall_memory` | knowledge | 975 | 4 docs | standalone, member, tasks-only |
| `remember` | knowledge | 1,441 | 9 docs | standalone |
| `review_memories` | knowledge | 804 | 4 docs | standalone |
| `search_journal` | knowledge | 613 | 4 docs | standalone |
| `update_doc` | knowledge | 1,015 | 6 docs | standalone |
| `team_continue` | onboarding | 2,012 | 6 docs | teams |
| `team_setup` | onboarding | 3,658 | 7 docs | teams |
| `process_context` | process | 1,753 | aicrew, 6 docs | standalone |
| `task_reservation_claim` | reservation | 2,196 | - | none (by name, CLI) |
| `task_reservation_finalize` | reservation | 2,155 | - | none (by name, CLI) |
| `task_reservation_receipt` | reservation | 854 | 1 doc | none (by name, CLI) |
| `task_reservation_release` | reservation | 2,149 | - | none (by name, CLI) |
| `task_reservation_status` | reservation | 558 | 1 doc | none (by name, CLI) |
| `task_reservation_transfer` | reservation | 2,179 | - | none (by name, CLI) |
| `task_reservation_update` | reservation | 2,141 | - | none (by name, CLI) |
| `add_task_comment` | tasks | 720 | aicrew, 3 docs | standalone, member, tasks-only |
| `confirm_feature_task` | tasks | 1,000 | 2 docs | standalone, tasks-only |
| `create_epic` | tasks | 832 | 1 doc | standalone, tasks-only |
| `create_task` | tasks | 4,411 | 6 docs | standalone, tasks-only |
| `get_epic` | tasks | 544 | aicrew, 1 doc | standalone, member, tasks-only |
| `get_task` | tasks | 532 | aicrew, 5 docs | standalone, member, tasks-only |
| `get_task_comment` | tasks | 645 | aicrew, 2 docs | standalone, member, tasks-only |
| `get_task_history` | tasks | 697 | aicrew, 2 docs | standalone, member, tasks-only |
| `list_epics` | tasks | 638 | aicrew, 1 doc | standalone, member, tasks-only |
| `list_features` | tasks | 863 | 2 docs | standalone, member, tasks-only |
| `list_task_comments` | tasks | 938 | aicrew, 2 docs | standalone, member, tasks-only |
| `list_tasks` | tasks | 2,303 | aicrew, 6 docs | standalone, member, tasks-only |
| `project_repository` | tasks | 441 | aicrew, 1 doc | standalone, member, tasks-only |
| `search_tasks` | tasks | 1,980 | 3 docs | standalone, member, tasks-only |
| `set_feature` | tasks | 1,848 | 2 docs | standalone, tasks-only |
| `triage_task` | tasks | 1,164 | aicrew, 4 docs | standalone, member, tasks-only |
| `update_epic` | tasks | 1,106 | 1 doc | standalone, tasks-only |
| `update_task` | tasks | 3,672 | 6 docs | standalone, tasks-only |
| `team_accept` | team | 1,048 | 2 docs | teams |
| `team_ack` | team | 1,385 | 3 docs | teams |
| `team_assignment` | team | 894 | 1 doc | teams |
| `team_block` | team | 1,434 | 2 docs | teams |
| `team_cancel` | team | 1,607 | 1 doc | teams |
| `team_close_stop` | team | 1,581 | 1 doc | teams |
| `team_decline` | team | 1,274 | 1 doc | teams |
| `team_edit` | team | 1,736 | 2 docs | teams |
| `team_finalize` | team | 2,319 | 2 docs | teams |
| `team_handoff` | team | 1,648 | 1 doc | teams |
| `team_heartbeat` | team | 1,395 | 1 doc | teams |
| `team_inbox` | team | 1,463 | 3 docs | teams |
| `team_join` | team | 2,149 | 2 docs | teams |
| `team_leave` | team | 1,253 | 4 docs | teams |
| `team_list` | team | 401 | 2 docs | teams |
| `team_members` | team | 1,268 | 2 docs | teams |
| `team_messages` | team | 1,343 | 1 doc | teams |
| `team_offer` | team | 2,057 | 2 docs | teams |
| `team_profile` | team | 2,292 | 3 docs | teams |
| `team_reserved` | team | 895 | 1 doc | teams |
| `team_resume` | team | 1,255 | 1 doc | teams |
| `team_resume_work` | team | 1,408 | 1 doc | teams |
| `team_review` | team | 1,807 | 1 doc | teams |
| `team_send` | team | 2,862 | 2 docs | teams |
| `team_stopped` | team | 1,489 | 2 docs | teams |
| `team_submit` | team | 2,355 | 1 doc | teams |
| `team_withdraw` | team | 1,421 | 1 doc | teams |
| `session_context` | team-mode | 388 | 2 docs | member |
| `write_report` | team-mode | 989 | 1 doc | member |

### 2.3 Use on the hubs

The hubs keep no record of calls per tool. The primary hub's service log
records writes by fixed message names, never reads and never a tool name.
The coordinator counted it as an admin for the 14 days to 2026-11-17 09:50Z,
counts only:

| write | count |
| --- | ---: |
| journal append | 4,731 |
| document published | 1,157 |
| task comment added | 853 |
| task updated | 780 |
| task created | 433 |
| task triaged | 29 |
| feature set | 16 |
| process reference selected | 14 |
| skills set imported | 9 |
| epic created | 4 |
| epic updated | 3 |
| feature task confirmed | 2 |
| reservation request refused | 68 |
| any team write (join, offer, accept, submit, handoff and the others) | 0 |

Two of these need care:

- An accepted reservation writes no log line (`reservationOK`,
  `internal/server/reservation_routes.go:361`); only refusals do (`:163`,
  `:177`). The 68 refusals say nothing about how many were accepted.
- Zero team writes means that no team was running: the pilot was paused on
  2026-10-15 and the members' services were removed on 2026-10-29. It is
  not evidence that the team tools are unneeded.

So no tool can be retired on the evidence that nothing calls it: every one
of the 72 names appears in some aimem document, and reads are not counted
anywhere. Section 5.4 proposes the smallest counter.

## 3. Findings

### 3.1 The team protocol is half of a personal session's list

The 27 team tools, `team_setup`, `team_continue` and `team_context` are
48,810 bytes, 50% of the checkout-bound list. aicrew v0.11.0 calls none of
them: its members run in aimem's team-mode facade (3.3), which lists no
`team_*` tool. Inside aimem they are named by the team commands
(`join_team` and `resume_team`, installed from `internal/wiring/assets/`),
the team guides (`docs/TEAM-QUICKSTART.md`, `docs/TEAM-AGENT-QUICKSTART.md`,
`docs/TEAM-SETUP.md`) and their designs.

By their own descriptions they divide into:

- **a member's work in a team:** `team_join`, `team_resume`, `team_leave`,
  `team_heartbeat`, `team_members`, `team_profile`, `team_messages`,
  `team_inbox`, `team_ack`, `team_send`, `team_assignment`,
  `team_reserved`, `team_accept`, `team_decline`, `team_block`,
  `team_resume_work`, `team_stopped`, `team_submit`;
- **coordinating and setting a team up:** `team_list`, `team_offer`,
  `team_withdraw`, `team_cancel`, `team_close_stop`, `team_edit`,
  `team_review`, `team_finalize`, `team_handoff`, and the onboarding pair
  `team_setup` and `team_continue`, with `team_context` as their guide.

aicrew now owns teams, membership, attempts and delivery, so all thirty are
superseded once aicrew is functional. They are not retired here: the
operator's decision of 2026-09-25 (`docs/ROADMAP.md`, "Next delivery
sequence") holds the removal of the legacy team until aicrew is
functional. This design moves them out of the default set instead
(section 4), which costs nothing if they are later removed.

### 3.2 The reservation tools are listed to models but called by a program

The seven `task_reservation_*` tools are 12,232 bytes and are listed to
every personal, tasks-only and member session. Five of them carry the same
thirteen parameters (2,141 to 2,196 bytes each). Their one caller is aicrew,
and it calls them as a program, not through a model:

- aicrew's member steps run `aimem reservation OP` under the team session
  file (aicrew `internal/agent/aimem.go:216-221`, `internal/agent/steps.go:32`).
- That command calls the tool's handler directly
  (`internal/mcp/reservation_cli.go:68-80`, `s.taskTool`), not through
  `tools/list`.

So no model needs them in its list. They are not merged either: their wire
is frozen by `docs/DESIGN-AIFORGE-RESERVATION-WIRE.md`, with fixtures in
both repositories, and a merge would change it for no gain once they are
unlisted.

### 3.3 aicrew allows two tools its members are never offered

aicrew's member turn allows `get_design_doc` and `process_context` among
its aimem reads (aicrew `internal/agent/homeaimem.go:195-197`,
`aimemReadTools`). A member turn starts its client with
`AIMEM_TEAM_SESSION` set (aicrew `internal/agent/launch.go:73`), so its
`aimem mcp` is the team-mode facade (`internal/mcp/mcp.go:40-42`). That
facade lists neither tool (`internal/mcp/teammode.go:32-45`, the lists
`teamToolList` draws from) and refuses both by name (`teammode.go:282`).
The two allow rules can never take effect. Whether a member needs a design
document or the process through MCP is aicrew's question; until it asks,
aicrew drops the two rules and aimem's member set stays as it is.

### 3.4 Ten definitions repeat one paragraph

`team_join`, `team_members`, `team_heartbeat`, `team_resume`, `team_leave`,
`team_profile`, `team_send`, `team_messages`, `team_inbox` and `team_ack`
are generated from one template (`internal/mcp/teams.go:71`) whose
description is a shared paragraph of about 650 characters, so the list
carries it ten times. This disappears from the default set with 3.1. If the
team tools stay listed anywhere for long, the paragraph belongs in
`team_context`, which exists to carry the protocol.

### 3.5 Long task definitions

`create_task` (4,411 bytes, 17 parameters) and `update_task` (3,672, 17) are
the two largest definitions in the default set; `list_tasks` (2,303) and
`search_tasks` (1,980) follow. They are the board's main writes and reads,
so they stay. Their text can be shortened, but the standalone set meets its
test without that (section 4), so it is a follow-up measured by the
inventory test, not part of this design.

### 3.6 No tool repeats another

`get_design_doc` reads a shared knowledge base's synthesized design;
`read_doc` reads one shared document by name. `confirm_memory`,
`forget_memory` and `review_memories` act on different states of a
memory. No pair differs only in a parameter.

## 4. The sets

| set | for | tools | bytes |
| --- | --- | ---: | ---: |
| standalone | a session on a project, personal or tasks-enabled | 33 | 36,027 |
| teams | added to standalone by a setting (4.2) | +30 | +48,810 |
| member | a team member's session (team-mode facade) | 18 | 15,138 |
| tasks-only | the hub's MCP under a project credential | 22 | 26,630 |

- **standalone:** the 13 knowledge tools, the 18 task, epic and feature
  tools, `writing_rule` and `process_context`. With tasks disabled the task
  tools and `process_context` leave it, as today.
- **teams:** the 27 team tools, `team_setup`, `team_continue` and
  `team_context`.
- **member:** today's team-mode list without the reservation tools: the 12
  task, epic and feature reads with the coordinator's `triage_task` and
  `add_task_comment`, `recall_memory`, `list_docs`, `read_doc`,
  `write_report`, `session_context` and `writing_rule`.
- **tasks-only:** the 18 task tools, the three knowledge reads it already
  serves (`recall_memory`, `list_docs`, `read_doc`) and `writing_rule`. Its
  team tools follow the teams setting; for the hub's MCP the setting is a
  parameter of the MCP URL (open question 1).
- **No set** lists the reservation tools. They stay served by name in every
  facade that serves them today, and through `aimem reservation`.

### 4.1 What selects a mode

Mostly what the session is bound to at start, which the facade already
reads: a team session file selects member (`mcp.go:40`), a tasks-only
credential selects tasks-only (`mcp.go:807`), anything else is standalone.
One setting adds the teams set. A tool that widens the set during a session
is rejected: it needs the client to honour a changed tool list
(`notifications/tools/list_changed`), which clients do unevenly, and it
would itself be one more definition in every list.

### 4.2 The teams setting

A per-checkout setting, written by the commands that join a team
(`aimem team ...` and the `join_team` entry point's install) and readable
with `aimem project show`. A session reads it at start, like the task
state. It is per checkout because a team is joined from one checkout, and a
standalone session elsewhere should not pay for it.

## 5. The reorganisation

### 5.1 What changes

| change | tools |
| --- | --- |
| moved out of the default set, into teams | 27 team tools, `team_setup`, `team_continue`, `team_context` |
| unlisted, served by name and by CLI | 7 reservation tools |
| moved out of the member set | 7 reservation tools |
| merged, split, renamed | none |
| retired | none (3.1: on hold by the operator's decision) |

### 5.2 How older clients and documented calls keep working

- **A tool outside the session's set stays served by name.** A client one
  release older may hold a cached list, and a documented call may name a
  team tool directly. The call succeeds; for a teams tool called while the
  setting is off, the answer carries one line saying how to turn the
  setting on. This holds for two releases after the change, then the team
  tools are refused by name like any other unlisted tool, with the same
  line, until the operator decides their removal.
- **The team entry points.** `join_team` and `resume_team` check in their
  step 0 that the tools they name are listed
  (`TestEntryPointsNameOnlyToolsTheLocalServerLists`,
  `internal/mcp/entrypoints_test.go:29`). The command that installs them
  sets the teams setting, and the test changes to: every tool an entry point
  names is in the standalone set or the teams set.
- **aicrew** is unaffected: its members use the team-mode facade, whose
  reservation calls go through the CLI (3.2), and its reads stay listed.
- **The skills set** names only `writing_rule`, which every set keeps.

### 5.3 What the documentation check must accept

`docs/examples_test.go` compares the tool names in the documents with
`mcp.ToolNames()` (`internal/mcp/toolnames.go`), the union of every name
any facade can serve. That union does not change, so the check needs no
change. The inventory test gains one assertion per set: its names, and for
standalone a byte total of at most half of 97,069.

### 5.4 The smallest usage counter

The stdio facade already calls the hub's HTTP API for every hub-backed
tool. It adds the tool's name as one request header on those calls; the hub
counts (tool, UTC day) in memory and writes the counts to one small table
once a minute; an admin reads them per day as names and counts. Nothing
about the call's content is stored. Tools answered locally
(`writing_rule`, `team_context`, `process_context` on a checkout) never
reach the hub and are not counted; the document that adds the counter says
so in its output.

## 6. The rule for a new tool

1. A new filter, view or field of something an existing tool already reads
   or writes is a parameter of that tool.
2. A new kind of record, or a write that needs its own authorization, is a
   new tool.
3. A tool that a program calls, not a model, is not listed: it is served by
   name or by a CLI command, like the reservation tools.
4. A new tool names its group in `toolGroup` and its sets in the inventory
   test; the test fails until it does, and fails if standalone exceeds half
   of 97,069 bytes.

## 7. The implementation's split

1. **The sets in the facade** (M, sensitive: the tool list is every
   client's wire contract): the four sets, the teams setting and the
   commands that write it, served-by-name with the notice line, the entry
   points' test, the inventory test's per-set assertions, and the team
   guides updated.
2. **The usage counter** (M, sensitive: the HTTP API and a store
   migration): 5.4, with the admin read.
3. **In aicrew** (on aicrew's board): drop `get_design_doc` and
   `process_context` from `aimemReadTools`, or ask aimem for them (3.3).

The text trims of 3.4 and 3.5 are follow-ups, measured by the inventory
test. The removal of the team protocol is the operator's decision once
aicrew is functional; the counter's figures inform it.

The three tools of feature 1 (`set_feature`, `confirm_feature_task`,
`list_features`) are in standalone and tasks-only; `list_features` is also
in member, as today.

## 8. Open questions

1. The teams setting on the hub's MCP: a URL parameter, or the credential's
   profile. The profile is cleaner but touches the access schema.
2. The length of the served-by-name period (5.2): two releases is proposed.
