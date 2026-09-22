# Team session storage contract

This is the internal storage contract of the agent-team design. See
TEAM-AGENT-QUICKSTART.md for HTTP/CLI/MCP. The transport enforces
live ordinary write-token and project-grant checks before every store operation,
including retries and roster reads. Trusted local/admin credentials cannot join
through these methods. User/token IDs come from the authenticated actor, not
model-controlled profile fields. Access records live in a different database;
this increment does not pretend to provide a cross-database transaction.

`JoinTeam` receives a stable team ID, requested role, declared profile and retry
key. The transport resolves readable names. Current enrollment, coordinator
eligibility and tasks enablement are checked inside the project transaction
before reading a receipt. A distinct join key creates a distinct session, even
for the same credential. At most 100 active sessions are allowed per team in
this increment; retries of accepted joins work at the limit. The limit is
operator-configurable through JoinTeamLimit and the transport.

Only one active coordinator can exist, enforced both by the transaction and a
unique SQLite index. Its generation increases on acquisition and resume, and
survives clean leave and subsequent acquisition. Revoking enrollment or
coordinator eligibility blocks further use without freeing its slot. Operator
recovery remains a later operation; re-enrollment permits explicit reconciliation.

`ChangeTeamSession` implements profile replacement with an expected profile
revision, heartbeat with availability, resume, and clean leave. Every new effect
requires the current active session generation and the same user/token. Resume
increments the session generation and, for a coordinator, coordinator generation.
Leave closes the session and increments its generation. A worker resume also
rebinds the session's reserved attempt to the new generation in the same
transaction (see [session rebinding](TEAM-ASSIGNMENT-STORAGE.md#session-rebinding));
leave keeps the reservation on the closed handle for operator recovery. This
storage API makes no claim to stop local commands. A retry returns the original
result without repeating effects, even after the session advanced or left; it
never makes the old returned handle valid again. Current enrollment and
credential binding are still required for replay.

Profiles record readable label, platform/version, reported provider/model/version,
source, observation time and bounded capabilities. Missing model fields normalize
to `unknown`; model identity is never inferred from the client. The source and
observation time are declarations, not an attestation. A missing observation time
is filled with hub acceptance time. These declarations grant no authority.

Roster reads require a current bound session and return bounded ID-ordered pages,
including closed sessions for reconciliation. The transport constructs its
public projection without internal user/token bindings.
The `Suspect` helper takes hub time and a policy timeout (design default 120s);
suspect is a liveness observation, not a persisted ownership transition. Heartbeat
does not prove model progress. No timeout frees a coordinator slot.

Session snapshot, audit event and retry receipt commit together. Audit contains
the accepted profile and its revision, actor, generations, operation and server
context. Existing admin event reads preserve session snapshots. Rejected-command
diagnostics, HTTP/CLI/MCP parity and ordinary-token authorization tests are in
the transport increment. Messaging and assignments
remain unadvertised until their corresponding workflows exist.

Schema 15 is additive to schema 14. Back up the complete state and prior binary
before upgrade; rollback requires both. Session/control tables are hub-local and
excluded from memory synchronization. Existing team drop/merge protection and
rename identity preservation apply.
