# Coordinator handoff storage

`HandoffTeamCoordinator` transfers a team's single coordinator slot in one
transaction. The [management routes](TEAM-MANAGEMENT-HTTP.md) expose it over
HTTP for both authorities and the coordinator path over CLI and MCP; token
replacement remains a separate increment. A handoff also broadcasts a
[lifecycle message](TEAM-MESSAGE-STORAGE.md#lifecycle-messages) to every
remaining member.

## Transfer

The command names the outgoing coordinator handle and the coordinator-slot
generation as observed, a target session handle and a reason. The target must
be an active non-coordinator session of the same team at its expected
generation, its user must currently be designated coordinator in the team's
enrollment, and it must hold no reserved attempt. A session that is already the
coordinator, has left, or belongs to another team is refused; the successor
gets nothing a join as coordinator would not have granted its own user.

In one transaction the outgoing session closes as `left` with an advanced
session generation, the target's role becomes `coordinator` with the next slot
generation while its own session generation is unchanged, and one
`team.coordinator.handoff` audit event records both handles, the previous and
new coordinator generation, the reason, any reconciliation and the successor's
snapshot. The retry receipt commits with them. The outgoing principal can join
again as a worker with a fresh key; there is no in-place demotion, and no
timeout, heartbeat or reconnect performs a handoff.

Existing offers and attempts survive untouched, including their issuing
coordinator generation, which stays audit data. Old coordinator handles are
stale for every command and read; the successor issues cancellations, closures,
withdrawals and result decisions for existing attempts with the new generation.
A join as coordinator is refused while the slot is held and continues the
generation sequence after the successor leaves cleanly.

## Authority and reconciliation

The current coordinator calls with its own bound live handle and current
coordinator generation; reconciliation evidence is refused from it. A trusted
admin actor calls for a coordinator that was lost, naming the lost session's
exact handle and the current slot generation without owning them, and must
record bounded reconciliation: an explicit `coordinator_stopped` affirmation, a
liveness check and at least one evidence reference. This is a recorded operator
assessment, not proof observed by the hub; it does not stop a surviving
coordinator process, and a lost coordinator that resumed before the admin acts
makes the observed generations stale. The service must authenticate live admin
or ordinary write authority on every invocation, including receipt replay.
Project task enablement and team existence are checked inside the transaction
before receipts; a caller whose enrollment was revoked is denied.

Identical authorized retries return the original successor snapshot, including
after later transitions or a database restart. Changed input under the same key
conflicts; a rolled-back handoff does not consume its key. Concurrent handoffs,
an old-coordinator offer to the successor or the successor accepting an offer
have one winner: a reservation that lands first blocks the transfer, and a
transfer that lands first makes the old handle stale. Tests cover both
authorities, every fence, rollback at each write boundary, independent SQLite
races and reopen. No schema, dependency or public route changes; the session
row's role column now follows the snapshot on every write. Storage fixtures do
not demonstrate a real coordinator restart or authorize a release or pilot.
