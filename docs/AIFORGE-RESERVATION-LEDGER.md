# AIForge reservation ledger: internal increment C1

This increment implements only the per-project storage primitive from the [reviewed reservation contract](DESIGN-AIFORGE-RESERVATIONS.md). It registers no HTTP or MCP reservation route. The store methods take trusted attribution, validate revisions and fences, and record receipts; they do not decide who is authorized to claim, release, finalize or read a receipt. Parent task comment 115's actor-policy questions remain for C4/C5.

Schema 19 adds `task_reservations`, `task_reservation_events` and `task_reservation_requests`. Existing task, history, receipt and legacy-team tables are preserved. Each task keeps its fence row after release, so reclaim and process restart cannot reuse a fence. A reservation-only claim or transfer advances the fence without changing task content revision or history. A content update, release or finalization advances task revision/history and the fence in the same transaction. The request receipt commits in that transaction; exact retries return the recorded outcome and changed-input reuse fails. Reservation receipts are keyed to a stable user ID rather than one token ID, so rotating a token does not change the storage retry scope; current authorization on replay remains a C5 obligation.

The migration is additive and atomic with the schema-version bump. A binary built for schema 18 refuses to open schema 19 through the existing newer-schema guard. This is a compatibility boundary, not an in-place downgrade procedure. Before any eventual rollout, stop older writers and take the normal verified database backup. Rolling back after schema 19 is used requires restoring a compatible backup and reconciling work committed since that backup; changing `schema_version` or dropping reservation tables would lose fence and receipt guarantees. This PR does not deploy or run a migration on a live hub.

The focused tests prove the ledger's transactional and restart behavior. They do **not** prove protection of existing generic task update/archive paths: C2 owns that guard. C3 owns cross-project dependency consistency. C4/C5 own wire and actor policy; C6 owns the fully authorized public API and fake-consumer end-to-end faults. Until those increments merge, the ledger is internal groundwork and cannot be treated as a usable coordination API.

## C2: existing write paths

C2 adds a transaction-local active-hold check to generic task updates and the shared task/history writer, including the legacy team paths. The ledger alone may use the separate writer after it verifies the current reservation ID, fence and task revision in that transaction. A legacy team offer also checks the hold before taking management ownership. The existing HTTP task update and team offer routes return 409 on a held task without exposing the holder; no reservation route is added. A stale revision and an old generic update receipt cannot bypass the hold. Once a release commits, ordinary unreserved editing works again.

Comments remain immutable discussion under the existing write grant: they do not change task content, revision, evidence references or ownership, so a comment on a held task remains allowed. Evidence references are task content and cannot be changed through a generic update while held. This is an internal write guard and does not grant any actor permission to use the ledger; C4/C5 still define wire and actor policy, and C6 remains the only public reservation API increment.

## C3: same-hub dependency claims

`Registry.ClaimTaskReservation` is the internal entry point for a claim that must prove dependency eligibility. It requires a selected-context/current-read-grant verifier for each project access ID; a missing or failed verifier refuses the claim. C5 must supply the real verifier and the actor/role checks. No HTTP or MCP reservation route is registered by C3. The older store-only `DB.ApplyTaskReservation` remains a ledger test seam and refuses a new claim with dependencies, so it cannot bypass the cross-project check.

The registry resolves the owner and the dependency graph through the authoritative same-hub task locator, then acquires SQLite write-intent transactions for every involved project in project-ID order. Under those locks it rereads the owner's revision and dependency list, each referenced task and project enablement, and detects cycles. Only direct dependencies must be `DONE`; archived `DONE` counts. Missing, unreadable, disabled, changed or uncertain evidence refuses the claim. A verifier failure is also a refusal. The owner transaction alone writes the hold, fence, event and receipt. Dependency transactions are released only after that commit or after rollback. This is a lock-based eligibility ordering, not a multi-database atomic write. Lifecycle and database lock waits honor a five-second claim deadline; dedicated short-busy SQLite connections allow cancellation to release acquired locks promptly. Once acquired, transactions use a noncanceling lifetime: cancellation before owner commit triggers explicit rollback, while cancellation during a started commit may finish and is recovered by the receipt. This prevents `database/sql` from independently releasing a dependency lock before the owner commit finishes. The existing task locator is checked for cancellation between project resolutions.

The registry holds its project lifecycle read lock from resolution through commit. The hub's rename, drop and merge operations take the matching write lock; task-bearing projects already refuse deletion. A rename before resolution is followed by the locator and stable project access ID. A concurrent same-hub rename waits for the claim to finish. Project disable uses the database write lock and is checked again under that lock. The host CLI sends a live project's drop request to the service; its direct registry fallback is for an offline service. Running another process that mutates project directories outside the hub's lifecycle path concurrently is outside this increment's supported contract.

The claim event records the dependency IDs, project access IDs and task revisions observed under locks. `Registry.ReconcileReservationDependencies` checks that evidence later without releasing an active hold; a reopened, changed, inaccessible or unavailable dependency, or a changed held task dependency list, requires explicit reconciliation. An identical claim retry returns its committed receipt even if a dependency changed afterward, subject to current owner-context verification. A store-only or pre-C3 claim without verified proof is treated as unresolved by reconciliation.

This increment adds no schema or wire change. C1's schema-19 compatibility and rollback limits still apply. The focused tests use separate registry handles and SQLite connections for claim versus reopen ordering, cancellation and retry, project lifecycle, verifier rollback and restart recovery. A pinned-driver test pauses the owner commit and cancels the request to prove the dependency lock lasts until commit completes. C4/C5 still own actor policy, and C6 alone may expose a fully authorized public operation.

## C5a: the reservation authorizer

C5a (task 01a0e39c-8769, from C5 seq196 with operator decisions D2(a), D3(a) and D5(a)) adds the hub's reservation authorizer in `internal/server/reservations.go`. It is the ledger's only production caller, and a source walk in the tests enforces that. It registers no route or MCP tool; C6 adds them on top of it.

**Actor and binding.**
- The authorizer derives the actor from the authenticated individual credential, and in team mode from the E4-verified team context (user, token, profile, team, role, session, generation). Admin and legacy credentials have no path; recovery is C5c.
- The request input has no field that can name any of these.
- Schema 20 adds the holder's verified binding to `task_reservations`: user, mode (`personal` or `team`), profile, team, role, session and generation.
- A claim or transfer sets the binding, and a release or finalize clears it. Every other transition must come from the same holder: same user, mode, profile and role.
- A resumed session or new generation keeps the hold, and so does a rotated token, because the token is never part of the binding.
- Each receipt records the caller's binding. A replay or receipt read is served only to that same holder.

**Pre-commit recheck (D2).**
- `ApplyTaskReservation` now requires an `authorize` check, and the authorizer always supplies one.
- The authorizer checks the caller's current authority before the transaction, and again inside it immediately before commit and on replay: a live credential, an enabled user and profile, and the personal write grant or the team profile's grant on the project. The claim path's dependency verifier applies the same check.
- A team context's online answer is accepted only while it is at most five seconds old.
- A refusal inside the ledger keeps its wire code; the claim path now wraps the verifier's error rather than flattening it.
- Update, release and finalize run through `Registry.ApplyTaskReservation`, which holds the project lifecycle read lock across the transaction, as a claim does. Drop, rename and merge take that lock before the registry mutex and then wait for the project's single connection. Without it, the in-transaction recheck (which reads the registry) and a concurrent lifecycle operation could each wait on the other.

**What C5a authorizes:**
- **Personal context:** claim, update, release and finalize of `standalone` holds, under the caller's personal write grant.
- **Team context:** only the bound worker's or independent's `update` of its own hold.

  Team claim, transfer, release and finalize need verified aicrew coordination facts and belong to C5b (below).
- **Status and receipt reads:** they recheck the caller's current authority. Status shows only the caller's own hold, and any other holder's hold reads as `none`.

**Compatibility (C5a).** A hold committed before schema 20 has no binding, so no caller matches it. It stays held, with its fence, until the C5c recovery path closes it. The migration is additive, and a schema-19 binary refuses a schema-20 database through the existing newer-schema guard. The C1 rollback limits still apply.

## C5c: recovery, the recovery reader and closure evidence

C5c (task 01a0e39c-dbb9, as frozen in C5 seq205 with the seq206 decisions) adds the operator's recovery path for a hold whose holder cannot close it.

**Where.** It adds four admin-only routes under `/v1/admin/reservations/{task_id}/recovery`, all over TLS the hub terminated itself, with no MCP tool:
- `POST …/release` and `POST …/cancel`;
- `GET …` (the recovery reader's hold status);
- `GET …/receipts/{operation}/{request_key_digest}`.

The `aimem reservation recover release|cancel|status|receipt` CLI drives them with `aimem identity`'s hub trust and admin-token rules. It reads a recovery body from standard input only.

**What a recovery may do.**
- It needs an `Idempotency-Key`, a reason and exactly one evidence.
  - An **operator attestation** is an attestation ID plus a statement of 16 to 2048 characters. Decision D-c2a allows it even while aicrew is reachable, and it is the only evidence for a hold from before bindings.
  - **aicrew stop evidence** is a `coordination.v1` `stopped` proof. It is verified online with the coordination client that C5c adds beside the introspection client, and it must match this hold's attempt reference, task and request key, with the holder recorded on the hold as its member ([coordination contract](DESIGN-AIFORGE-COORDINATION-WIRE.md)).
- A recovery **releases** the task (to READY or BLOCKED) or **cancels** it (CANCELLED). The store and the route both refuse DONE.
- The reservation ID, fence and expected revision must match, under the project lifecycle lock. A holder's transition racing a recovery commits exactly one of the two.

**Attribution and audit.**
- The recovery runs as a new `TaskActor` kind `recovery`, with the admin credential's name. Its receipts are scoped to `recovery/<name>`, and a recovery binding can never use the member path.
- The receipt and the event record the **affected actor**: the hold's stored binding, which never comes from input. The event log also keeps the attestation statement. The receipt and the response carry only the evidence kind and reference: an attestation ID, or the stop proof's `p1_` digest, never the proof.
- The access audit records `reservation.recovery.release|cancel|read`, `reservation.recovery.replay.release|cancel` and `reservation.recovery.refused.<code>`, with the task, the reservation, the fence, the affected user and mode, and the evidence kind and reference. It never records a proof or a statement.
- Every recovery request that passes the admin gate leaves exactly one of these records. That covers a mutation, a replay served from its receipt (for either evidence kind), a reader read, and every refusal, including the reader's.
- Inside the transaction, the hub rechecks that the admin is still registered. The host's env admin is valid for the process lifetime. It also rechecks that a stop fact is at most 5 s old.

**Closure evidence (C5c-w).**
- Schema 21 records the service of a team binding (`bound_service`). It also adds `task_reservation_services`, which keeps, per task and service, the last reservation that service's verified team context established, and how it closed: `holder_release`, `holder_finalize`, `recovery_release` or `recovery_cancel`, with the closing fence, time and revision.
- `DB.ServiceHoldStatus` answers `held`, `closed` or `none` for one service, as the coordination contract's Closure evidence section freezes it. That includes `closed` after someone else took the task, and it never describes another holder.
- C6 wires it to aicrew's read-scope route.

**The ledger's callers.** The member authorizer (`reservations.go`) and the recovery routes (`recovery.go`) are its only production callers, and the source walk in the tests enforces that.

## C5b: coordination-backed transitions

C5b (task 01a0e39c-db9a, criteria in its seq213 and seq215) authorizes every team transition other than a holder's `update`. Each is backed by one `coordination.v1` fact ([coordination contract](DESIGN-AIFORGE-COORDINATION-WIRE.md)), including the C5-w2 process pin.

**The actor rules**, per operation and verified role:

| Operation | Role | Fact |
| --- | --- | --- |
| claim | coordinator | `offer` |
| claim | independent | `independent_claim` |
| transfer | worker | `accepted_attempt` |
| release | coordinator | `never_accepted` |
| release | worker or independent | `stopped` |
| finalize | coordinator, worker or independent | `accepted_for_finalization` |

- Any other combination is `role_forbidden`.
- A permitted team transition without its `coordination_proof` is `invalid_request`, as is a personal transition or an `update` that carries one.

**Verifying the fact.** The hub asks aicrew once, before the ledger transaction (D2a), through the operational identity peer, which must be the caller's team profile service. It binds the answer to this request:
- the kind and operation;
- the task;
- the `k1_` digest of this request's key;
- the member, which must equal the caller's verified context: user, agent, team, role, session and generation;
- the request's holder: `external`, on the offer's or attempt's reference.

A fact aicrew does not vouch for, or that fails any of these checks, is `coordination_rejected`. An unreachable or wrong-shaped answer, including a missing or malformed pin, is `context_unavailable`. The answer commits only while it is at most 5 s old.

**The ledger checks, inside the committing transaction:**
- The hold's current work reference is the one the fact names. Otherwise `coordination_rejected`.
- A transfer goes only to the worker the offer named, same user and agent, of the hold's service and team.
- The never-accepted release and the coordinator's finalize take a narrow non-holder path: a coordinator of the hold's service and team. Every other transition needs the bound holder.
- **Last, the process pin**, on an `offer`, `accepted_attempt` or `independent_claim`. It is compared byte for byte with the project's current selection. A mismatch, or no selection, is `process_mismatch` (409).

Schema 22 records the offer's intended worker on the hold and clears it on transfer or close.

**Replay.**
- A committed transition is answered from its receipt before any coordination call (the replay rule), after the hub rechecks only aimem-owned authority and the acting member.
- A coordinator's finalize replays only for that coordinator's recorded session.
- The receipt and event record the fact kind and the proof's `p1_` digest, never the proof.

**`DONE` needs terminal evidence.** Finalizing a reservation hold to `DONE` requires `terminal_evidence` (reviewed delivery and human merge, per the reservation wire), in team **and** personal mode. This applies only to a reservation finalize. An ordinary task edit to `DONE` by a user who holds no reservation is unchanged, and a regression test keeps it so.

**Recovery (seq209).** A recovery on stop evidence now passes the hold it verified (its work reference) to the ledger, which compares it inside the transaction. A transfer that lands during verification makes the recovery `stale_fence` instead of closing the new attempt.

## C6a: the member routes and MCP tools

C6a (task 01a0e8c7-e1af, from C6 seq219 with the seq221 decisions) serves the reservation wire's member surface over the C5 authorizer: seven HTTP routes under `/v1/projects/{p}/tasks/{task_id}/reservation` and the seven `task_reservation_*` MCP tools, in personal and team mode ([reservation wire](DESIGN-AIFORGE-RESERVATION-WIRE.md#what-c6a-serves)).

- **Store.** `TaskReservationInput.Intent` is an optional `block`, `submit` or `resume` on `update` only, recorded in the event (D6-2a). `TaskReservationReceiptByKey` reconciles by operation and key alone, for the acting member only. `ReservationReceiptID` gives a receipt a stable ID. A claim's replay now reports `replayed`, as every other replay does.
- **Retention (D6-5a).** No request key is pruned in v1.
- **Surface.** The routes are task routes (ordinary tokens and the in-process MCP dispatcher reach them) and team routes (the first team-mode writes). The live OpenAPI documents them with operation-specific request schemas and the read refusals.

## C6c: the member reservation CLI

C6c (task 01a0e8c7-e1e9) serves the [coordination wire's reservation CLI](DESIGN-AIFORGE-COORDINATION-WIRE.md#4-reservation-cli-for-c6): `aimem reservation claim|transfer|update|release|finalize`, `receipt OPERATION` and `status`, each with `--task`, and `--key` on every command except `status`.

- **One engine.** A command runs its `task_reservation_*` tool exactly as the stdio facade would (`internal/mcp/reservation_cli.go`). It first reads the task, over the same connection, for the project the route path carries.
- **Context.** With `AIMEM_TEAM_SESSION`, the command is that team conversation. It uses the pinned binding and context header and verifies the context online first. A session that cannot be used blocks with `context_missing` and sends nothing. There is no personal caller to fall back to. Without the variable, the checkout's personal credential is used.
- **Input.** A mutation's body is one JSON object on standard input, never argv, bounded to 256 KiB. The body is refused if it contains trailing data or a field the arguments set (`version`, `project`, `task_id`, `request_key`, `operation`).
- **Output and exit.** The command prints one JSON document to standard output: the outcome or the typed refusal envelope. The document never contains the proof. Exit codes:
  - 0: committed, or a replay;
  - 3: final refusal;
  - 4: retryable refusal;
  - 5: a mutation was sent and no answer came (`receipt_unresolved`); reconcile with `receipt` and the same key;
  - 2: usage error, reported on standard error.

## C6b: aicrew's read scope

C6b (task 01a0e8c7-e212) serves aicrew's read-only reservation scope from the [coordination wire](DESIGN-AIFORGE-COORDINATION-WIRE.md#2-aicrews-read-scope-aicrew--aimem) §2 (D5b). Aicrew's reconciliation (its task b3) reads it.

- **Credential (D6-3a).** Access schema 5 adds an `operation` to peer credentials: `identity.redeem` or `reservation.read`. Credentials that already exist keep `identity.redeem`. The admin issue route and `aimem identity cred issue|rotate --operation` choose the operation.
  - At most two credentials are active per peer and operation.
  - Issuing is audited as `identity_peer.credential.issue.<operation>`.
  - A `reservation.read` credential never redeems: the gate refuses it on the redemption route with `peer_forbidden`, and so does the store if it gets that far. A redemption credential cannot read.
- **Recording (D6-4).** Schema 23 adds four columns to every receipt:
  - `reservation_id`: the reservation the transition acted on (for a claim, the new one);
  - `committed_at`;
  - `service_id` and the proof's `p1_` digest, written only for a transition made on a verified coordination fact. The service is always the caller's team service, which answered the fact.

  A receipt from before schema 23 reads as none.
- **Routes.** Three GETs under `/v1/identity/peers/{service_id}/`: `reservation-receipts/{proof_digest}`, `reservations/{task_id}/receipts/{operation}/{request_key_digest}` and `reservations/{task_id}`.
  - The bearer gate classifies them as identity wire routes and confines each peer credential to its own operation's routes. `/mcp` and every other route refuse it.
  - A route answers only over hub-terminated TLS, with `X-Aimem-Reservation-Version: 1`, for the path's own service. Reads are limited to 60 per credential per minute.
  - By key, a transition is visible when a claim or transfer under the service's proof established its reservation.
  - Hold status is C5c's `ServiceHoldStatus`.
- **Answers that are never guessed.** Every out-of-scope, missing or malformed record answers `none`. A lookup that cannot vouch for `none` answers the retryable `request_in_progress` instead: an unreadable project during the proof search, or a busy store.
