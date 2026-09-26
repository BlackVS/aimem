# AIForge identity proof and introspection wire contract, version 1

Status: reviewed design candidate for task E2. **No route, MCP tool, verifier, peer registration or credential is implemented by this document.** The proposed OpenAPI and examples live in `docs/fixtures/identity-v1/`. The live `internal/server/openapi.json` continues to describe live routes only.

This contract freezes the wire for the one-time identity proof and the online session introspection defined by the merged [identity and context contract](DESIGN-AIFORGE-CONTEXT.md). It composes with the [enrollment contract](DESIGN-AIFORGE-ENROLLMENT.md) (D1) and with aicrew `main` at [daebc43](https://github.com/BlackVS/aicrew/tree/daebc43018447bbc426d2a668a5bcc42f7391d7a). That aicrew commit covers its onboarding contract, its crew contract and the `store.Verifier` interface in `internal/store/identity.go`. If a parent contract disagrees with this document, the parent wins. Updated 2026-09-26.

## Approved operator decisions

The owner approved these decisions on task E2, recorded in comments seq161 and seq162. They are design decisions, not implementation authorization.

1. **Peer authentication uses service bearer credentials over TLS.** Each direction has its own credential, bound to one peer and one permitted operation. The receiving service stores only a digest, and the sending service protects the bearer at rest. Each credential can be revoked and rotated independently and never appears in logs. The caller verifies the server's TLS identity against the registered trust binding: a CA-backed DNS name or a pinned SHA-256 public key. Plain HTTP, trust on first use and disabled verification are refused. A service credential cannot enroll users, issue agent tokens, grant project access or act as an agent. It never replaces the user, session, generation or grant checks.
2. **The hub admin registers the peer, with audit.** The existing aimem hub admin role registers the aicrew peer through an explicit operation. The registration binds the service ID, the allowed hub (this hub), the permitted operations, the introspection endpoint and the TLS trust binding. Registration, change, rotation and revocation are audited, and the audit never records a secret. The pilot allows one aicrew service per hub. Aicrew's operator registers aimem's introspection credential on the aicrew side independently. Registering a peer grants no project access and no enrollment authority.
3. **Any enabled user token may request a proof receipt.** The token must be valid, unexpired, unrevoked and user-scoped, and its user must be enabled. Project-scoped, read-only, legacy, admin and checkpoint tokens are refused. The client requests the receipt explicitly for one registered peer and one challenge, and only that peer can redeem it. Redemption discloses to that peer only the hub ID, user ID, token ID and token state. A proof establishes identity only. It grants no membership, role or resource access. Neither the receipt nor the peer ever receives the bearer.
4. **Aicrew issues a separate session handle for aimem.** Aicrew issues a distinct random handle scoped to aimem, which the client stores and refreshes automatically as session state. The user never configures it. The handle is bound to the exact aimem hub, the aicrew service, the agent identity, the session and the generation. Aicrew rejects it on every endpoint where an agent acts, and aimem forwards it only on the authenticated introspection call. This design limits the authority that crosses between the services. It does not make a fully compromised aimem service safe.

## Version, encoding and secrets

The protocol is `identity.v1`. HTTP carries the version in `X-Aimem-Identity-Version: 1`, and MCP carries `version: 1` in tool input. A missing or unsupported version fails with `unsupported_version` and changes nothing. Bodies are UTF-8 JSON, IDs are stable opaque strings, and timestamps are RFC 3339 UTC. A generation is a positive decimal string, which avoids JSON integer precision loss.

| Secret | Format | Lifetime | Stored by |
| --- | --- | --- | --- |
| Proof receipt | `amr1_` followed by 43 base64url characters (256 random bits) | At most 60 s | Aimem, as a SHA-256 digest only |
| Aimem-scoped session handle | `acs1_` followed by 43 base64url characters (256 random bits) | At most 15 min | Aicrew, as a digest only. The client keeps it in protected session state. |
| Aicrew→aimem redemption credential | Issued by aimem as a peer token | Until rotated or revoked | Aimem stores a digest. Aicrew keeps the bearer in protected storage. |
| Aimem→aicrew introspection credential | Issued by aicrew | Until rotated or revoked | Aicrew stores a digest. Aimem keeps the bearer in protected storage. |

No response body, audit record, log line, task, fixture evidence or error message contains any of these secrets, an individual bearer or a D1 enrollment subcode. A secret always travels in a request position and never in a response. The one exception is the receipt, which goes back only to the individual credential holder who requested it.

## 1. Proof receipt (agent → aimem)

`POST /v1/identity/proofs`, with the matching MCP tool `identity_proof`. The caller authenticates with its individual aimem bearer. The body is `{peer_service_id, hub_id, challenge_id}`. The challenge ID comes from aicrew and is opaque to aimem: 1 to 128 characters from `[A-Za-z0-9._:-]`. Aimem cannot see the challenge's deadline and does not need it, because the receipt deadline is shorter.

Aimem runs these checks in order. It authenticates the bearer and checks decision 3. It requires `hub_id` to be this hub and `peer_service_id` to be an active registered peer on this hub. Any failure is a non-disclosing `peer_unknown`. It then applies the issuance bounds. On success it returns `200` with `{receipt, receipt_id, expires_at, binding: {hub_id, peer_service_id, challenge_id, user_id, token_id}}`. The binding contains only the caller's own IDs. The call has no request key, because a lost reply is recovered by requesting a new receipt, as the context contract prescribes. Each (token, peer, challenge) has at most one live unredeemed receipt, so a new request supersedes the old one. Aimem allows a new receipt for a challenge even after an earlier receipt was redeemed, because aicrew may have refused that completion afterwards. Issuance is rate-limited to 10 receipts per token per minute.

## 2. Receipt redemption (aicrew → aimem)

`POST /v1/identity/peers/{service_id}/redemptions`. This route is service-only and has no MCP tool. The caller authenticates with its aicrew redemption credential. `Idempotency-Key` carries aicrew's request key in encoded form: `k1_` followed by the unpadded base64url SHA-256 of the key's UTF-8 bytes, which is always 46 printable characters. Aicrew derives request keys such as `redeem:<challenge>:<key>`, and its completion key can be up to 128 bytes of any valid UTF-8, including spaces, non-ASCII characters and control characters. That domain cannot travel raw in an HTTP header, and a raw key could exceed any fixed header limit. The encoding covers every such key: identical keys always encode identically, and SHA-256 makes it infeasible for two different keys to share an encoding. The raw key never leaves aicrew. Aimem stores, compares and echoes only the encoded value. The body is `{hub_id, challenge_id, receipt}`. The path service ID must equal the authenticated peer, and the credential must permit `identity.redeem`.

In one transaction, aimem finds the receipt digest and checks that the receipt is unexpired, unredeemed and not superseded. The receipt must have been issued for this peer, for this `challenge_id` and on this `hub_id`. Aimem rechecks that the token is active, unrevoked and user-scoped and that its user is enabled. It then consumes the receipt and records the outcome under (peer, request key) together with a digest of the input. The response is `200` with `{redemption_id, request_key, replayed, peer_service_id, challenge_id, identity: {hub_id, user_id, token_id}, token_state: "active", redeemed_at}`. It contains no display name, grant, membership or bearer.

An identical retry with the same key and input returns the recorded result with `replayed: true`. Aimem rechecks the token state before answering. If the token has been revoked since the redemption, the retry is refused with `credential_inactive`. The same key with different input returns `idempotency_conflict`. If an identical request is still running, the retry waits up to 5 s and then returns `request_in_progress`, which is retryable. Aicrew bounds each redemption call to 10 s, which is shorter than its own 30-second in-flight wait. A timeout counts as a lost reply and is retried with the same key. A different key presented with a receipt that has already been redeemed returns `proof_invalid`, so a race between two keys has exactly one winner. Unknown, expired, superseded, already redeemed, wrong-peer, wrong-challenge and wrong-hub receipts all return the same `proof_invalid`, and the peer audit keeps the exact reason. Outcomes and receipt digests are kept for 15 minutes. That covers the 5-minute challenge, aicrew's 30-second in-flight wait and retry margin. After pruning, a replay returns `proof_invalid`, and aicrew recovers through a new receipt or a new challenge.

**Mapping to aicrew's `Verifier`.** `RedeemRequest.ChallengeID`, `HubID`, `Receipt` and `RequestKey` map to `challenge_id`, `hub_id`, `receipt` and the encoded `Idempotency-Key`. Aicrew's configured service ID fills the path. `VerifiedIdentity` is `identity`. Every refusal becomes a returned error, never a partial identity. Aicrew already checks that the identity is complete and that the hub matches the challenge. Retryable codes leave both stores unchanged.

## 3. Session introspection (aimem → aicrew)

`POST {registered endpoint}/v1/crew/introspect`. Aicrew owns this route. Its crew-context implementation must adopt the route as specified here, or propose a reviewed v2. Aimem authenticates with its introspection credential and sends `{version: 1, hub_id, nonce, handle}`. The nonce is 128 random bits, fresh for each call. Aimem sends no expected user, token or team, so aicrew answers only from its own stored link and session state and cannot echo IDs that aimem supplied.

An active handle gets `200` with `{nonce, active: true, service_id, hub_id, identity: {user_id, token_id}, agent_id, team_id, role, session_id, generation, handle_expires_at}`. Every other state gets `200` with `{nonce, active: false}`: unknown, expired, superseded, ended, stopped, fenced by a generation change, bound to another hub, or presented with a handle of the wrong audience. That second reply carries no reason, so a probe cannot learn why a handle is inactive. Aicrew never returns a grant, a profile decision or another member's data.

The budget is one attempt of at most 2 s, covering connect, TLS and response, with a 16 KiB response cap. There is no retry inside a request, and caller cancellation propagates. Aimem accepts a reply only if all of these hold:

- the TLS identity matches the registration;
- the nonce matches;
- `service_id` is the registered peer and `hub_id` is this hub;
- `handle_expires_at` is in the future;
- the generation is positive;
- `(service_id, team_id)` is linked to an access profile.

Any failure to reach, authenticate or parse the reply results in `context_unavailable` for team operations, with nothing applied. No allow decision is cached.

## 4. Team-mode request (client → aimem)

The client sends its individual bearer in `Authorization` and the aimem-scoped handle in `X-Aimem-Team-Context`. The local daemon attaches the handle from the connection's fixed context. It never takes the handle from a tool argument and never reads it from a global setting (E5). A request without the header is evaluated in personal mode and never calls aicrew. An operation that exists only in team mode is refused with `context_missing` instead. A handle on a connection bound to personal mode is `invalid_request`.

The verifier runs in the order the context contract sets:

1. Authenticate the bearer. A revoked or expired token, or a disabled user, gets `invalid_credential` before aicrew is contacted.
2. Introspect the handle.
3. Compare the reply with the authenticated caller. Aimem keeps the hub ID and user ID that the verified proof linked, and both must match exactly. A different user or hub gets `identity_mismatch`. The same user with a different token ID is a rotation the session has not caught up with yet. That gets `context_stale`, and the next action is re-proof through aicrew, which advances the generation and issues a new handle.
4. Evaluate only the linked profile's live grants, then role and reservation policy.

The audit records the actor, the mode, the service, the team, the session, the generation and the correlation ID, never the handle.

## Handle lifecycle

Aicrew issues the aimem-scoped handle together with its own client-side session state at session start, resume or re-proof. The client refreshes the handle through aicrew's session API before it expires. A refresh keeps the same session and generation. The previous handle then stays valid for at most 60 more seconds, never beyond its own expiry, so requests already in flight can finish. A generation change, rotation, role change, leave, removal or operator stop revokes every aimem-scoped handle of that session immediately. After that, introspection answers `active: false`. Replaying an old handle is harmless, because introspection gives the current answer.

## Refusals

Every refusal uses the context contract's envelope: `{code, message, active_mode, retryable, next_action, correlation_id}`. `active_mode` is omitted when revealing it would be unsafe. The `next_action` never tells a caller to switch to personal or broader credentials. HTTP status is fixed by v1, and MCP carries the same code.

| Code | Status | Retryable | Next action |
| --- | --- | --- | --- |
| `invalid_request`, `unsupported_version` | 400 | no | Correct the request or use a supported version. |
| `invalid_credential` | 401 | no | Renew or recover the individual credential through its authorized flow. |
| `peer_unauthenticated` | 401 | no | Operator checks the peer registration and credential. |
| `credential_scope_forbidden` | 403 | no | Use the installation's user-scoped individual credential. |
| `peer_unknown` | 403 | no | Verify the hub and aicrew service locators. |
| `peer_forbidden` | 403 | no | Operator checks the peer's permitted operations. |
| `proof_invalid` | 403 | no | Obtain a new receipt, or begin a new challenge. |
| `credential_inactive` | 403 | no | Recover the individual credential; do not link. |
| `identity_mismatch`, `identity_link_required`, `context_missing`, `context_stale` | 403 | no | Re-prove or resume through aicrew, and reconcile outstanding work. |
| `grant_denied`, `role_forbidden` | 403 | no | Request an authorized change, or use the role's permitted flow. |
| `idempotency_conflict` | 409 | no | Investigate the changed input; never reuse the key for other input. |
| `rate_limited` | 429 | yes | Wait, then request again. |
| `request_in_progress`, `identity_unavailable`, `context_unavailable` | 503 | yes | Retry later with the same key or context; nothing was applied. |

## Bounds

The limits below are values fixed by this contract. An implementation may tighten them. Relaxing any of them requires a reviewed v2.

- An aicrew challenge lives at most 5 min.
- A receipt lives at most 60 s. Each (token, peer, challenge) has at most one live receipt, and each token may request at most 10 receipts per minute.
- Redemption outcomes are kept for 15 min. A same-key wait on the server lasts at most 5 s, and aicrew's redemption call times out after at most 10 s.
- An introspection call gets one attempt of at most 2 s, and its response may be at most 16 KiB.
- An aimem-scoped handle lives at most 15 min, and after a refresh the old handle overlaps for at most 60 s.

## D1 boundary and delivery

The D1 enrollment path issues the individual credential. This contract only verifies that credential. A freshly enrolled user-scoped token can request a receipt immediately. The proof route never accepts an enrollment subcode, and a peer credential can never call enrollment. When D1 reissues a credential, it revokes the old token in the same transaction. Receipts that are still outstanding for the old token then fail with `credential_inactive`. Sessions bound to the old token get `context_stale` until aicrew re-proves them.

E3 implements proof issuance, redemption and peer registration against a fake aicrew. E4 implements introspection and the team-mode verifier against a fake introspection server. E5 implements local binding. The fixture test in `internal/server/identity_wire_contract_test.go` is a fake consumer. It checks coverage, the parity between the fixtures and the proposed OpenAPI, the bounds, and that no secret appears in a response. It also checks that no production identity route is registered. It does not prove authorization.
