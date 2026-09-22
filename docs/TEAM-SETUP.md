# Team setup for administrators

This is the first implementation slice of [agent teams](DESIGN-agent-teams.md).
It stores team configuration and enrollment. Agent join, roster, messaging and
assignment are not available yet. Standalone task workflows are unchanged.

Run the commands on the hub host as its service user, with the local service
running. Team routes require administrator authority; ordinary agent tokens
cannot configure or inspect this administrative surface. The target must be
an existing ordinary project with tasks enabled.

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
surrounding whitespace or line/tab controls. Description is limited to 4096 bytes;
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
`/v1/projects/{p}/teams/{team}/events` (GET). Writes carry `Idempotency-Key`.
Lists default to 50 records; `limit` accepts 1-100 and `after` continues from
`next_cursor` (team ID for lists, sequence for events). CLI list/events currently
print one page; use HTTP for further pages. A zero/empty cursor ends pagination.
Responses identify protocol version 1 and the list advertises only implemented
administrative operations.

Accepted changes and audit events commit together with their retry receipt.
Audit records contain actor identity, server timestamp, old/new revision, full
configuration, request ID, protocol/server version and selected process commit
when present. Request diagnostics include status, duration and correlation ID,
including failed authentication or malformed JSON, without request bodies,
credentials or retry keys. They use the existing service log sink and its
retention; they are not a durable delivery log. Rich audit export/analysis is a
later increment.

Project schema 14 adds the administration tables; schema 15 adds internal session
storage. Agent session HTTP/CLI/MCP operations are not exposed yet. Back up the
full state before upgrading and restore it with the previous binary if rolling
back: old binaries refuse schema
15. Renaming a project preserves its team IDs and access identity. Drop and merge
refuse projects containing team state, including empty teams; no deletion or
retention workflow is provided in this slice. Teams are excluded from memory
curation and synchronization and belong to one authoritative hub.
