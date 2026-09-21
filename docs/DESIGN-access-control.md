# Users, Groups, Project Access, And Tokens

Status: **access foundation and task integration released; console access
management pending (stage 4)**. Foundation: PR #35; task service and page:
PR #39-41, released in v0.4.0; identity directory: PR #52, released in
v0.5.0. Companion to
[Kanban](AIMEM-KANBAN-PROPOSAL.md). Keep the first version small.

The token-model amendment below precedes stage 4; its release version is not assigned.
Reuse aimem's built-in HTTPS
listener and bearer-token validation. No authentication proxy, OAuth server,
or separate login service is required. A reverse proxy is optional; if used,
it must preserve each client's Authorization header rather than substitute a
shared admin token. This describes the intended contract, not a verified live
proxy configuration.

## Model

### Token-model amendment (2026-09-21, implementation in progress)

This amendment supersedes the original project-only write-token contract
below. Ordinary tokens have an explicit `scope`: `user`, `project`, or
`read-only`. User-scoped tokens follow the user's current direct/group
grants on each write; adding a grant requires no token reissue. Project
tokens additionally restrict writes to one project instance. Read-only
tokens never write. All modes retain expiry/revocation, enabled-user and
task-enablement checks; task-read visibility is unchanged.

Access schema 2 adds token scope. Existing nonempty project tokens migrate
to `project`; empty-project tokens migrate to `read-only`, never `user`.
Project databases and legacy writer/admin tokens are unchanged. Older
binaries refuse access schema 2; back up the state root before upgrading.

HTTP issuance accepts optional `scope`; when omitted, the old project-or-
read-only interpretation remains. Explicit `user` and `read-only` require
an empty project, and `project` requires a project. Old hubs reject the new
field, so clients cannot accidentally obtain a broader/different token.
`aimem access token-issue-user <user-id> <label> <expiry>` issues a user token;
the existing `token-issue <user-id> <label> <project|-> <expiry>` is unchanged.
Issuance and identity/snapshot responses expose the scope, never secrets
except the one-time issuance response. Identity and task/epic/comment writes
share the same fresh token/grant authorization check, including MCP dispatch.

User-scoped tokens may be issued before any grants exist; they gain no
write authority until granted. Project tokens still require a current grant
at issuance. Renaming preserves project-instance grants; deletion/recreation
does not inherit them. Existing clients can use user tokens through their
per-hub task credential setting. Repository-local project-token overrides
are a separate client increment; no secret belongs in tracked `.aimem.json`.

- **Users** identify people or the owners of agent credentials.
- **Access groups** contain users and simplify assigning several users to projects.
- **Project assignments** allow a user or access group to work on an existing
  aimem project. No new project registry.
- **Tokens** authenticate a user or their agent. Ordinary agent tokens restrict
  writes to one working project; they do not grant more access than their user has.
- **Admin** uses the existing full-access admin token, obtained only directly
  from the aimem host console.

Access groups are separate from existing **knowledge groups**, which organize
shared knowledge. Editing `.aimem.json` or joining a knowledge group cannot grant
permissions. Access-group membership and project assignments are managed by admin.
No nested groups or custom role language in v1.

## Permissions

| Caller | Read tasks | Change tasks |
| --- | --- | --- |
| Admin token | All projects | All projects; full access to other aimem administration/data too |
| User/agent with project assignment and matching project token | All projects on this hub | Assigned working project only |
| Ordinary authenticated token without matching project write access | All projects on this hub | No |
| Unauthenticated caller | No | No |

A write requires both current user access (directly or through an access group)
and the token's write-project restriction to match the task's actual project.
A user assigned to several projects can have a separate agent token for each.
Removing the user's last assignment to a project removes write access there.
Disabled users and expired/revoked tokens cannot access tasks.

Task comments use the same permissions: all authenticated task readers may read
them; appending requires project write access. Comment authorship comes from the
authenticated caller, using stable user/token IDs for ordinary credentials.
Comments are append-only in v1, so no role has a comment edit/delete operation.

The hub enforces this for HTTP and MCP alike. Request parameters, current
working directory, and task assignment are not authorization. Admin has full
access but still uses normal data-integrity checks such as revision matching.
Task ownership does not grant permissions or lock other same-project agents out.

## Token Management

Preserve the current opaque bearer-token approach: generate a random secret,
show it once, store only its digest. Ordinary token metadata needs a user ID,
label, optional write-project, expiry, and revocation state. Read-only tokens
have no write-project. Never put tokens in task URLs or documents.

Admin manages users, groups, project assignments, and ordinary tokens through
simple admin pages and corresponding HTTP/CLI operations. Issue, list metadata,
and revoke are enough initially; rotation means issuing a replacement and
revoking the old token. No self-service issuance or delegated administration.

**Admin tokens can only be obtained at the aimem host console.** Web/API/MCP
cannot issue, reveal, or promote a token to admin. Existing environment/local
admin-token mechanisms retain their full-access meaning. Admin is not a role
that can be granted through ordinary user/group editing.

Keep the existing token login flow for the UI. User records do not require
passwords, email invitations, SSO, or a new login system in v1.

The normal setup is: admin creates a user, assigns that user or their access
group to an existing project, and issues an ordinary token for that project.
The operator configures the token in the agent's MCP connection or local secure
credential settings. An HTTP-only client uses the same token in its Authorization
header. aimem validates the token and current project access on each request;
the model does not send secrets as tool arguments or choose its own permissions.

## Implementation Boundaries

Baseline v0.3.31 has named writer/admin tokens, but not users, access groups,
project assignments, or expiring project-scoped tokens; see
[tokens.go](../internal/server/tokens.go). The first increment adds those through
[the access store](../internal/access/store.go) and
[admin endpoints](../internal/server/access.go).

Current MCP has two paths: local `aimem mcp` uses stdio and trusts the local
process/socket, with no separate agent login; remote `/mcp` authenticates the
connection with a bearer token. Hub document/collection calls use the configured
hub token. None of these establishes a distinct user and working-project grant
today. The remote MCP handler also needs to carry authenticated caller identity
into task operations instead of relying on its shared local API client; see
[MCP handler](../internal/mcp/mcp.go). New task tools use the user's project token
behind the scenes; the model does not need to put secrets in tool arguments.

Store identity, memberships, and ordinary-token metadata in one hub-local store;
use one authorization function for all task surfaces. Record who changed access
and who changed tasks. Keep IDs stable through display-name changes; project
rename must preserve grants, and delete/recreate must not inherit stale grants.
Admin credentials retain their current host-managed path; do not create a second
writable copy of their secrets in the user store. A lookup failure must reject an
ordinary credential rather than retry it with admin or legacy-writer authority.

Do not sync credentials or access grants through knowledge-group/journal sync.
Do not let a limited token fall through into the existing global writer role on
other API routes. Existing broad credentials need an explicit compatibility path;
this proposal does not claim all legacy aimem data already has project isolation.
Task calls through a trusted local socket must not bypass agent project checks.

## First Delivery

1. Users, access groups, existing-project assignments, and ordinary token issuance
   and revocation, preserving host-console admin access.
2. Shared authorization for task HTTP/MCP operations and small admin management UI.
3. Tests for own-project writes, other-project reads, rejected cross-project
   writes, membership removal, disabled users, expired/revoked tokens, full admin
   access, and refusal to issue admin tokens remotely.

No custom permissions engine, group nesting, separate service-account hierarchy,
federation, or cross-hub identity sync. Auth/storage changes use the repository's
sensitive-surface review gate. This document creates no live users or tokens.

The first implementation increment is the identity/access store, admin-only
management operations, and permission tests. It does not yet add the task board.
Pass criteria: a host-admin credential retains full access; an ordinary token
resolves to its user/project; disabled/revoked/expired access is rejected; and
remote requests cannot obtain an admin token or escalate an ordinary credential.

## First Increment: Implemented Scope And Use

The access store is `access.db` in the hub state root (its own schema version 1).
Existing project database schema and legacy `tokens.json` stay unchanged. An
ordinary token has an `aimem_user_` prefix, an expiry at most 366 days away, and
only its digest is stored. Authentication checks revocation/expiry/user state on
each request. Administration changes and audit records commit together.

The server lazily opens one access-store handle shared by authentication and
management routes and closes it at shutdown. Failed opens are retryable; ordinary
authentication does not create a missing store. Opening an already current schema
does not take a migration write lock. Identity and permission results are not
cached, so revocation and membership changes remain effective on subsequent calls.
Stop the service before replacing or restoring its access database file.

Project grants bind to a generated, host-local `access-id` file in the existing
project directory. It moves on rename and disappears on drop; a newly created
project receives a new identity. This deliberately avoids writable/synced meta
keys. Keep the file with the project when backing up/restoring. A merge does not
transfer the removed source project's grants to its destination; the administrator
must deliberately grant destination access. Access state is not ordinary sync data.

With the updated local service running, use `aimem access` on its host:

```text
aimem access user-add Alice
aimem access group-add Developers
aimem access member add <group-id> <user-id>
aimem access grant add <project> group <group-id>
aimem access token-issue <user-id> agent <project> <expiry-RFC3339>
aimem access list
aimem access token-revoke <token-id>
aimem access user-set <user-id> Alice disabled
```

Use `-` instead of the project to issue a read-only token. `list` returns IDs,
membership/grants, token metadata, a project-instance/name map, and the last 100
audit entries; no secrets or digests. The project map includes only readable,
existing access identities; unrelated damaged or uninitialized project directories
do not prevent listing users or tokens, and listing does not create identities.
The token issue response displays the secret
once. For direct assignment use `grant add <project> user <user-id>`; `member rm`
and `grant rm` remove the corresponding access path.

After project deletion or merge, use the stored `project_instance` from `list`
with `aimem access grant-rm-instance <project-instance> <user|group> <subject-id>`.
Its admin-only HTTP route is `DELETE /v1/access/grants/{instance}/{kind}/{id}`.
Removal is audited and can be retried. It only removes the specified grant and
does not resolve a project name, so recreating that name cannot target the new
project accidentally. Revoke obsolete project tokens separately by token ID with
`token-revoke`; their metadata and past audit records are retained. Cleanup is
explicit administration, not an automatic cross-database project-delete operation.

Admin HTTP clients use `/v1/access`, `/v1/access/users`, `/v1/access/groups`,
`/v1/projects/{p}/access/{kind}/{id}` and `/v1/access/tokens`; exact bodies and
member/revocation routes are in OpenAPI. `GET /v1/access/identity?project=<id>`
accepts ordinary tokens and reports identity and current task-write eligibility.

This increment does not expose task CRUD, task MCP or a management dashboard.
Ordinary tokens are denied all existing non-public endpoints except identity
inspection, so they cannot use a legacy route or remote MCP as an unrestricted
writer. The next task increment must add explicit route/tool authorization;
eligibility here is not a claim that task operations already exist.
