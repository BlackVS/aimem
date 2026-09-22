# Team audit timeline and export contract

This implements the core communication-audit slice of [agent teams](DESIGN-agent-teams.md)
("Communication logs and improvement evidence"): filtered reads of a team's
accepted audit events and a paged JSONL export of events plus message metadata,
for post-analysis and process improvement. Both are administrator reads over
the durable records that earlier increments already write in the same
transaction as each accepted command; nothing here adds records, changes
retention or derives metrics.

## Records and what they mean

An **event** is the audit record of one accepted command (configuration,
session, message, assignment, handoff, managed-task and token operations). It
carries the operation name, server timestamp and sequence, the acting identity
IDs (user, token and admin name; never a token value), request ID, server and
process versions, the team snapshot, and the affected session, assignment,
handoff, managed change, token rebind or message IDs. Rejected or failed
requests produce no event; they appear only in the rotated server diagnostics.

A **message record** is metadata about one stored message: ID, sequence, sender
session and generation, the profile revision and reported profile exactly as the
sender declared it (an unknown or absent model stays unknown; nothing is
inferred), creation time, recipient, kind, task, attempt and reply references,
the hub-authored lifecycle record when present, and one **delivery record** per
recipient session. `delivered_at` and `delivery_count` mean an inbox response
containing the message was prepared for that session; they are not proof that
a client received it or that a model consumed it. `acked_at` is the client's
explicit acknowledgement, not proof of understanding, an answer or acceptance.
An answer is a separate message whose `reply_to` names the question. Message
bodies are excluded by default and included only on request.

## Storage

`TeamAuditFilter` narrows a read: `session_id`, `task_id`, `attempt_id`,
`operation` (an exact name such as `team.assignment.offer`, or a prefix ending
in a dot such as `team.assignment.`), and `since`/`until` (RFC3339, normalized
to UTC, half-open on the record's server timestamp). Filters intersect.

`TeamAuditSnapshot` returns the team's current upper event and message
sequences with the server time. `TeamTimeline` returns sequence-ordered events
with sequence in `(after, up_to]` that match the filter: a session matches the
event session, the assignment worker or either handoff side; a task matches the
assignment or managed task; an attempt matches the assignment or managed
attempt. `TeamMessageAudit` returns sequence-ordered message records in the
same window: a session matches the sender or a delivery recipient, task and
attempt match the message references, and an operation filter selects lifecycle
messages by their operation. Both refuse unknown teams, disabled projects and
invalid filters, and are bounded by the page limit (1-100 at the transport); a
sparse filter walks the team's records in order until the page fills.

## HTTP and CLI

Both routes are admin-only under `/v1/projects/{p}/teams/{team}` and run the
full admin gate on every request, including every export page. Unknown or
repeated query parameters are refused so a misspelled filter never widens a read.

`GET /events` accepts the filter parameters with `after` and `limit` and returns
the usual page with `next_cursor`.

`GET /export` returns one page of `application/x-ndjson`:

1. a `header` record: `schema_version` (1), `protocol_version`, `project`,
   `team_id`, the normalized `filters`, `include_bodies`, `limit`, the
   `snapshot` (`event_sequence`, `message_sequence`, `taken_at`), `first_page`,
   `exported_at`, `server_version` and `request_id`;
2. up to `limit` `event` records, then up to `limit` `message` records;
3. an `end` record: `events` and `messages` counts on the page, `complete`, and
   `next` (`after_events`, `after_messages`, `snapshot_events`,
   `snapshot_messages`).

The first page takes the snapshot. A continued page passes all four `next`
values back (plus the same filters, limit and bodies choice), so records
accepted after the snapshot never appear, and `complete:true` marks the last
page. A page is read completely before anything is written, so failures are
ordinary error responses, not truncated files.

`aimem teams events PROJECT TEAM [key=value ...]` prints one page;
`aimem teams export PROJECT TEAM [key=value ...]` follows the pages at one
snapshot and writes a single header, every record and the final end record to
stdout. It stops with an error if a page lacks an end record or the cursor does
not advance.

## Retention and gaps

Accepted audit records are never purged automatically in v1; size limits
surface pressure instead of discarding history. Server diagnostics (rejections,
authorization failures, storage failures, request timing) use the service log
sink and its rotation and are not part of this export: a gap between an event
and its request diagnostics is expected once the log has rotated, and the
absence of a delivery observation is never proof of non-delivery. Analysis
(delays, unanswered questions, retries, correction cycles) and correlation with
operator diagnostics are later increments built on this export.
