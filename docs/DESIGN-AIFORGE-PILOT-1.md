# First pilot: aimem's side of the proposal

Status: proposal, 2026-10-04, for review. Nothing here is implemented; the tasks below follow the merged text. It is the aimem counterpart of the aicrew proposal *What the first pilot changes in aicrew and aimem*, revision 9: aicrew `docs/proposals/` (aicrew PR #85), until then the hub document `PROPOSAL-PILOT-1` in project aicrew. Section numbers in brackets, such as [2.3], refer to that document, and the two texts move in step. Section 9 lists the only places where this text may differ from it. The aimem work is tracked by tasks 01a102f8-bb79 (project repository, team registration and reads, readable commands, secret flags) and 01a102d9-440b (coordinator triage), both on the aicrew board.

The first pilot (2026-10-03) proved the identity chain and the attempt protocol and stopped at the accept step on mechanisms that did not exist yet. On the aimem side four things were missing:
- a project did not know its repository;
- a team existed on the hub only as a UUID that the operator copied by hand;
- the coordinator could not record its own READY assessment;
- the admin commands took several positional entities that a person could not tell apart.

**Owners.** One owner per fact [principle 1]. The aimem hub owns projects and every property of a project: its data, its process pin, its repository, and the grants that give teams access to it. aicrewd owns teams, their names, members, roles and attempts. Each section below says which side owns what it adds, and nothing the hub owns is also declared in aicrew.

## 1. A project owns its one repository [2.1]

**Today.** A project has a process pin (`aimem process select REPO COMMIT MANIFEST -p PROJECT`), its data and its grants. Nothing records the repository its work happens in.

**Decision.** The repository is a property of the project. The hub owns it, next to the process pin:

```sh
aimem project repo set --project example --kind github --url https://github.com/example/example.git [--access write|read]
aimem project repo clear --project example
aimem project show --project example
```

- `--kind` is `github`, `gitea` or `gitlab`: the API dialect used to verify a credential against that forge.
- `--url` is the clone URL (`https` or `ssh`), stored as given and never fetched by the hub. **The host of the clone URL identifies the credential a member needs** (`github.com`, `gitea.example.org`), so two projects on two instances of one forge need two credentials. There is no separate credential declaration.
- `--access` is what members need: `write` (branches and pull requests, the default) or `read` (projects members only consult).
- **No default branch.** The forge owns the default branch, and the coordinator reads it at offer time [3.5]. A copy on the hub would be a second owner of a forge fact.
- `project show` prints the repository, the process pin and the project's grants (users, groups and team profiles, with names beside IDs).
- One repository per project. Work across repositories is tasks in each project, with dependencies between them.
- The process pin stays with `process select`, unchanged; the host of its repository adds a read requirement.

**Who sets and reads it.** Setting and clearing are project-admin operations on the hub host, like `process select`. They read the local service. Reading is open to:
- an ordinary token with a grant on the project;
- a team session whose profile is granted the project;
- aicrewd through `team.read` (section 3).

The property holds no secret. Setting and clearing are audited (`project.repository.set`, `project.repository.clear`) with the old and new values.

**Rationale.** A repository is a fact about the project, not about the team or an attempt. Keeping it on the hub means aicrewd keeps no project list of its own, and a project's repository changes in one place.

## 2. Team names: aicrewd is the writer [2.2]

**Today.** An operator creates a team access profile with `aimem identity team create SERVICE TEAM`, where `TEAM` is aicrew's team UUID copied by hand. The profile stores the service, the team UUID and its state, with no name. At the pilot this produced a profile created under the team's name instead of its UUID.

**Decision.**
- **One writer.** The team name has one writer, aicrewd. `aicrew team create` registers the team on the hub through `team.register` (section 3), and `aicrew team rename` re-registers it. The hub profile carries `team_name` as a mirror of aicrewd's name. The operator's team-profile create by name is replaced by registration, and the hub offers no operator rename of a team profile.
- **Unique per peer.** A name already held by another team of the same peer is refused with `team_name_taken`.
- **Grants bind the UUID.** A grant binds the profile's UUID, never its name, so a rename moves no grant.
- **The operator sees the UUID.** `aimem identity team grant` and `revoke` resolve `--team-name` within the given peer and print the UUID they acted on.

**Rationale.** Two writers of one name would let the hub and aicrewd disagree about the same team. A compromised peer could also rename its teams so that an operator's next grant by name lands on the wrong one. With aicrewd as the only writer, the grant bound to the UUID, and the UUID printed, the operator sees which profile received the project.

## 3. Two bounded peer operations [2.3]

Project access stays the hub's grant. `aimem identity team grant --peer <service> --team-name <team> --project <project>` is the only way a project reaches a team, and `revoke` detaches it. Both remain hub-admin operations, never a peer's.

aicrewd keeps no project or repository list of its own; it reads them through two operations. Each is permitted to a peer credential issued for exactly that operation, the identity.v1 rule since C6b. The operator issues each credential like the existing ones, for example `aimem identity cred issue --peer <service> --operation team.register --expires 90d --output team-register.secret`.

**`team.register`**
- **Input:** aicrew's team UUID and its name.
- **Scope:** it creates the profile for that UUID under the calling peer, or renames it. It acts only on profiles of the calling peer, never creates or touches a grant, never re-enables a profile the operator disabled, and changes nothing else on the profile.
- **Output:** the profile's UUID and name.
- **Refusals,** each by name:
  - `peer_forbidden`: the credential was not issued for this operation, or the UUID belongs to another peer;
  - `team_name_taken`: another team of this peer holds the name;
  - `profile_disabled`: the operator disabled this profile; the operator re-enables it, aicrewd does not;
  - `invalid_request`: a malformed UUID or name (identity.v1's existing code).
- **Audit:** peer, UUID, old and new name, outcome.

**`team.read`**
- **Input:** aicrew's team UUID, or none for all teams of the calling peer.
- **Scope and output,** for the calling peer's own teams only:
  - the profile's UUID, name and enabled state;
  - the granted projects;
  - for each granted project, its repository (kind, URL, access) and its process pin.

  Nothing else: no tasks, no members, no knowledge, no other peer's teams.
- **A disabled profile** is returned with `enabled: false` and no grants.
- **Refusals:** an unknown UUID answers `not_found`; a credential not issued for this operation answers `peer_forbidden`.
- **Audit:** peer, UUID, outcome.

**How aicrewd uses them [2.3].** aicrewd calls `team.read` at offer time, at `team show`, at `team check` and in its reconciliation loop.
- **At offer time** the hub's answer is authoritative for the grant and the repository; aicrewd's cache serves only `show` and its gap announcements.
- **The process pin an offer carries does not come from `team.read`.** The coordination contract keeps it from aicrewd's own record of the step, and aimem rechecks it at commit, as today. The pin `team.read` returns serves only `show`, `check` and the gap announcements.

**Rationale.** Neither operation can attach a project or reach beyond the calling peer's own teams. A compromised aicrewd peer can therefore rename and read its own teams, but it cannot grant itself access, read another peer's teams, or see tasks, members or knowledge.

## 4. Coordinator triage in team mode: decision (a) [5]

**Today.** A coordinated claim is accepted only on a task that is READY, unheld and not archived. A team session has no task write (`task_write: false`), so the pilot's coordinator assessed a task READY but could not record it. The refusal of a claim on a BACKLOG task is the generic `reservation_conflict`.

**Decision (operator, 2026-10-04): option (a).** The coordinator role gets task triage authority in team mode, through the team profile. It may set state BACKLOG and READY in both directions, and change priority, size, `next_action` and comments, on tasks of the projects the profile is granted. Five bounds apply:
1. **Held tasks are untouchable.** A task under a reservation (any member's, the coordinator's own included) refuses every triage write with `task_held`. The existing rule that generic writes are refused while held stays. Triage happens before a claim or after a release, never under a hold.
2. **Authority is the grant.** The write is allowed only on tasks of projects granted to the team profile, and only when aicrewd's team-mode context names the member's role as coordinator. A worker's team session keeps `task_write: false`. The role is a fact aicrewd asserts about its own team; the hub trusts the peer for it, as it already does for the coordination facts.
3. **The actor is the member.** aimem records the coordinator's user ID as the actor, as for every team write, so the task history shows who triaged.
4. **Partial update.** A triage write never replaces the field set. At the pilot, a full `update_task` cleared `epic` and `candidate_refs`.
5. **Readable refusal.** A coordinated claim on a not-READY task answers `task_not_ready` instead of `reservation_conflict`. It is status 409 with `retryable: no`, because the refused claim's coordination proof and key are spent, as for every coordination refusal. The next action: the coordinator triages to READY, then begins the step again through aicrew with a new offer.

**The aimem increment** (filed from this text, after it merges):
- the profile's triage capability for the coordinator role, taken from aicrewd's team-mode context;
- `task_held` and `task_not_ready`;
- a partial task update, or a dedicated state transition, that leaves unnamed fields untouched, with OpenAPI parity.

aicrew's side (the role in the context it reports, and `/crew-triage`) is task 01a102d9-440b.

**Why (a), and what it costs.** This comparison is the record of the decision [5.2]:

| | (a) the coordinator writes triage through the team profile | (b) the offer carries the READY assessment |
|---|---|---|
| Who may change task state in team mode | the coordinator, on unheld tasks of granted projects | nobody directly; aimem moves one task to READY as part of a claim it verifies against the aicrew offer |
| Proof required | the team session plus aicrewd's role assertion in the team-mode context | an aicrew-verified offer or independent claim naming that task; the assessment is bound to the fact, never taken from the claimer's body |
| Actor recorded | the coordinator's user ID | the coordinator from the verified fact, as for the claim |
| Held tasks | refused (`task_held`) | not reachable (a held task is already claimed) |
| What a compromised coordinator session can do | triage any unheld task of the granted projects: state between BACKLOG and READY, priority, size, next action, comments; no content edits beyond those fields, no held task, no other project | make READY exactly the task its own offer names, inside a fenced claim; nothing else |
| What a compromised aicrewd can do | assert the coordinator role for any of its own linked members and so enable triage writes on the granted projects, attributed to that real member and made through that member's own session; it holds no member bearer, so it writes nothing itself (the context contract's existing trust in the peer's role assertion) | assert an offer for any task of a granted project, which a member's claim then turns into READY and a hold; the same trust in the peer's facts |
| What it covers | all of triage: grooming, splitting, priority, returning to BACKLOG, before any offer exists | only "ready for this attempt"; the rest of triage keeps no path in team mode |
| Contract cost | context matrix coordinator row, team-mode report `task_write`, reservation.v1 `task_held`/`task_not_ready`, task API partial update | coordination.v1 amendment or new version after C5-w3 was declared the last in-place amendment, plus reservation.v1 `task_not_ready` |
| Board | shows triage as it happens; the board stays authoritative | shows BACKLOG until a claim lands |

(b) has the smaller reach for a compromised session. (a) was chosen for three reasons:
- the pilot needed the whole of triage, not one transition;
- the board must show the decision before an offer exists;
- (a) stays inside the existing authority model (grant plus role), while (b) couples aimem's coordination contract to aicrew's attempt semantics.

The reach of (a) is bounded by the grant, the hold rule and the field list above, and it is recorded per actor.

## 5. Readable admin commands [6.1]

**Rules** for `aimem identity …`, `aimem access …` and `aimem project …`:
- **Named flags.** Entity names are named flags: `--peer`, `--team-name` or `--team-id`, `--project`, `--user-name` or `--user-id`, `--group-name` or `--group-id`, `--operation`.
- **One positional at most.** A positional argument stays only where a command has exactly one entity, such as `aimem identity peer check <service>`.
- **Selectors.** Looking up an existing entity takes exactly one of its name or its ID, and an unknown name is refused with the known names of that scope. A command that creates an entity whose ID the hub generates takes only the name (`access user-add`, `group-add`). A command that renames one takes the ID plus the new name (`access user-set`, and any future rename).
- **`--project` and `-p`.** `--project` is the long form everywhere, and `-p`, which every project-taking command uses today, stays as its alias.
- **The `aimem project` namespace.** `aimem project <verb>` is the namespace of the new commands (`repo set`, `repo clear`, `show`). The existing `projects`, `project-id` and `drop-project` stay as they are, and their `aimem project …` forms are added as aliases in the same release.
- **Groups.** `access grant` subjects that may be a group take `--group-name` or `--group-id`, under the same exclusive rule as users.
- **Output.** Team profiles carry `team_name`, and output prints names beside IDs (`team grants`, `peer list`, `cred list`, `access list`, `project show`).
- **Help.** Every command's `--help` shows a full example.
- **Compatibility.** The old positional forms keep working for one release, each printing a one-line notice naming the new form.

**Examples:**

```sh
aimem identity team grant --peer <service> --team-name pilot --project example
aimem identity team revoke --peer <service> --team-name pilot --project example
aimem identity team grants --peer <service> --team-name pilot
aimem identity cred issue --peer <service> --operation team.read --expires 90d --output team-read.secret
aimem identity cred revoke --peer <service> --credential <ID>
aimem access grant add --group-name reviewers --project example
aimem access token-issue-user --user-name pilot-worker --label pilot-worker --expires 2026-12-28T00:00:00Z --output worker.token
```

Routes on the wire keep their names; only the CLI and its output change. The runbooks (`PILOT-HUB-RUNBOOK.md`) and TASK-CREDENTIALS move to the new form in the same increment.

## 6. One flag per intent and per secret kind [6.4]

- **Writing a secret is always `--output <file|->`.** It replaces `--secret-file` in `aimem identity cred issue|rotate`, and printing to standard output in `aimem access token-issue` and `token-issue-user`. A file must not exist and is created readable only by its owner. `-` writes to standard output for a pipe, and is refused when standard output is a terminal, so the value never goes to the terminal. `aimem identity enroll issue --output -` is the pipe's source (section 7).
- **Reading a secret: one flag per kind,** the same in every command that reads that kind. aimem reads these kinds:
  - the tool's own credential, the hub-admin token: `--admin-token-file`, as today, on every hub-admin command;
  - a hub token installed into an aimem installation (`aimem hub add`, `aimem hub task-token`): `--token-file <file|->`, as today.

  The enrollment subcode record that `enroll issue --output -` writes is read by aicrew's console client as `--bundle -`, from standard input only. A file is refused, because D1 keeps subcodes out of files, so the subcode exists only in the memory of the two processes in the pipe.
- **`-` reads standard input everywhere.** A hidden prompt when standard input is a terminal is a nicety, not a requirement.
- **Old names** keep working for one release with a notice.

## 7. Contract changes

Each row names the aimem document it amends and the section of this text that needs it. Nothing here is implemented by this proposal. Each change is a reviewed PR of its own, before or with the increment that relies on it [10].

| Contract | Change | Needed by |
|---|---|---|
| DESIGN-AIFORGE-CONTEXT, role matrix, `Aicrew service` row | "no mutation" gains one named exception, `team.register`, bounded to the peer's own profiles' identity (UUID, name) and never a grant; the read-only view gains `team.read` over the peer's own teams' grants and project properties | 2, 3 |
| DESIGN-AIFORGE-CONTEXT, role matrix, `Team coordinator` row; the team-mode report | the coordinator's team session gets `task_write: triage` (state, priority, size, `next_action`, comments) on unheld tasks of granted projects; the worker row is unchanged | 4 |
| DESIGN-AIFORGE-IDENTITY-WIRE (identity.v1) | two new single-purpose peer operations, `team.register` and `team.read`, with their routes, inputs, outputs, audit and refusals (`peer_forbidden`, `team_name_taken`, `profile_disabled`, `not_found`, and the existing `invalid_request`); `cred issue` accepts them as operations; the operator's team-profile create by name is replaced by registration; grant and revoke resolve `--team-name` per peer and print the UUID. Team operations under a disabled profile keep `context_stale` (unchanged) | 2, 3, 8 |
| reservation.v1 §3 (refusals, in DESIGN-AIFORGE-COORDINATION-WIRE) | `task_not_ready` (409, `retryable: no`; next action: triage, then a new step through aicrew) replaces `reservation_conflict` for a coordinated claim on a not-READY task; `task_held` (409, `retryable: no` while the hold stands) for a triage write on a held task | 4 |
| Task API (OpenAPI parity, TASK-CREDENTIALS) | a partial task update or dedicated state transition that leaves unnamed fields untouched; the team-mode triage capability in the access profile | 4 |
| DESIGN-AIFORGE-ENROLLMENT (D1) | one change: the composed onboarding envelope may be written by the issuing console client to an owner-only file on the operator's machine for the private hand-off, and the operator deletes it after sending. The member side is unchanged (hidden prompt, process memory), and no subcode file exists. The CLI surface D1 implies: `aimem identity enroll issue` with `--purpose`, `--hub`, `--expires` and `--output -` (standard output for a pipe, refused on a terminal), consumed by `aicrew invitation issue --bundle -`; `aimem identity enroll revoke --bundle <id>` by the non-secret bundle ID, run by the operator | 6, 8 |
| aimem CLI (TASK-CREDENTIALS, PILOT-HUB-RUNBOOK, admin docs) | the flag vocabulary of sections 5 and 6, with the one-release notice | 5, 6 |

**coordination.v1 is unchanged.** Option (b) would have amended it, and it was not chosen.

## 8. Failure modes

These are aimem's rows of [11]. aicrew's rows (offers, deposits, enrollment and capabilities) are in the aicrew text.

| Situation | Outcome | Recovery |
|---|---|---|
| Grant revoked while an attempt is running | aimem denies the next affected team operation (DESIGN-AIFORGE-CONTEXT lifecycle); the reservation is not released silently; aicrewd notices in reconciliation and marks the attempt blocked | the operator re-grants, or the attempt is closed under the coordination contract's closure rules; the hold is reconciled, never dropped |
| Profile disabled while an attempt is running | the next team operation is refused with identity.v1's existing `context_stale` (the audited reason is the disabled profile); `team.read` returns the team with `enabled: false` and no grants; aicrewd sets the attempt blocked | the operator re-enables it on the hub; aicrewd does not |
| `team.register` for a UUID the hub knows under another name | the profile is renamed to aicrewd's name (aicrewd owns it); grants, bound to the UUID, do not move; audited with both names | none needed; the operator reads the UUID that `grant` printed |
| `team.register` with a name another team of the peer holds | refused, `team_name_taken`; the team exists in aicrewd without a hub registration | `aicrew team rename`, then re-register |
| Triage write on a held task | refused, `task_held` | after the release, or the coordinator withdraws the attempt first |
| Coordinated claim on a not-READY task | refused, `task_not_ready`, not retryable (the proof and key are spent) | the coordinator triages to READY and offers again: a new step |
| `enroll issue` succeeded, then aicrew's console refuses the invitation | nothing is revealed and no file is written; the console prints the bundle ID and the revoke command | the operator runs `aimem identity enroll revoke --bundle <id>`, then retries the pipeline with a new bundle |
| An enrollment subcode leaks alone, before composition | at most a `new_user` with no grants and no membership, created by whoever redeems it | the operator revokes by bundle ID; if it was already redeemed, the operator disables that user and revokes its credential, since revoking a spent code does not revoke the token it issued (D1) |
| Repository of a project changed while an attempt is running | the attempt keeps the repository fields recorded at offer; new offers take the new repository | none for the running attempt; the coordinator decides whether to withdraw it |

**The repository of a running attempt** is aicrew's guarantee, not aimem's. aimem binds no repository to a hold: a claim and its coordination fact carry the process pin, never a repository. The offer records the repository fields, and aicrew keeps them for the attempt's lifetime [3.5].

## 9. Differences from the aicrew proposal

This text may differ from the aicrew proposal only in the places [6.5] names:
- **`aimem project repo show`** may exist as a narrower alias of `project show`; this text does not add it.
- **`--token-file -` reads standard input,** consistent with [6.4].

`--admin-token-file` and aimem's `--token-file` are not differences: under [6.4] they are aimem's flags for the tool's own credential and for a hub token (section 6). The operator rename of a team profile is withdrawn [2.2]. Any other difference is a defect in one of the two texts.

## 10. Order

Following [9]:
- One S increment covers sections 1, 2, 3, 5 and 6: the repository property and `project show`, `team.register` and `team.read` with their refusals, the readable commands and `--output`.
- A second S increment covers section 4: the triage capability, `task_held`, `task_not_ready` and the partial update.
- Each contract change of section 7 lands as a reviewed PR before or with the increment that relies on it.
- No release is implied by this document.
