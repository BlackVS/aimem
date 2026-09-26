# AIForge identity proof and introspection wire contract, version 1

Status: reviewed contract (task E2). E3a implements the ledger and E3b serves the proof, redemption and peer-management routes over TLS terminated by the hub; see [E3 implementation boundary](#e3-implementation-boundary). E4a adds the introspection client, and E4b serves the approved team-mode reads through the verifier; see [E4 implementation boundary](#e4-implementation-boundary). **There is no MCP tool, and no team-mode write.** The contract fixtures live in `docs/fixtures/identity-v1/`; the live `internal/server/openapi.json` describes the served routes.

This contract freezes the wire for the one-time identity proof and the online session introspection defined by the merged [identity and context contract](DESIGN-AIFORGE-CONTEXT.md). It composes with the [enrollment contract](DESIGN-AIFORGE-ENROLLMENT.md) (D1) and with aicrew `main` at [daebc43](https://github.com/BlackVS/aicrew/tree/daebc43018447bbc426d2a668a5bcc42f7391d7a). That aicrew commit covers its onboarding contract, its crew contract and the `store.Verifier` interface in `internal/store/identity.go`. If a parent contract disagrees with this document, the parent wins. Updated 2026-09-26.

## Approved operator decisions

The owner approved these decisions on task E2, recorded in comments seq161 and seq162. They are design decisions, not implementation authorization.

1. **Peer authentication uses service bearer credentials over TLS.** Each direction has its own credential, bound to one peer and one permitted operation. The receiving service stores only a digest, and the sending service protects the bearer at rest. Each credential can be revoked and rotated independently and never appears in logs. The caller verifies the server's TLS identity against the registered trust binding: a CA-backed DNS name or a pinned SHA-256 public key. Plain HTTP, trust on first use and disabled verification are refused. A service credential cannot enroll users, issue agent tokens, grant project access or act as an agent. It never replaces the user, session, generation or grant checks.
2. **The hub admin registers the peer, with audit.** The existing aimem hub admin role registers the aicrew peer through an explicit operation. The registration binds the service ID, the allowed hub (this hub), the permitted operations, the introspection endpoint and the TLS trust binding. Registration, change, rotation and revocation are audited, and the audit never records a secret. The pilot allows one aicrew service per hub. Aicrew's operator registers aimem's introspection credential on the aicrew side independently. Registering a peer grants no project access and no enrollment authority.
3. **Any enabled user token may request a proof receipt.** The token must be valid, unexpired, unrevoked and user-scoped, and its user must be enabled. Project-scoped, read-only, legacy, admin and checkpoint tokens are refused. The client requests the receipt explicitly for one registered peer and one challenge, and only that peer can redeem it. Redemption discloses to that peer only the hub ID, user ID, token ID and token state. A proof establishes identity only. It grants no membership, role or resource access. Neither the receipt nor the peer ever receives the bearer.
4. **Aicrew issues a separate session handle for aimem.** Aicrew issues a distinct random handle scoped to aimem, which the client stores and refreshes automatically as session state. The user never configures it. The handle is bound to the exact aimem hub, the aicrew service, the agent identity, the session and the generation. Aicrew rejects it on every endpoint where an agent acts, and aimem forwards it only on the authenticated introspection call. This design limits the authority that crosses between the services. It does not make a fully compromised aimem service safe.

## Version, encoding and secrets

The protocol is `identity.v1`. HTTP carries the version in `X-Aimem-Identity-Version: 1`. identity.v1 is HTTP-only and has no MCP tool. A missing or unsupported version fails with `unsupported_version` and changes nothing. Introspection is the one identity.v1 call that aicrew receives, and it carries the version in two places: the `X-Aimem-Identity-Version` header and the body field `version`. Both are required and must both be `1`. When either is missing or unsupported, or when the two disagree, aicrew answers `400` with `unsupported_version` and evaluates nothing, so it never reports a handle as active on such a request. Aimem treats that refusal like any other failed introspection. The team operation fails with `context_unavailable` and nothing is applied, and the peer audit records a version mismatch. An introspection reply carries no version. Bodies are UTF-8 JSON, IDs are stable opaque strings, and timestamps are RFC 3339 UTC. A generation is a positive decimal string, which avoids JSON integer precision loss.

| Secret | Format | Lifetime | Stored by |
| --- | --- | --- | --- |
| Proof receipt | `amr1_` followed by 43 base64url characters (256 random bits) | At most 60 s | Aimem, as a SHA-256 digest only |
| Aimem-scoped session handle | `acs1_` followed by 43 base64url characters (256 random bits) | At most 15 min | Aicrew, as a digest only. The client keeps it in protected session state. |
| Aicrew→aimem redemption credential | Issued by aimem as a peer token | Until rotated or revoked | Aimem stores a digest. Aicrew keeps the bearer in protected storage. |
| Aimem→aicrew introspection credential | Issued by aicrew | Until rotated or revoked | Aicrew stores a digest. Aimem keeps the bearer in protected storage. |

No response body, audit record, log line, task, fixture evidence or error message contains any of these secrets, an individual bearer or a D1 enrollment subcode. A secret may appear only in the locations listed below. Every other location is forbidden, including responses, audit records, refusal envelopes and messages.

| Secret | Permitted locations |
| --- | --- |
| Individual bearer | `Authorization` header of a proof request or a team-mode request |
| Proof receipt | The `receipt` field of the proof response, returned only to the credential holder who requested it, and the `receipt` field of a redemption request |
| Aimem-scoped handle | The `X-Aimem-Team-Context` header of a team-mode request, and the `handle` field of an introspection request |
| Redemption credential | The `Authorization` header of a redemption request |
| Introspection credential | The `Authorization` header of an introspection request |

## 1. Proof receipt (agent → aimem)

`POST /v1/identity/proofs`. It is HTTP-only: **there is no MCP tool**, because the receipt is a secret that must never enter a model-visible tool result, transcript or log. The client bootstrap or aicrew client code that holds the individual credential calls this route directly. The caller authenticates with its individual aimem bearer. The body is `{peer_service_id, hub_id, challenge_id}`. The challenge ID comes from aicrew and is opaque to aimem: 1 to 128 characters from `[A-Za-z0-9._:-]`. Aimem cannot see the challenge's deadline and does not need it, because the receipt deadline is shorter.

Aimem runs these checks in order. It authenticates the bearer and checks decision 3. It requires `hub_id` to be this hub and `peer_service_id` to be an active registered peer on this hub. Any failure is a non-disclosing `peer_unknown`. It then applies the issuance bounds. On success it returns `200` with `{receipt, receipt_id, expires_at, binding: {hub_id, peer_service_id, challenge_id, user_id, token_id}}`. The binding contains only the caller's own IDs. The call has no request key, because a lost reply is recovered by requesting a new receipt, as the context contract prescribes. Each (token, peer, challenge) has at most one live unredeemed receipt, so a new request supersedes the old one. Aimem allows a new receipt for a challenge even after an earlier receipt was redeemed, because aicrew may have refused that completion afterwards. Issuance is rate-limited to 10 receipts per token per minute.

## 2. Receipt redemption (aicrew → aimem)

`POST /v1/identity/peers/{service_id}/redemptions`. This route is service-only and has no MCP tool. The caller authenticates with its aicrew redemption credential. `Idempotency-Key` carries aicrew's request key in encoded form: `k1_` followed by the unpadded base64url SHA-256 of the key's UTF-8 bytes, which is always 46 printable characters. Aicrew derives request keys such as `redeem:<challenge>:<key>`, and its completion key can be up to 128 bytes of any valid UTF-8, including spaces, non-ASCII characters and control characters. That domain cannot travel raw in an HTTP header, and a raw key could exceed any fixed header limit. The encoding covers every such key: identical keys always encode identically, and SHA-256 makes it infeasible for two different keys to share an encoding. The raw key never leaves aicrew. Aimem stores, compares and echoes only the encoded value. The body is `{hub_id, challenge_id, receipt}`. The path service ID must equal the authenticated peer, and the credential must permit `identity.redeem`.

In one transaction, aimem finds the receipt digest and checks that the receipt is unexpired, unredeemed and not superseded. The receipt must have been issued for this peer, for this `challenge_id` and on this `hub_id`. Aimem rechecks that the token is active, unrevoked and user-scoped and that its user is enabled. It then consumes the receipt and records the outcome under (peer, request key) together with a digest of the input. The response is `200` with `{redemption_id, request_key, replayed, peer_service_id, challenge_id, identity: {hub_id, user_id, token_id}, token_state: "active", redeemed_at}`. It contains no display name, grant, membership or bearer.

An identical retry with the same key and input returns the recorded result with `replayed: true`. Aimem rechecks the token state before answering. If the token has been revoked since the redemption, the retry is refused with `credential_inactive`. The same key with different input returns `idempotency_conflict`. If an identical request is still running, the retry waits up to 5 s and then returns `request_in_progress`, which is retryable. Aicrew bounds each redemption call to 10 s, which is shorter than its own 30-second in-flight wait. A timeout counts as a lost reply and is retried with the same key. A different key presented with a receipt that has already been redeemed returns `proof_invalid`, so a race between two keys has exactly one winner. Unknown, expired, superseded, already redeemed, wrong-peer, wrong-challenge and wrong-hub receipts all return the same `proof_invalid`, and the peer audit keeps the exact reason. Outcomes and receipt digests are kept for 15 minutes. That covers the 5-minute challenge, aicrew's 30-second in-flight wait and retry margin. After pruning, a replay returns `proof_invalid`, and aicrew recovers through a new receipt or a new challenge.

**Mapping to aicrew's `Verifier`.** `RedeemRequest.ChallengeID`, `HubID`, `Receipt` and `RequestKey` map to `challenge_id`, `hub_id`, `receipt` and the encoded `Idempotency-Key`. Aicrew's configured service ID fills the path. `VerifiedIdentity` is `identity`. Every refusal becomes a returned error, never a partial identity. Aicrew already checks that the identity is complete and that the hub matches the challenge. Retryable codes leave both stores unchanged.

## 3. Session introspection (aimem → aicrew)

`POST /v1/crew/introspect` on the peer. The registered endpoint is the full https URL of this route, as the fixtures show (`https://aicrew.example/v1/crew/introspect`); a registration whose path is anything else is not operational. Aicrew owns this route. Its crew-context implementation must adopt the route as specified here, or propose a reviewed v2. Aimem authenticates with its introspection credential and sends `{version: 1, hub_id, nonce, handle}`. The nonce is `n-` followed by 128 random bits in lowercase hex, fresh for each call. Aimem sends no expected user, token or team, so aicrew answers only from its own stored link and session state and cannot echo IDs that aimem supplied.

An active handle gets `200` with `{nonce, active: true, service_id, hub_id, identity: {user_id, token_id}, agent_id, team_id, role, session_id, generation, handle_expires_at}`. `role` is one of aicrew's member roles: `coordinator`, `worker` or `independent`. Every other state gets `200` with `{nonce, active: false}`: unknown, expired, superseded, ended, stopped, fenced by a generation change, bound to another hub, or presented with a handle of the wrong audience. That second reply carries no reason, so a probe cannot learn why a handle is inactive. Aicrew never returns a grant, a profile decision or another member's data.

The budget is one attempt of at most 2 s, covering connect, TLS and response. Aimem reads at most 16,384 bytes of the reply body. A longer reply is refused as `context_unavailable` without being parsed. There is no retry inside a request, and caller cancellation propagates. Aimem accepts a reply only if all of these hold:

- the TLS identity matches the registration;
- the nonce matches;
- `service_id` is the registered peer and `hub_id` is this hub;
- `handle_expires_at` is in the future;
- the generation is positive;
- `role` is one of the three roles, and every ID is present;
- `(service_id, team_id)` is linked to an access profile.

Any failure to reach, authenticate or parse the reply, or a reply that fails these checks, results in `context_unavailable` for team operations, with nothing applied. Two answers are about the session rather than the peer and give `context_stale`, whose next action is to revalidate through aicrew: `active: false`, and an active reply whose `handle_expires_at` the hub clock has already passed. The hub clock is authoritative, with no skew allowance; the handle refresh overlap absorbs ordinary drift. No allow decision is cached.

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

Every refusal uses the context contract's envelope: `{code, message, active_mode, retryable, next_action, correlation_id}`. `active_mode` is omitted when revealing it would be unsafe. The `next_action` never tells a caller to switch to personal or broader credentials. HTTP status is fixed by v1.

| Code | Status | Retryable | Next action |
| --- | --- | --- | --- |
| `invalid_request`, `unsupported_version` | 400 | no | Correct the request or use a supported version. |
| `tls_required` | 403 | no | Connect to the hub's TLS listener with certificate verification. |
| `invalid_credential` | 401 | no | Renew or recover the individual credential through its authorized flow. |
| `peer_unauthenticated` | 401 | no | Operator checks the peer registration and credential. |
| `credential_scope_forbidden` | 403 | no | Use the installation's user-scoped individual credential. |
| `peer_unknown` | 403 | no | Verify the hub and aicrew service locators. |
| `peer_forbidden` | 403 | no | Operator checks the peer's permitted operations. |
| `proof_invalid` | 403 | no | Obtain a new receipt, or begin a new challenge. |
| `credential_inactive` | 403 | no | Recover the individual credential; do not link. |
| `identity_mismatch`, `identity_link_required`, `context_missing`, `context_stale` | 403 | no | Re-prove or resume through aicrew, and reconcile outstanding work. |
| `grant_denied`, `role_forbidden` | 403 | no | Request an authorized change, or use the role's permitted flow. |
| `team_operation_unsupported` | 403 | no | Use the aicrew flow for this work; team mode does not serve this operation. |
| `idempotency_conflict` | 409 | no | Investigate the changed input; never reuse the key for other input. |
| `rate_limited` | 429 | yes | Wait, then request again. |
| `request_in_progress`, `identity_unavailable`, `context_unavailable` | 503 | yes | Retry later with the same key or context; nothing was applied. |

## Bounds

The limits below are values fixed by this contract. An implementation may tighten them. Relaxing any of them requires a reviewed v2.

- An aicrew challenge lives at most 5 min.
- A receipt lives at most 60 s. Each (token, peer, challenge) has at most one live receipt, and each token may request at most 10 receipts per minute.
- Redemption outcomes are kept for 15 min. A same-key wait on the server lasts at most 5 s, and aicrew's redemption call times out after at most 10 s.
- An introspection call gets one attempt of at most 2 s, and its reply body may be at most 16,384 bytes.
- An aimem-scoped handle lives at most 15 min, and after a refresh the old handle overlaps for at most 60 s.

## D1 boundary and delivery

The D1 enrollment path issues the individual credential. This contract only verifies that credential. A freshly enrolled user-scoped token can request a receipt immediately. The proof route never accepts an enrollment subcode, and a peer credential can never call enrollment. When D1 reissues a credential, it revokes the old token in the same transaction. Receipts that are still outstanding for the old token then fail with `credential_inactive`. Sessions bound to the old token get `context_stale` until aicrew re-proves them.

E3 implements proof issuance, redemption and peer registration against a fake aicrew. E4 implements introspection and the team-mode verifier against a fake introspection server. E5 implements local binding. The fixture test in `internal/server/identity_wire_contract_test.go` is a fake consumer. It checks coverage, the parity between the fixtures and the proposed OpenAPI, the bounds, and that no secret appears in a response. It also checks that each contract path is a live route that requires TLS and names no MCP tool, and that the hub never serves the aicrew-owned route. It does not prove authorization; `internal/server/identity_routes_test.go` exercises the served routes.

## E3 implementation boundary

The owner approved splitting E3 and made four implementation decisions (E3 task comments seq169 and seq170).

**Split.** E3a is the internal ledger. E3b adds the routes over TLS that aimem terminates itself. E3c adds the operator CLI. **E3c must be delivered before the real pilot and before onboarding is accepted.** The web console remains deferred.

**TLS deployment requirement (from E3b onward).** The proof, redemption and peer-management routes accept a request only when aimem terminated its TLS itself, meaning the hub is started with a certificate and key. A request over plain HTTP, or one terminated by a proxy, is refused. Forwarded headers are never accepted as proof of TLS. Proxy-terminated TLS is deferred. Routes unrelated to identity keep their current behavior.

**Peer credential lifecycle.**
- **Format.** A peer credential is `aimem_peer_` followed by 256 random bits in hex. It is returned once, when issued, and stored only as a SHA-256 digest.
- **Scope.** It is bound to one registered peer and to the `identity.redeem` operation family.
- **Limits.** It lives at most 366 days. A peer has at most two active credentials, so a rotation can overlap.
- **Lost issuance response.** The bearer cannot be recovered. The admin lists the peer's credential metadata, revokes the credential that was never received, and issues a new one.
- **Expiry.** An expired credential is refused as if unknown and no longer counts toward the limit of two.
- **Revocation.** It applies to authentication that starts after it commits, affects only that credential, and is idempotent. Disabling a peer refuses all of its credentials.
- **Rotation.** Issue a second credential, move aicrew to it, then revoke the old one. Issuing a third while two are active is refused.
- The bearer is never logged or audited.

**Outbound introspection credential.** Aimem's copy of the credential that aicrew issues was deferred to E4; E4a delivers it (see [E4 implementation boundary](#e4-implementation-boundary)). E3 stores the peer's introspection endpoint and TLS trust binding.

**E3a status.** Access schema 4 adds four ledger tables: `identity_peers`, `identity_peer_credentials`, `identity_receipts` and `identity_redemptions`. The code is `internal/access/identity_proofs.go`. Its only callers are E3b's routes; no MCP tool or command reaches it. Refusal reasons are audited under `identity.redeem.refused.<reason>`, while callers see only the stable codes above. Aimem stores and compares redemption keys only in `k1_` form.

**Schema 4 upgrade and rollback.** The migration from schema 3 is additive and runs in one transaction; all existing data is preserved. Older binaries refuse schema 4, so rolling back the binary alone is not supported. Back up the state root before upgrading. A rollback restores the pre-upgrade `access.db` together with the matching hub state.

**E3b status.** `internal/server/identity.go` serves the two wire routes and six hub-admin routes, all only over TLS terminated by the hub:

| Route | Caller |
| --- | --- |
| `POST /v1/identity/proofs` | A live user-scoped individual token. Host-managed admin, legacy writer, project-scoped and read-only credentials get `credential_scope_forbidden`. |
| `POST /v1/identity/peers/{service_id}/redemptions` | The registered peer's `aimem_peer_` credential, for its own `service_id` only (`peer_forbidden` otherwise). |
| `GET`, `POST /v1/identity/peers`; `PUT /v1/identity/peers/{service_id}` | Hub admin: list, register, disable or re-enable the peer. |
| `GET`, `POST /v1/identity/peers/{service_id}/credentials`; `DELETE …/credentials/{credential_id}` | Hub admin: list metadata, issue (bearer shown once, `no-store`), revoke. |

A peer credential authenticates as a separate `peer` role that the bearer gate confines to the redemption route shape; every other route, including `/mcp`, refuses it before any handler runs. The peer listing reports `introspection_operational` (always false until E4a computes it). Proof and credential-issue responses are `no-store`, and no refusal, log line or audit record carries a secret. A proof or redemption waits at most 5 s for the store: the bearer gate starts a request deadline of 5 s before it authenticates, the authentication queries and the ledger transaction both run under it, and a call that cannot proceed in time answers the retryable `request_in_progress` without applying anything. The bound covers contention inside the hub process, where every access-store call shares one connection. A SQLite lock held by another process is bounded separately by the store's 5 s busy timeout, because the database driver does not abandon a busy wait early when the deadline passes. Every refusal the gate makes on the two wire routes (a missing, unknown, revoked or expired bearer, or the wrong kind of credential) uses the refusal envelope: `invalid_credential` on the proof route and `peer_unauthenticated` on the redemption route, or `credential_scope_forbidden` for a peer bearer on the proof route. The gate recognizes the two wire routes with the same pattern matching the route mux dispatches with, so a percent-encoded spelling such as `/v1/identity/%70roofs` gets the same envelope, deadline and peer confinement as the literal path.

**Deploying E3b.** The hub must be started with `AIMEM_TLS_CERT` and `AIMEM_TLS_KEY` so that its TCP listener terminates TLS itself. Without them, and on the local unix socket, every identity route answers `tls_required`. Clients, including the E3c operator CLI, connect to that listener with certificate verification enabled and never disable it.

**E3c status (operator CLI).** `aimem identity peer list|register|enable|disable` and `aimem identity cred list|issue|rotate|revoke` (`cmd/aimem/identity.go`) drive the admin routes. They cannot use the local unix socket, so they connect to the hub's TLS listener:
- **Hub trust.** The CLI verifies the hub's certificate against the system roots, `--hub-ca-file`, or an explicit `--hub-pin sha256-<SPKI>`. There is no insecure mode and no fallback. The registered peer endpoint's trust binding (`--peer-trust-dns` or `--peer-trust-pin`) is a separate setting.
- **Admin bearer.** It is read only from `--admin-token-file`, which must be private. On Unix that means a regular file owned by the current user with no group or other access. On Windows the CLI checks the file's DACL: only the current user, SYSTEM and Administrators may be able to read it or change its access.
- **Issued bearer.** It is written only to a new `--secret-file`. That file is created exclusively and privately (mode 0600, or a protected owner-only DACL) *before* the request, so an existing path or a missing directory means nothing is issued. The bearer never appears in output.
- **Lost responses.** If the hub confirms a credential but its bearer cannot be written, only that credential is revoked, and the result is reported. An unknown outcome never triggers a reissue or a guessed revocation. The CLI lists the unrevoked credentials that did not exist before the request and the safe next step. It finds them by comparing credential IDs against a snapshot taken before the request, and shows expiry as the hub reports it without judging it against the local clock. Every command it tells the operator to run is complete and runnable. It repeats the hub URL, the path of the admin token file (never the token itself) and the trust option. Each argument is quoted literally for the platform's operator shell: POSIX single quotes on Unix, and PowerShell single quotes on Windows. A path containing quotes, `$`, backquotes or spaces is therefore passed through exactly.
- **Rotation.** `rotate` issues the second credential only. Once aicrew uses it, the operator revokes the old one explicitly.

## E4 implementation boundary

The operator approved splitting E4 into E4a and E4b and decided its open points (E4 task comments seq186 and seq187).

**E4a status.** E4a makes introspection work as a client and closes team mode, without serving any team operation:

- **Outbound credential.** The hub reads the bearer aicrew issued for introspection from the file named by `AIMEM_INTROSPECTION_TOKEN_FILE`, on every call, so a replaced file takes effect on the next one. The file must be private: on Unix a regular file owned by the hub's account with no group or other access, and on Windows a file whose DACL lets only that account, SYSTEM and Administrators read it or change its access. Otherwise it is not used. The bearer never enters the database, state backups, the admin API, responses, logs or the audit. Rotation is: replace the file, confirm with `aimem identity peer check`, then have aicrew revoke the old credential.
- **Client.** `internal/introspect` makes one attempt of at most 2 s per call. It follows no redirect, ignores proxy settings, and verifies the peer's TLS identity against the registered binding: system roots plus the registered DNS name for `ca_dns`, or the leaf's SPKI for `spki_sha256`. It sends the version in both places with a fresh nonce, reads at most 16,384 bytes, refuses unknown fields and trailing data, and applies every §3 check except the profile link, which belongs to the E4b verifier. An inactive reply must carry nothing but its nonce.
- **`introspection_operational`.** The peer listing computes it without a network call. It is true only for the single enabled peer on this hub whose endpoint is the https URL of `/v1/crew/introspect`, with a valid trust binding, while the credential file is usable.
- **Operator check.** `aimem identity peer check SERVICE` (`POST /v1/identity/peers/{service_id}/check`, hub admin, hub TLS only) has the hub send one introspection with a random handle that no session holds. Only a verified inactive answer is healthy. The answer and the audit record (`identity_peer.check.<outcome>`) name a fixed outcome and never the credential or the handle.
- **Team-mode gate.** A request carrying `X-Aimem-Team-Context` is in team mode, whatever the header's value. Every such request is refused before any handler runs and before aicrew is contacted. This covers every route on the TCP listener, `/mcp`, the public pages (which then require a bearer) and the local socket. A missing or unknown bearer gets `invalid_credential`. A malformed or repeated header gets `invalid_request`. A credential other than a live, user-scoped individual token, including the operator on the local socket, gets `credential_scope_forbidden`. Otherwise the answer is `team_operation_unsupported`. No team-mode request is ever served as a personal request. Requests without the header behave exactly as before.

**E4b status.** Team mode serves exactly eight routes (`teamRoutes` in `internal/server/teamcontext.go`): `GET /v1/access/identity` as the context report, the five task reads (list, get, history, comment list, comment get) and the two epic reads (list, get). Every other route keeps `team_operation_unsupported`, sent before aicrew is contacted. On a team route the hub runs §4 in order:

1. It authenticates the individual bearer as usual.
2. It picks the single operational peer; none, or an ambiguous one, is `context_unavailable`.
3. It makes exactly one introspection call per request, bounded by the caller's cancellation and the 2 s budget, and caches nothing.
4. It binds the reply to the caller. A different user, or a reply naming another hub, is `identity_mismatch`. The same user with a different token is `context_stale`, and so are an inactive or expired handle.
5. `(service_id, team_id)` must name an enabled team access profile; otherwise `context_stale`.
6. Every aicrew role may read.

Where a read resolves its project, the hub then evaluates only that profile's live grant on the project's access instance, together with the caller's live user-scoped token. Personal and group grants never count, and a missing grant is `grant_denied`. The context report lists the verified session and the projects the profile currently grants. It says `task_write: false` and `knowledge: unavailable`, and it shows no handle and no other member.

Every verified request is audited as `team.verified`. Every team-mode refusal after authentication is audited as `team.refused.<code>`, including the gate's own `invalid_request`, `credential_scope_forbidden` and `team_operation_unsupported`. The actor is the authenticated user, or `credential:<name>` for any other credential. The subject names the session's service, team, session, generation, role and route when the session is known, and otherwise the request line; it always names the reason and the correlation ID, and never the handle. Each of these refusals carries `active_mode: "team"` and that correlation ID. The local socket's refusals are the exception: their caller is the operator, not an authenticated credential, so they are neither audited nor mode-tagged.

Profiles and profile grants have Go methods in `internal/access` but no route or command yet. The operator surface for them is a separate increment, and no real team can be granted access until it exists. Team mode does not require the hub-terminated TLS that the identity routes do. A hub served over plain HTTP carries the bearer and the handle in the clear, exactly as it already does for the bearer, so production hubs should serve TLS.
