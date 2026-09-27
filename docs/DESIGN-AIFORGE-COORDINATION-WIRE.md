# AIForge coordination facts and reservation read scope, version 1

Status: reviewed contract candidate (task C5w, 01a0e39c-8786). **Nothing here is implemented.** C5b implements aimem's use of `coordination.v1`, C6 serves the read scope and the reservation CLI, and aicrew's task b0 serves `coordination.v1`. The fixtures live in `docs/fixtures/coordination-v1/`. The live `internal/server/openapi.json` describes only served routes.

This contract completes the [reservation wire contract](DESIGN-AIFORGE-RESERVATION-WIRE.md) (C4) for team transitions. It reuses the shapes and rules of [identity.v1](DESIGN-AIFORGE-IDENTITY-WIRE.md) and composes with aicrew's [crew contract](https://github.com/BlackVS/aicrew/blob/2063838/docs/CREW-CONTRACT.md) ("Attempts and the aimem reservation"). If a parent contract disagrees with this document, the parent wins. Updated 2026-09-27.

## Approved operator decisions

The operator decided these on task C5, comment seq201, after aicrew's impact analysis (aicrew task b, seq135, relayed in C5 seq200).

1. **D4 (a): only the acting member mutates.** Every reservation mutation arrives only over the acting member's own verified connection: its individual aimem credential, plus its team-context handle in team mode. Aicrew's server holds no credential that can change a reservation. *Operator's reason: acting on someone else's behalf is a bad idea.*
2. **D5b: aicrew gets a read-only scope, as a prerequisite.** Aicrew can read hold status and receipts, but only for reservations whose `coordination_proof` this aicrew service issued. The scope carries no mutation, shows no other holder, and returns no task content beyond the hold, the receipt and the fence. It is a separate peer credential with one permitted operation, stored as a digest. *Operator's reason: trusting a client's word is just as bad.*
3. **D1 (a): aimem verifies aicrew's facts online.** It does so through `coordination.v1`, which is shaped like identity.v1 introspection: a pinned peer, a nonce echo, a 2 s bound, no caching, and inactive replies that give no reason.
4. **Replay rule.** When aimem replays a committed receipt, it rechecks only the authority it owns (token, grant, profile). It does not query the coordination fact again: the fact was verified when the change was made, and a replay has no new effect.

## How a team step runs

Aicrew's store keeps its state machine; each reservation step becomes three calls across two processes.

1. **Begin (member client → aicrew).** The member's aicrew client (`aicrew-agent`), authorized by its aicrew session token, asks aicrew to start the step. Aicrew does three things:
   - it commits the intent and the capacity;
   - it chooses the aimem request key;
   - it mints a **proof reference** bound to that one pending intent.

   It returns the key, the proof reference, the expected revision, the fence and the complete task content to send.
2. **Mutate (member client → aimem).** The client sends the reservation mutation over its own aimem connection. Before committing, aimem verifies the proof through `coordination.v1` (§1).
3. **Settle (member client → aicrew, confirmed by aicrew).** The client reports the outcome to aicrew. Aicrew treats that report as a hint and confirms it through its read scope (§2), with the same key digest and proof.

Aicrew never calls a reservation mutation, and aimem never takes a coordination fact from the member.

## 1. Coordination facts: `coordination.v1` (aimem → aicrew)

### Proof reference

| Property | Value |
| --- | --- |
| Format | `acp1_` followed by 43 base64url characters (256 random bits) |
| Issued by | aicrew, at a step's begin |
| Stored by | aicrew as a SHA-256 digest; aimem as the `p1_` digest below, on the receipt |
| Bound to | exactly one pending intent. That intent fixes: the operation and the fact kind; the team, the acting member's agent, session and generation; the task; the offer or attempt it concerns; and the `k1_` digest of the member's aimem request key |
| Lifetime | at most 15 min, and never longer than the intent stays pending. Once aicrew settles or voids the intent, the proof answers inactive |

**Where the proof may appear.** A proof is a single-use bearer of one step. It may appear only:
- in the `coordination_proof` field of that step's reservation mutation;
- in the `proof` field of a `coordination.v1` request;
- on the standard input of the reservation CLI (§4).

It never appears in argv, a response, a receipt, an audit record, a log or a refusal. aimem and aicrew identify it only by its digest: `p1_` followed by the unpadded base64url SHA-256 of the proof's bytes (46 characters). The `k1_` request-key digest uses the identity.v1 encoding: `k1_` followed by the unpadded base64url SHA-256 of the key's UTF-8 bytes.

### Request

`POST /v1/crew/coordination` on the peer. Aicrew owns this route. Its endpoint has the same scheme, host and port as the registered introspection endpoint, with the path `/v1/crew/coordination`, and uses the same TLS trust binding.
- **Credential.** aimem authenticates with its outbound introspection credential, the same file (`AIMEM_INTROSPECTION_TOKEN_FILE`) and the same rotation. Aicrew records the operations each of its issued credentials permits. A credential used here must permit `crew.coordination`, and one that doesn't is refused like an unknown credential.
- **Version.** It travels in the header `X-Aimem-Coordination-Version: 1` and the body field `version: 1`. Both are required and must be equal. Otherwise aicrew answers `400 unsupported_version`, evaluates nothing, and never reports a proof as active.
- **Body.** `{version: 1, hub_id, nonce, proof}`. The nonce is `n-` followed by 128 random bits in lowercase hex, fresh for each call.

aimem sends no expected member, task, operation or key. Aicrew answers only from its own current state, so it cannot echo IDs that aimem supplied.

### Reply

**Active.** An active proof whose intent is still pending gets `200` with:

```
{nonce, active: true, service_id, hub_id,
 fact: {kind, operation, task_id, request_key_digest,
        member: {user_id, agent_id, team_id, role, session_id, generation},
        offer_ref?, attempt_ref?, intended_worker?: {user_id, agent_id},
        expires_at}}
```

**Inactive.** Every other state gets `200` with only `{nonce, active: false}`. That covers:
- an unknown, expired or already settled proof;
- a voided intent, or a withdrawn offer;
- a fenced or stale generation, or a changed role;
- a proof bound to another hub.

The inactive reply carries no reason, so a probe learns nothing.

Aicrew answers every fact from one snapshot of current state, never from the proof table alone. A fact that has stopped being true (an offer withdrawn, a generation fenced, an acceptance voided by a stop request) is inactive.

### Fact kinds

| `kind` | `operation` | Acting member (`member.role`) | Also carries | aimem also requires |
| --- | --- | --- | --- | --- |
| `offer` | `claim` | the team's current coordinator (`coordinator`) | `offer_ref`, `intended_worker` | request `holder = {mode: external, work_ref: offer_ref}`; aimem records `intended_worker` on the hold |
| `accepted_attempt` | `transfer` | the intended worker (`worker`), from the session the offer is bound to | `offer_ref`, `attempt_ref` | the hold's current `work_ref` equals `offer_ref`; request `holder.work_ref` equals `attempt_ref`; `member` is the `intended_worker` aimem recorded from the offer fact |
| `never_accepted` | `release` | the current coordinator, or a verified successor (`coordinator`) | `offer_ref` | the hold's current `work_ref` equals `offer_ref` |
| `stopped` | `release` | the holding worker (`worker` or `independent`), after it confirmed the stop | `attempt_ref` | the hold's current `work_ref` equals `attempt_ref`, and the caller is the bound holder |
| `accepted_for_finalization` | `finalize` | the holder, or the coordinator from the session and generation that recorded the acceptance | `attempt_ref` | the hold's current `work_ref` equals `attempt_ref`; `terminal_evidence` present for `DONE` |
| `independent_claim` | `claim` | the claimer (`independent`) | `attempt_ref` | request `holder = {mode: external, work_ref: attempt_ref}` |

A holder's `update` (block, submit, resume) needs no fact. C5a authorizes it from the stored binding alone.

### Acceptance by aimem

aimem makes one attempt of at most 2 s (connect, TLS and response), with no redirect, no proxy, no retry and no cache. Caller cancellation propagates. It reads at most 16,384 bytes of the reply, and refuses unknown fields and trailing data. It accepts an active reply only if all of these hold:

- the TLS identity matches the registration, and the nonce matches;
- `service_id` is the registered peer, and `hub_id` is this hub;
- the verified team context of the caller is linked to that service's team profile;
- `fact.expires_at` is in the future by the hub clock;
- `fact.operation` is the operation being authorized, and `fact.kind` is one the table allows for it;
- `fact.task_id` is the request's task;
- `fact.request_key_digest` is the `k1_` digest of this request's `Idempotency-Key`. A proof reused under another key, or for another mutation, never matches;
- `fact.member` equals the caller's verified team context exactly: user, agent, team, role, session and generation;
- the references match the request and the current hold, as the table requires.

**Outcomes.**
- **Unavailable: about the peer.** Any of these gives `context_unavailable` (retryable):
  - a failure to reach, authenticate or parse the reply;
  - a TLS identity, nonce, `service_id` or `hub_id` that does not match;
  - a reply of the wrong shape.

  The same key may be retried while the proof lives, and nothing is applied.
- **Rejected: about the fact.** An inactive reply gives `coordination_rejected`, and so does an active one whose fact fails any other check: expiry, operation, kind, task, key digest, member or references. That refusal is final for this key and this proof: the member begins the step again through aicrew.

The answer is used once. aimem takes it before the ledger transaction (C5 decision D2a), and the transaction commits only while the answer is at most 5 s old. The in-transaction recheck is aimem-owned and never queries aicrew.

### Replay

**A replay does not query the fact again.** An identical request with a committed receipt is answered from the receipt after aimem rechecks only the authority it owns: the live token, the grant, the profile, and that the replaying caller is the receipt's holder binding (C5a). It never calls `coordination.v1`, so a settled or expired proof never blocks reconciliation.

A retry whose first attempt did **not** commit is not a replay: it is a new attempt, and it verifies the fact again.

## 2. Aicrew's read scope (aicrew → aimem)

### Credential

A separate aimem peer credential, `aimem_peer_` followed by 256 random bits in hex, issued by the hub admin for the registered aicrew peer with the single operation `reservation.read`. Its lifecycle is identity.v1's: returned once, stored as a SHA-256 digest, at most 366 days, at most two active per peer and operation so a rotation can overlap, revocable, audited, and never logged. It cannot redeem proofs. The redemption credential (`identity.redeem`) cannot read reservations, and neither credential can mutate anything. Disabling the peer refuses both.

aimem records, on every receipt and hold committed under a verified fact, the `service_id` that answered the fact and the proof's `p1_` digest. The read scope sees exactly those records.

### Operations

Both are HTTP-only, over TLS the hub terminated itself, with `X-Aimem-Reservation-Version: 1`. The path `service_id` must be the authenticated peer. The hub's bearer gate confines a `reservation.read` credential to exactly these two route shapes, as it confines the redemption credential to its route, and refuses it everywhere else, including `/mcp`, before any handler runs.

| Operation | Route | Answer |
| --- | --- | --- |
| Receipt by proof | `GET /v1/identity/peers/{service_id}/reservation-receipts/{proof_digest}` | `{state: "committed", receipt}` for the transition committed under that proof, or `{state: "none"}` |
| Hold status | `GET /v1/identity/peers/{service_id}/reservations/{task_id}` | `{state: "held", reservation_id, fence, holder_mode: "external", work_ref, task_revision}` when the task's current hold was set under a proof this service issued, otherwise `{state: "none"}` |

The receipt is `{id, operation, task_id, request_key_digest, reservation_id, fence, task_revision, member_user_id, verified_mode: "team", committed_at}`.

**What the scope never returns:** task content, another holder's identity or reference, a hold set under another service's proof or under personal mode, the raw request key, or the proof itself. Every out-of-scope record answers `none`, the same as a missing one.

**When `none` is final.** A committed transition's receipt is durable. But `none` means only that nothing has committed under that proof *yet*: a member's request verified just before may still commit. Aicrew treats `none` as final only after both of these:
- it has settled or voided the intent, so `coordination.v1` answers inactive;
- at least 10 s have passed since then, which covers aimem's 5 s answer age plus the 2 s budget and a margin.

Before that, a `none` is a reason to wait, not an outcome.

**Refusals.**

| Status | Code | Cause |
| --- | --- | --- |
| 400 | `unsupported_version` | missing or unsupported version |
| 401 | `peer_unauthenticated` | missing, unknown, revoked or expired credential |
| 403 | `peer_forbidden` | another operation's credential, or another service's path |
| 403 | `tls_required` | no hub-terminated TLS |
| 503 | `request_in_progress` | the store is busy (retryable) |

Reads are rate-limited to 60 per credential per minute (`rate_limited`, 429, retryable).

## 3. Refusal added to reservation.v1

| Code | Status | Retryable | Next action |
| --- | --- | --- | --- |
| `coordination_rejected` | 403 | no | Begin the step again through aicrew; never reuse the proof or the key. |

An unreachable or unparsable `coordination.v1` answer is the existing `context_unavailable` (503, retryable).

## 4. Reservation CLI (for C6)

C6 must offer the reservation mutations and the receipt read as `aimem` CLI commands as well as MCP tools, so aicrew's client can drive a step deterministically without a model:

```
aimem reservation claim|transfer|update|release|finalize --task TASK_ID --key REQUEST_KEY
aimem reservation receipt OPERATION --task TASK_ID --key REQUEST_KEY
aimem reservation status --task TASK_ID
```

- **Input.** The mutation body (expected revision, reservation ID and fence, holder, complete content, reason, terminal evidence and `coordination_proof`) is read as JSON from **standard input only**, never argv, as E5a reads the session handle. The request key is not a secret and may be an argument.
- **Context.** The command runs under the process's bound team session (`AIMEM_TEAM_SESSION`, E5a), with the same pinned binding, header and online verification as `aimem mcp`. Without the variable it runs in personal mode. It never falls back from team to personal mode.
- **Output.** The committed outcome or the refusal envelope goes to standard output as one JSON document, and never includes the proof.
- **Exit status.**

  | Exit | Meaning |
  | --- | --- |
  | 0 | committed, or a replayed committed outcome |
  | 3 | final refusal |
  | 4 | retryable refusal |
  | 5 | outcome unknown: a transport failure after the request was sent. Reconcile with `receipt` and the same key; never use a fresh key |
  | 2 | usage |

## Bounds

These values are fixed by v1. An implementation may tighten them; relaxing any of them needs a reviewed v2.

- A proof lives at most 15 min, and only while its intent is pending.
- A coordination call gets one attempt of at most 2 s, and its reply may be at most 16,384 bytes.
- A coordination answer is at most 5 s old when its transition commits.
- A read-scope `none` is final only 10 s after aicrew voided or settled the proof.
- Read-scope reads are limited to 60 per credential per minute.
- A `reservation.read` credential lives at most 366 days, with at most two active per peer.

## Delivery

- **C5w (this task)** freezes the contract, the fixtures and a fake-consumer test (`internal/server/coordination_wire_contract_test.go`). The test checks:
  - fact coverage and the mapping from facts to operations;
  - the request and reply shapes, and the nonce echo;
  - the version and size cases;
  - the `k1_` and `p1_` digests;
  - where secrets may appear;
  - that the hub serves neither `/v1/crew/coordination` nor any read-scope route yet.
- **C5b** implements aimem's `coordination.v1` client and the coordination-backed transitions against a fake.
- **C6** serves the read scope, the reservation routes, the MCP tools and the CLI.
- **Aicrew b0** serves `coordination.v1`, and b3 reconciles through the read scope.
