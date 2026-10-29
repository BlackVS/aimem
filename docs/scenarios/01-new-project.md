# 01. A new project on a fresh workstation, with a user task token

This card is the path of [the new-project quickstart](../QUICKSTART-NEW-PROJECT.md),
sections 2 and 3, on a hub that already runs and a workstation that has
never talked to it. On the hub, an administrator switches tasks on for the
project, imports the skills, adds the user and issues the user's task
token, onboards the project in one command (tasks on, the process by tag,
the grant) and binds the repository.
On the workstation, a person wires the project, adds the hub with the
writer token, stores the task token and checks what an agent will see. A
task is then created and listed through the hub's API with that token.
Last, a second project is onboarded the same way: the same token writes
to it with nothing changed on the workstation. This is the path a
new user follows first, so a release must not break any step of it.

Tier: release gate.

## Preconditions

- A hub machine with `aimem serve` running and its HTTPS listener on
  `<hub-url>`, with a certificate signed by the CA in `<ca-file>`. Run the
  hub steps as the hub's service account, as the quickstart's section 2
  does after `ssh` and `su`.
- A workstation with aimem installed and its `aimem serve` running, and an
  empty project directory. Run the workstation steps from that directory.
  The quickstart's one-liner installs and wires in one go; this card
  starts from the installed binary and wires the project with
  `aimem wire`.
- A writer token for the workstation in `<token-file>`, readable only by
  your account. Step 2 shows the command that issues one; copy its secret
  into that file, as the quickstart's step 3.3 does with `umask`, `cat` or
  `Get-Clipboard`.
- A skills release archive at `<skills-archive>`, version
  `<skills-version>`, with its `SHA256SUMS` beside it.
- The process repository `https://github.com/BlackVS/aimem.git` with the
  tag `<tag>`.
- `<task-token-file>` does not exist yet: step 6 writes it. The
  quickstart copies the token from the hub to the workstation; in the
  suite both sides read the same file.

## Steps

### 1. The hub's service answers (hub)

Every hub step talks to this service through its local socket.

```sh
aimem health
```

```text
"status":"ok"
```

### 2. A writer token is issued for the workstation (hub)

The writer token lets the workstation journal and share documents; its secret is printed once.

```sh
aimem token add <machine>
```

```text
restart is not required; the hub reads the registry per request
```

### 3. The skills release is imported (hub)

The process requires a skill, and members install skills only from the hub.

```sh
aimem skills import --from-archive <skills-archive>
```

```text
importing release set <skills-version>
"imported":true,"source":"release"
```

### 4. The users are listed before one is added (hub)

An existing user is reused rather than added twice.

```sh
aimem access list
```

```text
(no project has grants or tokens)
```

### 5. The user is added (hub)

The task board knows the agent by this user.

```sh
aimem access user-add --user-name <user>
```

```text
"name": "<user>"
```

### 6. The user task token is issued into a private file (hub)

The token follows the user's grants, now and later, and only its ID is printed.

```sh
aimem access token-issue-user --user-name <user> --label <machine> \
  --expires <expires> --output <task-token-file>
```

```text
"scope": "user"
"expires_at": "<expires>"
```

### 7. The project is onboarded in one command (hub)

This switches tasks on, selects the process by tag and grants the user; the user's token already writes, so none is issued.

```sh
aimem project onboard <project> --user <user> --process https://github.com/BlackVS/aimem.git <tag>
```

```text
  tasks: turned on
<tag> holds commit
  process: selected
  grant: granted user <user>
  token: none issued; user <user> already has one that can write: <machine> (user, expires
readiness: ready for work
```

### 8. The project is listed under the decided ID (hub)

A typo in the ID would show here as a second project.

```sh
aimem projects
```

```text
"<project>"
```

### 9. The project is bound to its repository (hub)

The repository's host names the forge credential a member needs.

```sh
aimem project repo set --project <project> --kind github --url <repo-url>
```

```text
now:      github <repo-url>
```

### 10. The project shows its repository and process (hub)

This is the check the quickstart asks for after binding the repository.

```sh
aimem project show --project <project>
```

```text
repository: github <repo-url>
tasks:      on
process:    https://github.com/BlackVS/aimem.git
(ref <tag>)
```

### 11. The project is wired (workstation)

This writes the handoff, the agent hooks and `.aimem.json` with the project ID and the hub's name.

```sh
aimem wire --project <project> --hub <hub-name>
```

```text
.aimem.json: written: created (project: <project>, hub: <hub-name>, groups: none)
project wired.
```

### 12. The workstation adds the hub with the writer token (workstation)

The hub's certificate is verified against the CA, and the token is read from a private file.

```sh
aimem hub add <hub-name> <hub-url> --token-file <token-file> --ca-file <ca-file>
```

```text
hub "<hub-name>" configured
```

### 13. The task tools get their credential (workstation)

The user task token is stored for the hub; it never goes on a command line.

```sh
aimem hub task-token <hub-name> --token-file <task-token-file>
```

```text
task credential stored for hub "<hub-name>"
```

### 14. The credential is active with user scope (workstation)

The workstation asks the hub about the token without showing it.

```sh
aimem hub credential <hub-name>
```

```text
individual credential set, active, scope user
```

### 15. The process's skills are installed from the hub (workstation)

The process names the review skill and the hub serves it; `--tool` names the client, which is found on `PATH` otherwise.

```sh
aimem skills install --tool claude
```

```text
installed oh-code-review <skills-version> (release) for claude
```

### 16. The local service answers (workstation)

Step 3.6 of the quickstart starts here.

```sh
aimem health
```

```text
"status":"ok"
```

### 17. The project has the pinned ID (workstation)

Every clone journals into the project the ID names.

```sh
aimem project-id .
```

```text
<project>
```

### 18. A session start carries the handoff (workstation)

This is the hook output an agent receives when it starts.

```sh
aimem session-start
```

```text
"hookEventName":"SessionStart"
Canonical session handoff (docs
```

### 19. The bound documents are published (workstation)

The handoff is shared through the hub without being listed.

```sh
aimem docs push
```

```text
SESSION-STATE -> rev 1
```

### 20. The hub lists the shared handoff (workstation)

This reads the documents back from the hub.

```sh
aimem docs list
```

```text
SESSION-STATE
docs/SESSION-STATE.md
```

### 21. The process is what a session is told (workstation)

It names the set, the tag and the commit, the manifest and the installed skill.

```sh
aimem process show
```

```text
Tasks (Kanban) are ON for project <project>.
Process set: aimem <tag> @
(manifest processes/aimem/manifest.json)
Required skills: oh-code-review (installed <skills-version> from the hub (release))
--- Checklist gating READY ---
```

### 22. A task is created with the user task token (workstation)

An agent's task tools make this same request; its success proves the credential, the grant and the board.

```http
POST /v1/projects/<project>/tasks
Token-File: <task-token-file>
Idempotency-Key: scenario-01-first-task

{"title": "First task from the workstation", "state": "BACKLOG"}
```

```text
201 Created
"title":"First task from the workstation"
```

### 23. The project's tasks list the new task (workstation)

The quickstart's last check: the task an agent created appears in the list.

```http
GET /v1/projects/<project>/tasks
Token-File: <task-token-file>
```

```text
200 OK
"title":"First task from the workstation"
```

### 24. The second project is onboarded in one command (hub)

The second project starts like the first, and the user's token already follows the new grant.

```sh
aimem project onboard <project-2> --user <user> --process https://github.com/BlackVS/aimem.git <tag>
```

```text
  tasks: turned on
  process: selected
  grant: granted user <user>
  token: none issued; user <user> already has one that can write
readiness: ready for work
```

### 25. The same token writes to the second project (workstation)

Nothing changed on the workstation since step 13.

```http
POST /v1/projects/<project-2>/tasks
Token-File: <task-token-file>
Idempotency-Key: scenario-01-second-project

{"title": "First task of the second project", "state": "BACKLOG"}
```

```text
201 Created
"title":"First task of the second project"
```
