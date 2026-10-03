# First pilot: aimem's side of the proposal

Status: proposal, 2026-10-04, for review. Nothing here is implemented; the tasks below follow the merged text. It is the aimem counterpart of the aicrew proposal *What the first pilot changes in aicrew and aimem* (aicrew `docs/proposals/`; until that merges, the hub document `PROPOSAL-PILOT-1`, project aicrew, revision 6). Section numbers in brackets, such as [2.1], refer to that document. The aimem work is tracked by task 01a102f8-bb79 (project repository, team registration, readable commands, secret flags) and task 01a102d9-440b (coordinator triage); both are on the aicrew board.

The first pilot (2026-10-03) proved the identity chain and the attempt protocol and stopped at the accept step on mechanisms that did not exist yet. On the aimem side four things were missing:
- a project did not know its repository, so a worker had to be told which one to clone;
- a team existed on the hub only as an unnamed UUID that the operator had to copy into a profile;
- the coordinator could not record its own READY assessment;
- the admin commands took several positional entities that a person could not tell apart.

This document says what aimem adds for each, and what it leaves to aicrew.

## 1. A project owns its one repository [2.1]

**Today.** A project has a process pin (`aimem process select REPO COMMIT MANIFEST -p PROJECT`), its data, and grants. Nothing records the repository its work happens in.

**Proposed.** The repository becomes a property of the project, owned by the hub, next to the process pin:

```sh
aimem project repo set --project example --kind github --url https://github.com/example/example.git \
  [--default-branch main] [--access write|read]
aimem project repo clear --project example
aimem project repo show --project example
aimem project show --project example
```

- `--kind` is `github`, `gitea` or `gitlab`. The credential kind a member needs follows from it; there is no separate credential declaration.
- `--url` is the clone URL (`https` or `ssh`), stored as given and never fetched by the hub.
- `--default-branch` is recorded for offers; without it the field stays empty and aicrew asks the forge.
- `--access` is what members need: `write` (branches and pull requests, the default) or `read` (projects members only consult).
- `project show` prints the repository, the process pin and the project's grants (users, groups and team profiles, with names beside IDs). `project repo show` prints the repository alone.
- One repository per project. Work across repositories is tasks in each project, with dependencies between them.
- The process pin stays where it is: `process select` is unchanged, and its repository is a separate, read-only requirement.

**Who may set and read it.** Setting or clearing is a project-admin operation, on the hub host like `process select` (the local service). Reading is open to anyone who can read the project's tasks: an ordinary token with a grant on the project, a team session whose profile is granted the project, and aicrewd through its peer credential (see the open questions in section 6). The repository holds no secret, only a URL and its declared needs.

**Changes.** Setting or clearing it is audited (`project.repository.set`, `project.repository.clear`) with the old and new values. A changed repository takes effect at the next offer; work already in progress keeps the repository its offer named.

## 2. A team registers its name on the hub [2.2]

**Today.** An operator creates a team access profile with `aimem identity team create SERVICE TEAM`, where `TEAM` is aicrew's team UUID copied by hand. The profile stores the service, the team UUID and its state, but no name, so every later command and every output names the team by UUID. At the pilot this produced a profile created under the team's name instead of its UUID.

**Proposed.** aicrewd registers each team it creates, by name and UUID, through its own peer credential:
- **A new credential operation `team.register`,** next to `identity.redeem` and `reservation.read`. The operator issues it once per peer, like the others: `aimem identity cred issue --peer <service> --operation team.register --expires 90d --output team-register.secret`.
- **The registration** takes the team UUID and its name. It is idempotent: the same UUID and name again returns the same profile; the same UUID with a new name renames it; a name already used by another team of that peer is refused with the existing team's UUID. It creates the profile, or updates its name, and nothing else.
- **No grants.** Registration never attaches a project. Project access stays the operator's: `aimem identity team grant --peer <service> --team-name <team> --project <project>` [2.3] is the only way a project reaches a team.
- **Profiles carry `team_name`,** unique per peer. Operator-created profiles get a name with `--team-name` at create, or later with `aimem identity team rename --peer <service> --team-id <UUID> --team-name <team>`.
- **Audited** like the other peer operations (`team_profile.register`, `team_profile.rename`), with the credential that did it.

## 3. Coordinator triage in team mode [5]

**Today.** A coordinated claim is accepted only on a task in state READY, unheld and not archived. A team session has no task write (`task_write: false`), so the coordinator could assess a task READY but not record it. The refusal of a claim on a BACKLOG task is the generic `reservation_conflict`, "the task is not held by this holder", which does not say why. At the pilot, an operator credential set READY by hand.

**The operator's open decision** is between two options, and this document does not choose:
- **(a) Triage authority for the coordinator role in team mode.** The team profile grants the coordinator role a bounded set of task writes on its granted projects: the state BACKLOG↔READY, the priority and size fields, `next_action`, and comments. aimem records the member as the actor. Workers get none of this, and DONE and CANCELLED stay where they are today.
- **(b) The READY assessment travels with the claim.** aimem accepts a coordinated claim on a BACKLOG task when the claim carries the offering coordinator's READY assessment, already bound to the coordination fact aimem verifies against aicrew. It moves the task to READY as part of the claim, in the same transaction, with the coordinator recorded as the actor of that transition.

**In both options:**
- **`task_not_ready`.** A claim on a task that is not READY is refused with its own code, `task_not_ready`, naming the task's state and what moves it (under (a), the coordinator's triage; under (b), a claim carrying the assessment). `reservation_conflict` keeps its meaning: a hold by someone else.
- **A partial task update.** Today `update_task` replaces the whole field set, so a triage edit that omits a field clears it; that is a trap for any agent. The increment adds a partial update: only the fields given change, under `expected_revision`, or a dedicated state transition. The existing full replace stays for clients that use it.

The aimem increment for the chosen option is filed once the operator decides (task 01a102d9-440b).

## 4. Readable admin commands [6.1]

**Rules.**
- Entity names are named flags: `--peer`, `--team-name` or `--team-id`, `--project`, `--user-name` or `--user-id`, `--operation`.
- A positional argument is kept only where a command has exactly one entity, such as `aimem identity peer check <service>`.
- Where an entity has an ID and a name there are two flags, exactly one of them required. An unknown name is refused with the known names of that scope.
- Output prints names beside IDs: `team grants`, `peer list`, `cred list`, `access list` and `project show`.
- Every command's `--help` ends with one complete example.
- The current positional forms keep working for one release and print a one-line notice naming the new form.

**Examples of the new form:**

```sh
aimem identity team create --peer <service> --team-id <UUID> --team-name pilot
aimem identity team grant --peer <service> --team-name pilot --project example
aimem identity team revoke --peer <service> --team-name pilot --project example
aimem identity team grants --peer <service> --team-name pilot
aimem identity cred issue --peer <service> --operation reservation.read --expires 90d --output read.secret
aimem identity cred revoke --peer <service> --credential <ID>
aimem access grant add --user-name pilot-worker --project example
aimem access token-issue-user --user-name pilot-worker --label pilot-worker --expires 2026-12-28T00:00:00Z --output worker.token
```

**Scope.** The `aimem identity …`, `aimem access …` and new `aimem project …` commands. Routes on the wire keep their names; only the CLI and its output change. The runbooks (`PILOT-HUB-RUNBOOK.md`) move to the new form in the same increment.

## 5. One flag per secret intent [6.4]

- **Writing a secret is always `--output <file>`.** It replaces `--secret-file` in `aimem identity cred issue|rotate`, and printing to standard output in `aimem access token-issue` and `token-issue-user`. The file must not exist; it is created readable only by its owner, and the value never reaches the terminal. The old flag and the stdout form keep working for one release with a notice.
- **Reading a secret is always `--token-file <file|->`,** `-` meaning standard input. It already exists on `aimem hub add` and `aimem hub task-token`, and the hub-admin bearer keeps its specific `--admin-token-file`, since it is a different secret with a different owner.

## 6. Order and open questions

**Order** [9]:
- One S increment covers sections 1, 2, 4 and 5: the project repository and `project show`, team registration with `team.register` and `team_name`, the readable commands, and `--output`.
- Section 3 is a second S increment, after the operator chooses (a) or (b).
- No release is implied by this document.

**Open questions for review:**
- **How aicrewd reads a team's grants and each granted project's repository and process pin** [2.3]. The proposal says "through its peer credential". The options are to extend `reservation.read` or to add a `team.read` operation; the second keeps each credential to one purpose and is the recommendation.
- **Whether a project repository needs a history,** or only the current value plus the audit. Only the current value plus the audit is the recommendation; an offer records the repository it named.
- **Under option (a), whether triage rights are a property of the role** (coordinator) **or a separate grant on the profile.** A property of the role is simpler, and is the recommendation.
