# 02. A new project with a project-local task token

This card is the new-project path of card 01 with the other task
credential that [TASK-CREDENTIALS.md](../TASK-CREDENTIALS.md) describes,
the project-local override: a token scoped to one project, stored for one
checkout. An agent machine that should write to one project only, and to
no other the user is granted, uses it in place of the user task token.
The hub issues the token with `aimem access token-issue --project`; the
workstation stores it with `aimem task-token set` in the project's
checkout. The token writes to its project, and another project's process
is refused to it, so a release must keep both halves working.

Tier: release gate.

## Preconditions

- The hub, the workstation, `<token-file>`, `<skills-archive>` and the
  process repository as in [card 01](01-new-project.md).
- An empty project directory on the workstation. Run the workstation
  steps from that directory.
- `<project-token-file>` does not exist yet: step 9 writes it. The
  quickstart copies the token from the hub to the workstation; in the
  suite both sides read the same file. Delete it once step 12 has stored
  the token.

## Steps

### 1. Tasks are switched on for the project (hub)

This creates the project on the hub.

```sh
aimem tasks on -p <project>
```

```text
tasks on for project "<project>"
```

### 2. The skills release is imported (hub)

The process requires a skill, and members install skills only from the hub.

```sh
aimem skills import --from-archive <skills-archive>
```

```text
"imported":true,"source":"release"
```

### 3. The process is selected by tag (hub)

The project needs a process before agents work from its board.

```sh
aimem process select https://github.com/BlackVS/aimem.git <tag> -p <project>
```

```text
<tag> holds commit
```

### 4. A second project's tasks are switched on (hub)

The token of step 9 must not reach this project.

```sh
aimem tasks on -p <project-2>
```

```text
tasks on for project "<project-2>"
```

### 5. The second project's process is selected (hub)

So step 16 is refused by the token's scope, not for a missing process.

```sh
aimem process select https://github.com/BlackVS/aimem.git <tag> -p <project-2>
```

```text
<tag> holds commit
```

### 6. The user is added (hub)

The project token belongs to this user.

```sh
aimem access user-add --user-name <user>
```

```text
"name": "<user>"
```

### 7. The user is granted the project (hub)

The grant is what lets the user's tokens write to the project's tasks.

```sh
aimem access grant add --project <project> --user-name <user>
```

```text
"ok": true
```

### 8. The user is granted the second project too (hub)

So the refusal in step 16 comes from the token's scope, not from a missing grant.

```sh
aimem access grant add --project <project-2> --user-name <user>
```

```text
"ok": true
```

### 9. A project-scoped token is issued into a private file (hub)

The token writes to this one project only, and only its ID is printed.

```sh
aimem access token-issue --user-name <user> --label <machine> --project <project> \
  --expires <expires> --output <project-token-file>
```

```text
"scope": "project"
"expires_at": "<expires>"
```

### 10. The project is wired (workstation)

The checkout gets `.aimem.json` with the project ID and the hub's name.

```sh
aimem wire --project <project> --hub <hub-name>
```

```text
.aimem.json: written: created (project: <project>, hub: <hub-name>, groups: none)
```

### 11. The workstation adds the hub with the writer token (workstation)

A project-local credential is bound to the hub the checkout names, so the hub is registered first.

```sh
aimem hub add <hub-name> <hub-url> --token-file <token-file> --ca-file <ca-file>
```

```text
hub "<hub-name>" configured
```

### 12. The project token is stored for this checkout (workstation)

The hub checks the token's project scope and write access before it is stored.

```sh
aimem task-token set --token-file <project-token-file>
```

```text
Project-local task credential set; .aimem.json contains only the local requirement.
```

### 13. The checkout uses the project-local credential (workstation)

This names the source without showing the secret.

```sh
aimem task-token show-source
```

```text
"source":"project-local"
"project":"<project>"
"hub":"<hub-name>"
```

### 14. The process is what a session is told (workstation)

The project-local token reads its project's process.

```sh
aimem process show
```

```text
Process set: aimem <tag> @
```

### 15. A task is created with the project token (workstation)

The request an agent's task tools make, with the project-local credential.

```http
POST /v1/projects/<project>/tasks
Token-File: <project-token-file>
Idempotency-Key: scenario-02-first-task

{"title": "First task with the project token", "state": "BACKLOG"}
```

```text
201 Created
"title":"First task with the project token"
```

### 16. The token is refused another project's process (workstation)

The user is granted the second project, but this token is scoped to the first.

```sh
aimem process show -p <project-2>
```

```text
access to the process set was DENIED on this machine
```
