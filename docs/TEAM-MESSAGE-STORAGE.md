# Team message storage contract

This implements the storage portion of [agent teams](DESIGN-agent-teams.md).
The messaging transports (HTTP, CLI and MCP) enforce live ordinary write-token
scope and project grants on every call, including retries and reads, as the
session transport does.
The access database is separate; these project transactions do not provide
cross-database authorization atomicity.

`SendTeamMessage` accepts a bound session handle, recipient, typed content and
retry key. Sender identity, generation, accepted profile and profile revision
come from that session. New sends require an active session and current
generation. Current enrollment and credential binding are checked before receipt
lookup; an identical retry can recover its original result after a generation
change without repeating effects or making the old handle valid again.

Kinds are question, answer, note, progress, blocker and review_feedback. Content
is bounded to 32 KiB including JSON encoding. Question deadlines are optional
RFC3339 timestamps, not timers. Answers must reference an existing same-team
question; other replies must also stay within the team. Task IDs and typed task
references must exist in the same project. Other typed references use the task
reference format; they are descriptive links, not verified external resources.
Client messages cannot carry attempt references, the kind `lifecycle` or the
recipient kind `participants`: those belong to hub-authored lifecycle messages
(below). No message kind or text creates an assignment or changes task ownership.

A member recipient identifies a session, not a user. A broadcast snapshots all
active, enrolled sessions at send time, including the sender. Sessions that lost
coordinator eligibility are excluded; missing heartbeats do not exclude them.
Recipient access credentials are rechecked by the future transport when reading,
not inspected across databases by the send transaction. Later joins receive no
old broadcast delivery but can read team history. Direct routing is not private:
every current team member can read the complete team history.

`TeamMessages` returns sequence-ordered pages of 1–100 messages after a cursor,
with a next cursor and has-more flag. History reads do not mark delivery. Inbox
reads return only that session's unacknowledged messages and atomically record
a delivery attempt before returning. The stored delivered_at timestamp and
delivery_count mean a response was prepared, not that the client received or
understood it. Lost responses can be retried from the same cursor; start at zero
to recover every outstanding item. A cursor is a paging position, never an ack.
Bounded polling and request-rate limits belong to the transport increment.

`AckTeamMessages` accepts 1–100 distinct explicit message IDs already offered in
that session's inbox. A batch is all-or-nothing. Repeating an acknowledgement
does not repeat its effect. An acknowledgement does not constitute an answer,
task acceptance or completion. New acknowledgements require the current active
generation; receipt replay follows the send rules above.

Message content, recipient snapshot, send audit and retry receipt commit together.
Delivery attempts and acknowledgements commit with their corresponding audit
events. Audit events reference message IDs without copying message text; message
history retains the accepted sender profile. The default maximum is 10,000
messages per team; the internal API accepts a trusted limit from 1 to 1,000,000.
New sends at capacity fail while exact retries remain available. There is no
automatic pruning or archival in this increment.

## Lifecycle messages

The hub writes one message of kind `lifecycle` in the same transaction as every
assignment transition (offer, accept, decline, withdraw, block, resume-work,
cancel, stopped, close-stop, submit, review, recover and the rebind on worker
resume) and every coordinator handoff. It has no sender session or profile,
carries `lifecycle` (operation, task, attempt, attempt state, task state, actor
kind, the acting session handle when a user acted, and for a handoff the
successor handle and the new coordinator generation), a readable text, the
task and attempt references and
a task typed reference. Attempt transitions use recipient kind `participants`:
the attempt's worker session and the current active coordinator session, both
filtered to active and enrolled, minus the session that acted, so an actor never
receives its own transition and operator recovery reaches both parties. Handoff
uses recipient kind `team`: every active enrolled member, the outgoing session
already being closed. The audit event of the transition names the message ID.

Lifecycle messages bypass the client send quota, because a transition must not
fail for lack of inbox capacity; they count toward the retained total. They are
delivered, paged and acknowledged exactly like other messages: an inbox read
records a delivery attempt, an explicit ack records receipt, and neither is
acceptance or completion. A departed counterpart gets no delivery, but the
message remains in team history. A failed message or delivery write rolls the
whole transition back. Lifecycle messages inform; the assignment row remains
the authority on ownership, and only an accepted offer authorizes work.

Schema 16 adds message and delivery tables to schema 15. These tables are
hub-local and excluded from memory synchronization. Existing team project
drop/merge protection applies. Back up full state and the previous binary before
upgrading; rollback requires restoring both. This change does not deploy a hub.
