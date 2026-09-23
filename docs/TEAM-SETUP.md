# Team setup for administrators

This is the first implementation slice of [agent teams](DESIGN-agent-teams.md).
It stores team configuration and enrollment. Agent join, roster and messaging
are described in [TEAM-AGENT-QUICKSTART.md](TEAM-AGENT-QUICKSTART.md).
The full execution workflow is not available yet; the
[HTTP assignment primitives](TEAM-ASSIGNMENT-HTTP.md) are for protocol validation.
The schema 17
[storage foundation](TEAM-ASSIGNMENT-STORAGE.md) protects managed tasks;
standalone workflows for unmanaged tasks are unchanged.

Run the commands on the hub host as its service user, with the local service
running. Team routes require administrator authority; ordinary agent tokens
cannot configure or inspect this administrative surface. The target must be
an existing ordinary project with tasks enabled.

## Guided provisioning

The normal path needs no JSON file. On the hub host, as its service user:

```sh
aimem teams provision create PROJECT --team "Pilot" --coordinator alice --expiry 2026-12-31T00:00:00Z
aimem teams provision add PROJECT --team "Pilot" --member worker-b --role worker --create-user --expiry 2026-12-31T00:00:00Z
aimem teams provision add PROJECT --team "Pilot" --member bob --role coordinator --no-token
```

Each run resolves the user by exact name or ID (a name shared by two users
must be given as an ID; a missing user is created only with `--create-user`),
sets the project grant, creates the team with its first coordinator or adds
the member to the existing team's enrollment at the read revision (one retry
on a revision conflict; unrelated enrollment and settings are kept; a worker
request never removes an existing coordinator flag), and issues that member
one project-scoped token labelled `team-<team>-<user>` (or `--label`). The
secret is shown once, at the end, on its own line, with the installation
line for the member's checkout (`printf '%s' "$SECRET" | aimem task-token
set`); `--secret-file PATH` writes it to a new file with mode 0600 instead
and prints nothing secret (on Windows the file carries its directory's
ACL rather than a Unix mode, so keep it in a private directory and delete it
after delivery). Every step is reported as existing or created, so
a rerun after a partial failure duplicates nothing: an existing user, grant,
team or enrollment is reused, and a live token with the same label is never
reissued (a lost secret means revoking that token and issuing another under
a new `--label`). Creating an enrollment creates no session: the member
joins with `/join_team` under its own token. Ordinary member tokens cannot
run this; `aimem teams setup` prints these commands as the operator handoff
when a member lacks a grant or an enrollment.

## Configuration files

Create access users with `aimem access user-add NAME`, then use their IDs in a
UTF-8 JSON file. Enrollment is separate from project grants: it grants no project
access, and coordinator eligibility does not create an active coordinator session.

```json
{
  "name": "Compiler team",
  "description": "Compiler maintenance",
  "enrollment": [
    {"user_id": "USER_ID", "coordinator": true}
  ]
}
```

```sh
aimem teams create PROJECT team.json create-team-1
aimem teams list PROJECT
aimem teams show PROJECT TEAM_ID
aimem teams events PROJECT TEAM_ID
```

Names are unique within the project, case-sensitive, 1-128 UTF-8 bytes without
surrounding whitespace or line/tab controls. An enrolled member sees its own
teams through `aimem teams mine PROJECT` (or the `team_list` MCP tool), read
only and without a session; a team literally named `mine` is reached by ID on
the admin routes. Description is limited to 4096 bytes;
at most 100 distinct existing users may be enrolled. Disabled users may be listed
in configuration; enrollment never re-enables them or widens their grants.

To edit, read the latest team, prepare a file containing the complete `name`,
`description`, `enrollment`, and `expected_revision` from that read, then run:

```sh
aimem teams configure PROJECT TEAM_ID replacement.json update-team-1
```

Omitted description/enrollment clears them. A stale revision returns HTTP 409
with the current configuration: review it and retry using a new key for a new
request. Reuse the same key and identical content only to retry an uncertain
write; it returns the original result without adding another audit event. An
omitted key is generated for that invocation, so use an explicit key when a retry
may be needed. Credentials and audit context are never configuration fields.

The HTTP surface is `/v1/projects/{p}/teams` (GET/POST),
`/v1/projects/{p}/teams/{team}` (GET/PUT), and
`/v1/projects/{p}/teams/{team}/events` (GET) and
`/v1/projects/{p}/teams/{team}/export` (GET). Writes carry `Idempotency-Key`.
Lists default to 50 records; `limit` accepts 1-100 and `after` continues from
`next_cursor` (team ID for lists, sequence for events). Events accept the audit
filters and the export streams JSONL pages of events and message metadata; see
[TEAM-AUDIT-EXPORT.md](TEAM-AUDIT-EXPORT.md). CLI list/events print one page;
`aimem teams export` follows all pages. A zero/empty cursor ends pagination.
Responses identify protocol version 1 and the list advertises only implemented
administrative operations.

Accepted changes and audit events commit together with their retry receipt.
Audit records contain actor identity, server timestamp, old/new revision, full
configuration, request ID, protocol/server version and selected process commit
when present. Request diagnostics include status, duration and correlation ID,
including failed authentication or malformed JSON, without request bodies,
credentials or retry keys. They use the existing service log sink and its
retention; they are not a durable delivery log and are not part of the audit
export. Analysis over the export is a later increment.

Project schema 14 adds administration, 15 sessions and 16 durable messages.
See TEAM-AGENT-QUICKSTART.md for agent HTTP/CLI/MCP operations. Back up the
full state before upgrading and restore it with the previous binary if rolling
back: old binaries refuse schema 16. Renaming a project preserves its team IDs and access identity. Drop and merge
refuse projects containing team state, including empty teams; no deletion or
retention workflow is provided in this slice. Teams are excluded from memory
curation and synchronization and belong to one authoritative hub.
