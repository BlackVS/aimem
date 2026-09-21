# Task credentials on an agent machine

User-scoped tokens follow the user's current direct/group project grants.
Adding or removing a grant does not require reissuing the token. Choose an
expiry appropriate to the token's access to all currently granted projects.
Project-scoped tokens restrict writes to one project instance and still
require a current grant. Read-only tokens never permit writes.

The client chooses one credential:

1. A required project-local override, when `.aimem.json` contains
   `"task_credential": "local"`.
2. Otherwise the OS user's task credential for the project's configured hub.

Both are ordinary credentials, separate from the hub's checkpoint token.
Existing per-hub project/read-only tokens keep their restrictions; changing
the client does not change their scope. Projects still need Kanban enabled.

## User-level credential

On the hub host, an administrator issues the user-scoped token:

```sh
aimem access token-issue-user USER_ID token-agent-workstation EXPIRY_RFC3339
```

On the agent machine, configure that token for the existing hub:

```sh
aimem hub task-token HUB_NAME "$USER_TOKEN"
```

This setting applies to projects using that hub unless they require a local
override. Adding another project grant updates the token's authority on the
hub without changing this setting. Never use an admin/checkpoint credential.

## Project-local override

Ask the hub administrator for a project-scoped token using the existing
`aimem access token-issue USER_ID TOKEN_LABEL PROJECT EXPIRY_RFC3339` command.
Run the following from the configured project root on the agent machine:

```sh
printf '%s' "$PROJECT_TOKEN" | aimem task-token set
aimem task-token show-source
```

In PowerShell, pipe the variable directly:

```powershell
$ProjectToken | aimem task-token set
aimem task-token show-source
```

`set` checks the token with the hub and requires project scope and current
write access to the configured project. It preserves other `.aimem.json`
fields and adds only the nonsecret local requirement. `show-source` reports
the source, project, hub name and checkout directory, never the secret.
It inspects local configuration; it does not promise the token is still
valid on the hub.

The secret is stored outside the checkout in the user's aimem state
directory, bound to the canonical checkout path, project ID, hub name and
exact hub URL. Unix uses owner-only permissions; Windows encrypts with
current-user DPAPI. Writes replace files through temporary files. A state
directory inside the checkout is refused for local credentials.

A missing, unreadable, malformed, revoked, expired or mismatched override
is an error, even if the global credential would work. Local overrides
require a hub that reports explicit token scope (the user-token model).
Removed grants also refuse local use. Local-override requests do not follow redirects.
Task calls re-read credentials; restart the agent session to refresh the
tool listing. Session-start process context also respects the local
requirement and never substitutes a checkpoint token for a failed override.

Configure each clone and worktree separately. A copied requirement marker
has no accompanying secret and deliberately blocks fallback until you run
`set` there. Moving the checkout or changing its project/hub binding also
requires setting the credential again. Start agents and run these commands
from the configured project root.

To explicitly return this checkout to the per-hub user credential:

```sh
aimem task-token clear
aimem task-token show-source
```

This clears the requirement and local secret. It does not revoke the token
on the hub; use `aimem access token-revoke TOKEN_ID` there when retiring it.
Removing the secret file alone does not restore fallback.

This client feature is part of the token-model rollout. Additional-project
onboarding remains on hold until the complete hub/client release is deployed
and the two-project workflow is verified.
Older clients do not understand the local marker and continue using their
per-hub token. Upgrade every participating agent client before relying on
repository-local selection; the marker cannot constrain an older binary.
