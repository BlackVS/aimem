# AIForge coordination facts and reservation read scope, version 1

Status: reviewed contract (task C5w, 01a0e39c-8786, merged in #136), amended before any implementation by task C5c-w (01a0e62c-9d60) with the `closed` hold-status answer ([Closure evidence](#closure-evidence)), by task C5-w2 (01a0e6a6-bccc) with the process pin ([Process pin](#process-pin)), and by task C5-w3 (01a0eb83-bde1) with the evidence digest ([Evidence digest](#evidence-digest)). **C5-w3 is the last in-place amendment before the pilot.** After the pilot, a wire change goes through a new version or an optional field, never an in-place amendment. **Implementation status.** aimem uses `coordination.v1` since task C5b. It serves the reservation CLI (§4) since task C6c and the read scope (§2) since task C6b. Aicrew's task b0 serves `coordination.v1`. The fixtures live in `docs/fixtures/coordination-v1/`. The live `internal/server/openapi.json` describes only served routes.

This contract completes the [reservation wire contract](DESIGN-AIFORGE-RESERVATION-WIRE.md) (C4) for team transitions. It reuses the shapes and rules of [identity.v1](DESIGN-AIFORGE-IDENTITY-WIRE.md) and composes with aicrew's [crew contract](https://github.com/BlackVS/aicrew/blob/2063838/docs/CREW-CONTRACT.md) ("Attempts and the aimem reservation"). If a parent contract disagrees with this document, the parent wins. Updated 2026-09-28.

## Approved operator decisions

The operator decided these on task C5, comment seq201, after aicrew's impact analysis (aicrew task b, seq135, relayed in C5 seq200).

1. **D4 (a): only the acting member mutates.** Every reservation mutation arrives only over the acting member's own verified connection: its individual aimem credential, plus its team-context handle in team mode. Aicrew's server holds no credential that can change a reservation. *Operator's reason: acting on someone else's behalf is a bad idea.*
2. **D5b: aicrew gets a read-only scope, as a prerequisite.** Aicrew can read hold status and receipts, but only for reservations whose `coordination_proof` this aicrew service issued. The scope carries no mutation, shows no other holder, and returns no task content beyond the hold, the receipt and the fence. It is a separate peer credential with one permitted operation, stored as a digest. *Operator's reason: trusting a client's word is just as bad.*
3. **D1 (a): aimem verifies aicrew's facts online.** It does so through `coordination.v1`, which is shaped like identity.v1 introspection: a pinned peer, a nonce echo, a 2 s bound, no caching, and inactive replies that give no reason.
4. **Replay rule.** When aimem replays a committed receipt, it rechecks only the authority it owns (token, grant, profile). It does not query the coordination fact again: the fact was verified when the change was made, and a replay has no new effect.
5. **Process pin, option (a): aimem checks the reference.** Aicrew's integration assessment (aicrew task b, seq165) found that an offer's process version must come from an authoritative aimem source, while under D4(a) and D5b aicrew can read only holds and receipts. The operator decided on 2026-09-28 (task C5-w2) that aimem verifies the pin. The `offer`, `accepted_attempt` and `independent_claim` facts carry the process reference aicrew recorded, and aimem checks the repository, commit and manifest against the project's current selection before it commits the claim or transfer. It checks no digest. *Operator's reason: the commit hash already pins the content, and it matches how aimem stores the project's selection.* After aicrew's consumer review the operator extended the pin to `independent_claim`: the pin comes from an authoritative aimem check, never from a client, for every claim.
6. **Evidence digest (task C5-w3, decisions D1 to D4 on its scope).** A team member confirms the delivery (the coordinator, through aicrew's confirm-delivery step), and neither aimem nor aicrew queries a forge. The `accepted_for_finalization` fact therefore binds the `e1_` digest of exactly the delivery evidence the coordinator confirmed, and aimem refuses a finalize whose `terminal_evidence` is anything else with `evidence_mismatch` ([Evidence digest](#evidence-digest)). The digest covers the references only; binding their kinds is a later wire change.

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
- in aicrew's authenticated **begin response**, which delivers the newly minted proof to the member's client that asked for the step, and to no one else. This is aicrew's session API, authorized by that member's aicrew session token;
- in the `coordination_proof` field of that step's reservation mutation;
- in the `proof` field of a `coordination.v1` request;
- on the standard input of the reservation CLI (§4).

Apart from the begin response, it never appears in argv, a response, a receipt, an audit record, a log or a refusal. In particular, a `coordination.v1` reply, a read-scope receipt and a refusal never echo it. aimem and aicrew identify it only by its digest: `p1_` followed by the unpadded base64url SHA-256 of the proof's bytes (46 characters). The `k1_` request-key digest uses the identity.v1 encoding: `k1_` followed by the unpadded base64url SHA-256 of the key's UTF-8 bytes.

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
        process?: {repo, commit, manifest},
        evidence_digest?: "e1_…",
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
| `offer` | `claim` | the team's current coordinator (`coordinator`) | `offer_ref`, `intended_worker`, `process` | request `holder = {mode: external, work_ref: offer_ref}`; aimem records `intended_worker` on the hold; `process` is the project's current selection ([Process pin](#process-pin)) |
| `accepted_attempt` | `transfer` | the intended worker (`worker`), from the session the offer is bound to | `offer_ref`, `attempt_ref`, `process` | the hold's current `work_ref` equals `offer_ref`; request `holder.work_ref` equals `attempt_ref`; `member` is the `intended_worker` aimem recorded from the offer fact: the same `user_id` **and** `agent_id`; `process` is the project's current selection ([Process pin](#process-pin)) |
| `never_accepted` | `release` | the current coordinator, or a verified successor (`coordinator`) | `offer_ref` | the hold's current `work_ref` equals `offer_ref` |
| `stopped` | `release` | the holding worker (`worker` or `independent`), after it confirmed the stop | `attempt_ref` | the hold's current `work_ref` equals `attempt_ref`, and the caller is the bound holder |
| `accepted_for_finalization` | `finalize` | the holder, or the coordinator from the session and generation that recorded the acceptance | `attempt_ref`, `evidence_digest` | the hold's current `work_ref` equals `attempt_ref`; `terminal_evidence` present for `DONE`, and exactly the evidence `evidence_digest` binds ([Evidence digest](#evidence-digest)) |
| `independent_claim` | `claim` | the claimer (`independent`) | `attempt_ref`, `process` | request `holder = {mode: external, work_ref: attempt_ref}`; `process` is the project's current selection ([Process pin](#process-pin)) |

A holder's `update` (block, submit, resume) needs no fact. C5a authorizes it from the stored binding alone.

**A `stopped` fact behind an operator recovery (C5c).** An operator's recovery may present a `stopped` proof as its stop evidence (C5 decision D3a). aicrew mints that proof for the **recovery request**. aimem then applies the rules above with two differences:
- `request_key_digest` is the `k1_` digest of the recovery request's `Idempotency-Key`;
- `member` must be the holder recorded on the hold (the same user, team and role), not the admin who sends the request. That holder's session and generation may have moved on since the claim.

aimem accepts the fact for a recovery release and for a recovery cancel. It refuses stop evidence for a hold that is not a team hold, including a hold from before bindings, which only an operator attestation can recover.

### Process pin

An `offer`, an `accepted_attempt` and an `independent_claim` fact carry `process`, the version of the project's process that aicrew recorded for that step:
- for an offer, the version the coordinator offered the work under;
- for an acceptance, the version the worker accepted it under. The worker's client keeps the same version locally for its accepted attempt;
- for an independent claim, the version the independent worker claims the work under.

Aicrew takes the pin from its own record of the step, never from a read of aimem at that moment. aimem is the authority on whether it is still the project's selection (decision 5).

| Field | Form: the same as the hub's process selection (`GET /v1/projects/{project_id}/process`, `current`) |
| --- | --- |
| `repo` | the Git URL of the process repository: at most 512 bytes, starting with `https://`, `ssh://` or `git@`, with no whitespace or quote character and no leading `-` |
| `commit` | the full commit ID: 40 lowercase hex characters. **The commit is the version.** It pins the content, so the pin carries no content digest and aimem checks none |
| `manifest` | the manifest path at that commit: a clean relative slash path of at most 256 bytes that stays inside the repository |

- **Exactly these three fields.** `process` carries no branch or tag (`ref`), no selection time or selector, and no digest.
- **Required where it applies.** `process` is required on `offer`, `accepted_attempt` and `independent_claim`, the facts that start work, and absent from every other kind. Closing work (`never_accepted`, `stopped`, `accepted_for_finalization`) and a holder's `update` never depend on the selection.
- **Shape.** A missing, malformed or misplaced `process`, or one with any other field, makes the reply the wrong shape: `context_unavailable`, like every other shape failure ([Acceptance by aimem](#acceptance-by-aimem)).

**The check.** Before it commits a claim on an `offer` or `independent_claim` fact, or a transfer on an `accepted_attempt` fact, aimem compares `process` with the project's current selection. It reads the selection **inside the ledger transaction that commits the transition**.
- `repo`, `commit` and `manifest` must each equal the selection's field, byte for byte. There is no normalization: a trailing `.git`, another letter case or another URL form of the same repository is a mismatch.
- The selection's `ref`, `selected_at` and `selected_by` play no part.
- A project with no selected process has nothing to match.

A mismatch, or no selection, refuses the transition with **`process_mismatch`** (409, final for this key and this proof) and applies nothing. The comparison runs only after every fact check has passed, so a fact that fails any check is `coordination_rejected`, whatever its pin.

Because aimem reads the selection in the committing transaction, the timing is exact:
- a selection change after aimem accepted the fact but before commit refuses the transition;
- a change after commit does not undo it.

**A transfer is checked against the current selection, not against the offer's pin.** If the selection changes between an offer and its acceptance, the transfer is refused. The offer's hold stays with the coordinator, who closes it with `never_accepted` and offers again under the current version.

**Replay.** A replay of a committed claim or transfer is answered from its receipt and does not compare the pin again (the replay rule), even after the selection has moved on. A retry that did not commit is a new attempt and compares it again.

**Disclosure.** The refusal is the standard reservation envelope. It names neither the current selection nor the pin the fact carried. The member reads the current selection the way it already reads its process context.

### Evidence digest

An `accepted_for_finalization` fact carries `evidence_digest`: the digest of the delivery evidence the coordinator confirmed through aicrew's confirm-delivery step. No other kind carries it. Aicrew computes it over the evidence the finalize will send; aimem recomputes it over the request.

- **What it covers.** The finalize request's `terminal_evidence` references, in their exact order, and nothing else: no evidence kind, no other field.
- **Canonical form.** For each reference in order: the 4-byte big-endian length of its UTF-8 bytes, then those bytes. SHA-256 over the concatenation, written `e1_` followed by the unpadded base64url digest (46 characters).
- **No normalization.** No trimming, case or Unicode folding, sorting or deduplication. Reordering, altering, dropping or adding a reference changes the digest. The length prefix keeps `["ab", "c"]` and `["a", "bc"]` apart, and a reference that contains a newline apart from two references.
- **Shape.** A missing, malformed or misplaced `evidence_digest` makes the reply the wrong shape: `context_unavailable`, as for the process pin.
- **Vectors.** The fixture's `evidence_digest.vectors` are the canonical encodings both sides test against.

**The check.** Before it commits a finalize on an `accepted_for_finalization` fact, aimem recomputes the digest over the request's `terminal_evidence` **inside the ledger transaction that commits it**, right after the process-pin check. Any difference refuses the finalize with **`evidence_mismatch`** (409, final for this key and this proof) and applies nothing. The check applies to every finalize on that fact, whatever its target state. The committed receipt records the digest with the fact's kind and proof digest.

**Replay.** A replay of a committed finalize is answered from its receipt (the replay rule). The receipt's request digest covers `terminal_evidence`, so other evidence under the same key is `idempotency_conflict`.

**A personal finalize is unchanged.** It carries no proof and no fact, so there is no digest to match. Its `DONE` still requires terminal evidence.

**Disclosure.** The refusal is the standard reservation envelope. It names neither digest.

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
- the references match the request and the current hold, as the table requires;
- on `offer`, `accepted_attempt` and `independent_claim`, `fact.process` is well formed, and absent on every other kind ([Process pin](#process-pin)). Its comparison with the project's selection comes last, in the committing transaction.
- on `accepted_for_finalization`, `fact.evidence_digest` is a well-formed `e1_` digest, and absent on every other kind ([Evidence digest](#evidence-digest)). It is compared with the request's `terminal_evidence` in the committing transaction, after the pin.

**Outcomes.**
- **Unavailable: about the peer.** Any of these gives `context_unavailable` (retryable):
  - a failure to reach, authenticate or parse the reply;
  - a TLS identity, nonce, `service_id` or `hub_id` that does not match;
  - a reply of the wrong shape, including a missing, malformed or misplaced `process` or `evidence_digest`.

  The same key may be retried while the proof lives, and nothing is applied.
- **Rejected: about the fact.** An inactive reply gives `coordination_rejected`, and so does an active one whose fact fails any other check: expiry, operation, kind, task, key digest, member or references. That refusal is final for this key and this proof: the member begins the step again through aicrew.
- **Mismatched: about the project's process.** A well-formed `process` that differs from the project's current selection, or a project with no selection, gives `process_mismatch`. It is final for this key and this proof: the member reloads the current process and begins the step again through aicrew.
- **Mismatched: about the delivery evidence.** A finalize whose `terminal_evidence` is not exactly the evidence the fact's `evidence_digest` binds gives `evidence_mismatch`. It is final for this key and this proof: the member finalizes with exactly the confirmed evidence, in its order, or begins the finalize again through aicrew if the confirmation changed.

The answer is used once. aimem takes it before the ledger transaction (C5 decision D2a), and the transaction commits only while the answer is at most 5 s old. The in-transaction recheck is aimem-owned and never queries aicrew.

### Replay

**A replay does not query the fact again.** An identical request with a committed receipt is answered from the receipt after aimem rechecks only the authority it owns: the live token, the grant, the profile, and that the replaying caller is the receipt's **acting member**: the member whose verified binding made the transition and is recorded on the receipt (C5a). For a holder's transition that is the holder (same user, mode, profile and role; the session may have moved on). For a coordinator's finalize it is that coordinator, from the session recorded on the receipt. It never calls `coordination.v1`, so a settled or expired proof never blocks reconciliation.

A retry whose first attempt did **not** commit is not a replay: it is a new attempt, and it verifies the fact again.

## 2. Aicrew's read scope (aicrew → aimem)

Served since task C6b; see the [ledger notes](AIFORGE-RESERVATION-LEDGER.md#c6b-aicrews-read-scope).

### Credential

A separate aimem peer credential, `aimem_peer_` followed by 256 random bits in hex, issued by the hub admin for the registered aicrew peer with the single operation `reservation.read`. Its lifecycle is identity.v1's: returned once, stored as a SHA-256 digest, at most 366 days, at most two active per peer and operation so a rotation can overlap, revocable, audited, and never logged. It cannot redeem proofs. The redemption credential (`identity.redeem`) cannot read reservations, and neither credential can mutate anything. Disabling the peer refuses both.

aimem records, on every receipt and hold committed under a verified fact, the `service_id` that answered the fact and the proof's `p1_` digest. The read scope sees exactly those records.

### Operations

All three are HTTP-only, over TLS the hub terminated itself, with `X-Aimem-Reservation-Version: 1`. The path `service_id` must be the authenticated peer. The hub's bearer gate confines a `reservation.read` credential to exactly these three route shapes, as it confines the redemption credential to its route, and refuses it everywhere else, including `/mcp`, before any handler runs.

| Operation | Route | Answer |
| --- | --- | --- |
| Receipt by proof | `GET /v1/identity/peers/{service_id}/reservation-receipts/{proof_digest}` | `{state: "committed", receipt}` for the transition committed under that proof, or `{state: "none"}` |
| Receipt by key | `GET /v1/identity/peers/{service_id}/reservations/{task_id}/receipts/{operation}/{request_key_digest}` | `{state: "committed", receipt}` when that transition was made on a reservation this service's proof established (a claim or transfer under its proof), otherwise `{state: "none"}`. This is how aicrew confirms a holder's `update`, which carries no proof |
| Hold status | `GET /v1/identity/peers/{service_id}/reservations/{task_id}` | `{state: "held", reservation_id, fence, holder_mode: "external", own_work_ref, task_revision}` (the field names reservation.v1's status uses) when the task's current hold was set under a proof this service issued; `{state: "closed", reservation_id, closing_fence, closed_by, closed_at, task_revision}` when this service's most recent reservation on the task is no longer active ([Closure evidence](#closure-evidence)); otherwise `{state: "none"}` |

The receipt is `{id, operation, task_id, request_key_digest, reservation_id, fence, task_revision, member_user_id, verified_mode: "team", committed_at}`.

### Closure evidence

Operator decision D-c1(a) (C5c seq206) gives aicrew positive evidence that **its own** reservation closed, however it closed. This includes operator recovery, which carries no proof aicrew issued (C5c). The rule replaces task 01a0de38, which it absorbs.

**Which reservation `closed` describes.** Hold status looks first at the task's current hold:
- If that hold was set under this service's proof, the answer is `held`.
- Otherwise the answer describes **this service's most recent reservation on the task**, meaning the latest reservation that a claim or transfer under one of its proofs established:
  - if that reservation is no longer active, the answer is `closed`;
  - if there is none, the answer is `none`.

`closed` is answered whatever holds the task now: nothing, a personal holder, or another service's hold. It never describes that other holder, neither its identity, mode or reference nor that it exists at all. A service with no reservation of its own on the task still gets `none`.

**Fields.**

| Field | Meaning |
| --- | --- |
| `reservation_id` | The closed reservation, exactly as aicrew recorded it from its own receipts |
| `closing_fence` | The fence the closing transition advanced to. It is always greater than any fence the reservation had while it was active |
| `closed_by` | One of the four values below |
| `closed_at` | When the closing transition committed (RFC 3339 UTC) |
| `task_revision` | The task revision that closing transition left |

**`closed_by` values.**

| Value | The closing transition |
| --- | --- |
| `holder_release` | A release by a member over its own verified connection: the holder's stop release, or a coordinator's release of an unaccepted offer (`stopped` or `never_accepted` facts) |
| `holder_finalize` | A finalize by the holder, or by the reviewing coordinator (`accepted_for_finalization` fact) |
| `recovery_release` | An operator recovery release (C5c), on aicrew stop evidence or on an operator attestation. Decision D-c2(a) allows an attestation even while aicrew is reachable |
| `recovery_cancel` | An operator recovery finalize as `CANCELLED` (C5c). Recovery never produces `DONE` |

The answer never names the recovery admin, the attestation, the reason or the evidence. Those stay in aimem's audit and in the recovery reader, which is admin-only.

**How aicrew uses it.** Aicrew closes an attempt as recovered, or confirms a transition it could not settle, only when both of these hold:
- the answer's `reservation_id` equals the attempt's reservation ID exactly;
- `closing_fence` is greater than the fence aicrew last confirmed for the attempt.

A `closed` answer for another reservation ID leaves the attempt open. So does a fence that did not advance, or a `none` or `held` answer. This is the crew contract's "Recovery" rule and task 01a0de37's criteria.

`closed` is durable: the same closed reservation reads the same until this service establishes a newer reservation on the task. Then it reads that newer reservation's state instead. A receipt that aicrew holds for the closing transition (its own `holder_release` or `holder_finalize`) still reconciles by key or by proof as before. `closed` adds evidence and replaces nothing.

**What the scope never returns:** task content, another holder's identity, reference or existence, a recovery admin, attestation or reason, a hold set under another service's proof or under personal mode, the raw request key, or the proof itself. Every out-of-scope record answers `none`, the same as a missing one.

**When `none` is final.** A committed transition's receipt is durable, and it commits in the same transaction as the transition. But `none` means only that nothing has committed *yet*: a member's request may still be on its way. The rule depends on whether the step carries a proof.

*A proof-backed transition* (every kind in the §1 table). Aicrew treats a receipt-by-proof `none` as final only after both of these:
- it has settled or voided the intent, so `coordination.v1` answers inactive;
- at least 10 s have passed since then, which covers aimem's 5 s answer age plus the 2 s budget and a margin.

Before that, a `none` is a reason to wait, not an outcome.

*A holder's `update`, which carries no proof.* Voiding an intent and letting time pass fence nothing: aimem checks no fact for an update, so a delayed update can commit at any time while the holder, fence and revision still match. A receipt-by-key `none` for an update therefore stays **unresolved**, however long ago aicrew abandoned the step, until separate evidence rules out a later commit:
- **The hold moved past the request.** Aicrew first observes the reservation's fence past the request's fence, or the task revision past the request's expected revision. It learns this from hold status, or from a later committed receipt on the same reservation. The request can then never commit: its fence or revision check fails. **Only after that** does aicrew read the receipt by key: `committed` is the outcome, and `none` is now final. The order matters. A receipt read *before* the evidence could miss an update that commits in between and then itself moves the fence.
- **The same key replays.** The member, or the step driver, resends the identical request with the same key. It is answered from a committed receipt, or it runs as a new attempt whose outcome is then known.

Until one of these, the step stays unresolved, and aicrew sends nothing that depends on it.

**Refusals.**

| Status | Code | Cause |
| --- | --- | --- |
| 400 | `unsupported_version` | missing or unsupported version |
| 401 | `peer_unauthenticated` | missing, unknown, revoked or expired credential |
| 403 | `peer_forbidden` | another operation's credential, or another service's path |
| 403 | `tls_required` | no hub-terminated TLS |
| 503 | `request_in_progress` | the store is busy (retryable) |

Reads are rate-limited to 60 per credential per minute (`rate_limited`, 429, retryable).

## 3. Refusals added to reservation.v1

| Code | Status | Retryable | Next action |
| --- | --- | --- | --- |
| `coordination_rejected` | 403 | no | Begin the step again through aicrew; never reuse the proof or the key. |
| `process_mismatch` | 409 | no | Reload the project's current process, then begin the step again through aicrew under it; never reuse the proof or the key. |
| `evidence_mismatch` | 409 | no | Finalize with exactly the evidence aicrew confirmed, in its order; if the confirmation changed, begin the finalize again through aicrew; never reuse the proof or the key. |

An unreachable or unparsable `coordination.v1` answer is the existing `context_unavailable` (503, retryable).

## 4. Reservation CLI (for C6)

C6 must offer the reservation mutations and the receipt read as `aimem` CLI commands as well as MCP tools, so aicrew's client can drive a step deterministically without a model (served since task C6c; see the [ledger notes](AIFORGE-RESERVATION-LEDGER.md#c6c-the-member-reservation-cli)):

```
aimem reservation claim|transfer|update|release|finalize --task TASK_ID --key REQUEST_KEY
aimem reservation receipt OPERATION --task TASK_ID --key REQUEST_KEY
aimem reservation status --task TASK_ID
```

- **Input.** The mutation body (expected revision, reservation ID and fence, holder, complete content, an update's optional `intent`, reason, terminal evidence and `coordination_proof`) is read as JSON from **standard input only**, never argv, as E5a reads the session handle. The request key is not a secret and may be an argument.
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
- A `closed` answer is durable until this service establishes a newer reservation on the task.
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
- **C5-w2** amends the `offer`, `accepted_attempt` and `independent_claim` facts with the process pin and adds `process_mismatch`. The fake-consumer test checks:
  - that the pin appears exactly on those three kinds, with exactly the three fields, in the hub selection's forms;
  - the malformed pins, as `context_unavailable`;
  - the comparison rule against a full hub selection;
  - that a failing fact is rejected before the pin is compared;
  - that a replay does not compare the pin again;
  - that the refusal matches the reservation.v1 fixtures.
- **C5-w3** amends the `accepted_for_finalization` fact with the evidence digest, adds `evidence_mismatch`, and implements the check. It is the last in-place amendment before the pilot. The tests check:
  - the canonical vectors (`evidence_digest.vectors`) against aimem's digest;
  - that the digest appears exactly on the finalize fact, over its member request's evidence;
  - the malformed digests, as `context_unavailable`;
  - a reordered, altered, dropped or extra reference, as `evidence_mismatch` with nothing applied, through the real routes;
  - the replay, a changed list under the same key, and a personal `DONE`, which is unchanged;
  - that the refusal matches the reservation.v1 fixtures and the hub's.
- **C5b** implements aimem's `coordination.v1` client and the coordination-backed transitions against a fake, including the process-pin check.
- **C6** serves the read scope, the reservation routes, the MCP tools and the CLI.
- **Aicrew b0** serves `coordination.v1`, and b3 reconciles through the read scope. Aicrew's C5-w3 counterpart produces the evidence digest.

**Pilot prerequisite: lockstep deploy.** `evidence_digest` is required on both sides. An aimem with C5-w3 refuses an older aicrew's finalize facts as the wrong shape, and an older aimem refuses a newer aicrew's field the same way. Both refusals are retryable and apply nothing. So aimem and aicrew deploy their C5-w3 changes together, before the pilot.
