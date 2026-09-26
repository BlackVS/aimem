# AIForge verified identity and session context contract

Status: reviewed contract, implemented in bounded increments. E1 adds only internal identity/profile storage and grant evaluation; no team endpoint, credential, session verification or client behavior is implemented or approved for deployment. Parent boundary: [DESIGN-AIFORGE.md](DESIGN-AIFORGE.md), merged in f47dc5a. Updated 2026-09-25.

## Outcome and boundary

One aimem user ID is the actor for both standalone and aicrew work. An agent makes direct aimem requests with its individual aimem credential. A session selects personal or team context; aimem derives effective access from that one context and rechecks it at the server. Aicrew owns team membership, role, session and work coordination. Aimem owns users, credentials, resource grants and the team access profile to which grants attach.

The first implementation must preserve current standalone behavior. It must not expose legacy writer, admin or checkpoint authority as a fallback for new agent flows. This document fixes security and lifecycle semantics for the next implementation reviews. Route names, transport encoding, persistence layout and rollout sequencing require separate reviewed implementation plans. The knowledge resource matrix and reservation API are separate contracts; neither may weaken the rules here.

## Trust assumptions

Aimem is authoritative for actor authentication, resource grants and task ownership. Aicrew is trusted only for its own membership, role and session facts. A compromised aicrew service could assert false membership within an already granted team profile; aimem must still prevent access outside that profile, arbitrary actor attribution and use after the individual token is revoked. The local OS account and its protected credential store are a trusted boundary, not isolation between agents running as the same OS user. Revocation stops new authorized operations; it cannot erase knowledge already delivered to a client.

## Stable identities and authorities

| Concept | Stable key and owner | Meaning |
| --- | --- | --- |
| Hub | Immutable hub ID issued by aimem | Names the authority that issued user and resource IDs. A URL or local hub alias is a locator, not identity. Older hubs need an explicit ID migration before linking. |
| Actor | (hub ID, aimem user ID), issued by aimem | The only author of an agent-originated aimem action. Display name may change; token rotation does not create a new actor. |
| Credential | Aimem token ID under one user ID | One persistent individual agent secret per installation. Aimem issues, authenticates, expires and revokes it. Aicrew never receives the secret. |
| Team | (aicrew service ID, team ID), issued by aicrew | Membership, role, session generation and coordination authority. Team and member names are labels. |
| Team access profile | Aimem profile ID linked to one aicrew team key | Aimem resource grants attach here. It has no independently editable member list; aicrew is the membership authority. It cannot authenticate as an actor. |
| Resource | (hub ID, resource kind, stable resource ID) | Project instance, knowledge space or other grant target. Names and repository paths select candidates but grant no access. |
| Session | (aicrew service ID, session ID, generation) | One running team context for one linked actor. An opaque session handle is a short-lived session secret, not a shared team bearer or a second persistent agent credential. |

An operator establishes the trusted aicrew service ID and its allowed aimem hub, then links one team key to one aimem access profile. The profile may be granted resources only through ordinary operator-authorized aimem administration. Aicrew may assert membership and role for its own team; it cannot create aimem users, change resource grants, impersonate an actor or expand a credential's scope. The aicrew-to-aimem service credential may redeem proofs and later call only reviewed reservation integration operations. The aimem-to-aicrew service credential may introspect team sessions only. Both are bounded to the registered peer and audited; neither is offered to an agent or used for knowledge access. Their concrete transport and issuance remain a separate implementation review.

## Identity linking and credentials

The proposed proof is a one-time server-verified receipt, avoiding a custom signed token and avoiding disclosure of the aimem bearer to aicrew:

1. Aicrew creates an unpredictable challenge ID and a deadline within five minutes, bound to its service ID, a pending invitation or existing agent record, and the target hub ID. The challenge carries no authority.
2. The agent sends that challenge directly to aimem with its individual aimem credential. Aimem authenticates the token and enabled user, requires a user-scoped token (not a project-scoped, read-only or legacy token), and creates a short-lived single-use proof receipt bound to the challenge, aicrew service ID, hub ID, user ID and token ID. The receipt itself is not a knowledge or task credential.
3. The agent gives the receipt to aicrew. Aicrew redeems it through an authenticated service-to-service call to aimem. Aimem returns the verified IDs, token status and receipt result only to the intended aicrew service. Consumption is atomic. A retry by that same service with the same request key returns the same result; a different caller or changed request is denied.
4. Aicrew links by (hub ID, user ID), never by name, URL, repository or model. Linking an existing aicrew agent to a different aimem user requires an explicit operator-authorized rebind and reconciliation of active work. A duplicate name never silently merges identities.

The agent retains one aimem credential for direct aimem tools. Aicrew may issue a short-lived session handle after proof, but stores and refreshes it automatically in the session's protected local state. The handle cannot authorize an aimem request by itself: aimem still authenticates the individual token and compares its user ID and token ID with the verified team session. Aicrew must not ask the agent to install a broad second knowledge credential. Existing project-scoped tokens remain a compatibility path within their old scope; they do not silently become team credentials.

Proof expiry, bounded request-key retention, transport authentication and secret storage need explicit implementation parameters and tests. A lost proof reply is recovered by asking aimem for another receipt with the same individual credential. A lost redemption reply is reconciled with the original request key; neither failure may create a second actor or link the wrong user.

## Peer verification and delegation limits

Aicrew chooses the challenge ID and its deadline (at most five minutes). Aimem chooses the proof receipt ID and its deadline (at most one minute), stores only a digest of the receipt, and never logs the receipt or bearer. Aicrew first checks that the original challenge is still pending and unexpired. Its redemption call names its registered service ID, that exact challenge and an idempotency key; aimem verifies the receipt binding, audience, receipt expiry and current token state before returning hub/user/token IDs. Aicrew binds the result to its pending invitation or agent record, not an unrequested proof. The concrete wire encoding and service authentication mechanism remain open for the implementation review.

Aicrew's session-introspection answer is derived from its stored verified identity link and current membership/session state. It cannot merely echo IDs supplied by aimem. It includes the linked hub/user/token IDs, team key, role, session ID, generation, active state and expiry. Aimem checks all of them, including that the reply came from the registered aicrew service for the linked team profile. The session handle is random, limited to one actor/team/session, expires within fifteen minutes and is refreshed automatically; renewal retains the same actor and cannot revive a fenced generation. Aicrew never returns a grant decision: aimem computes that from its own current profile grants.

Aicrew's service authority is limited to redeeming its own proof receipts and, after the reservation contract is reviewed, submitting only a reservation transition tied to a verified actor, team session, role, task, attempt generation, operation and retry key. Aimem rechecks those fields and the resource grant and reservation before applying it. The service credential cannot write task content as an agent, call knowledge APIs, administer grants, mint an actor, convert a personal request into team mode or substitute an arbitrary user ID. If the actor's individual credential is revoked, the service cannot continue agent-origin task work under its own identity. Dedicated hub maintenance services remain separate and do not inherit this delegation.

## Selecting and verifying context

Each client conversation or trusted local MCP connection fixes one mode: personal or a verified aicrew team session. The mode is not a mutable machine-wide setting and is not accepted from a tool argument. An agent may have personal and team conversations at the same time; a request belongs to the mode of its own connection. A fresh conversation is recommended when changing modes for context quality, while access enforcement remains server-side.

For every aimem request, including task and knowledge tools, the proposed verifier performs these steps in order:

1. Authenticate the individual aimem credential and recheck user enabled, token expiry, revocation and token scope. Derive actor IDs from authentication, never from request fields.
2. Resolve the request's fixed session mode. Personal mode evaluates only that actor's live direct grants and ordinary access-group memberships, excluding team profiles, plus the existing token cap. Team mode requires the opaque session handle and calls aicrew's authenticated introspection operation for current service ID, hub ID, user ID, token ID, team ID, role, session ID, generation, state and expiry. Aimem accepts no client-supplied member or role assertion as proof.
3. In team mode, require an exact match to the authenticated hub/user/token and the linked team access profile. Recheck the profile's current aimem grant for the requested resource. Evaluate only profile grants; never union them with personal grants or silently substitute a personal credential. Aicrew membership or role alone grants no aimem resource.
4. Apply the operation's resource and role policy, then task reservation/fence checks where applicable. The same resolved context must reach HTTP, MCP, local adapter, background capture, recall, docs, records, journals and task paths. A route added later starts denied until mapped to this verifier and the knowledge matrix.
5. Attribute the action to the authenticated aimem user ID, with the verified context and session reference in audit metadata. A service integration may submit a bounded reservation request, but cannot supply an arbitrary actor label or bypass a user/session fence.

For the first pilot, team introspection is online on every authorized team operation; aimem does not cache an allow decision. Aicrew unavailability or a stale reply fails closed for team operations, with a retryable context-unavailable response. This favors revocation correctness over availability. Personal mode does not depend on aicrew. An implementation may optimize this only under a separately reviewed freshness and revocation contract. Aimem must authenticate the aicrew endpoint and bind its reply to the request; a handle alone is not an authority.

The local daemon either runs one MCP process per conversation with a fixed protected context or binds a shared process's context to an authenticated connection. It may never use a global current-team variable, trust an LLM tool argument as the selector, or route a failed team request through a broad legacy token. Credential, session handle, cached knowledge and pending writes are separated by hub ID, actor ID and mode/session key. Offline cached reads are marked as cached, not fresh authorization; replay of writes rechecks current grants and context.

## Role and action matrix

| Mode / role | Knowledge | Task and coordination action |
| --- | --- | --- |
| Personal standalone | Personal grants and token cap only | Ordinary task operations subject to the authoritative aimem reservation and project process. No aicrew membership is required. |
| Team coordinator | Team profile grants only | Plans/offers/reviews through aicrew. Aimem task writes require the reservation contract and resource grant; role is not aimem administration. |
| Team worker | Team profile grants only | Works only on its accepted, generation-fenced attempt. Direct aimem claim/edit cannot bypass an offer or reservation. |
| Team independent worker | Team profile grants only | May claim eligible unreserved work atomically through aimem's reservation contract. Cannot manage aicrew membership or grants. |
| Aicrew service | No agent knowledge access | Only explicitly bounded proof verification and reservation integration operations; no arbitrary actor selection. |

All team roles remain subject to the project's review and human-merge gates. Detailed read/contribute permissions for memories, raw journals, docs, records, shared groups and sync belong to the knowledge access matrix. Until that matrix and the scoped knowledge route are implemented and tested, a team context must report knowledge access unavailable rather than use legacy broad authority.

## Lifecycle, revocation and recovery

| Event | Required result |
| --- | --- |
| Aimem token expires or is revoked, or user is disabled | New aimem operations fail immediately under aimem's live check. Team context cannot keep working through aicrew's cached identity. Existing work remains reserved for explicit reconciliation. |
| Aicrew membership, role, session or generation changes | The next team operation sees the current introspection result. Old handles or generations are refused. A role downgrade cannot retain a prior write permission. |
| Team profile grant or service trust is removed | Aimem denies the next affected team operation, even if aicrew still reports an active member. Personal grants do not fill the gap. |
| Individual credential rotates | Aimem issues a new token for the same user ID and revokes the old one according to the reviewed rotation procedure. Aicrew re-proves the same identity, rebinds the session to the new token ID and advances its generation. Old handles are fenced; reservations are reconciled, not released silently. |
| Team session disconnects or aicrew is unavailable | Running work stays reserved. Requests fail closed with a retryable response; no automatic switch to personal mode. Resume first revalidates identity, context, generation and pending operation receipts. |
| Leave or switch with outstanding work | Aicrew refuses clean leave until accepted/blocked/stop-pending work is reconciled. Aimem does not infer personal authority from a leave request. After confirmed leave, a separate personal conversation may use personal grants. |
| Simultaneous personal and team sessions | They are isolated by connection/session key. Personal operations cannot take a team-held reservation; team operations never acquire personal grants. The task reservation contract resolves collisions across modes. |

Revocation takes effect for an operation whose authorization check starts after the revocation commits at its owning service. An operation already authorized may finish; the two services do not share one transaction. Mutations must revalidate the aimem token/profile and the aicrew session generation immediately before their own commit. The separate reservation contract must fence task writes across a concurrent stop or role change; this document does not promise retroactive cancellation of an in-flight read.

Rotation is a new credential for the same actor, not a new identity. Aicrew links and historical attribution survive token and display-name changes. If the client loses its individual credential, recovery requires an authorized reissue path; neither a session handle nor a service credential is a fallback. Local restart restores only protected session state, then verifies it online before reporting ready. Unknown, expired and incomplete context reports are blocked, not treated as personal mode.

## Failure contract

HTTP and MCP must expose the same failure envelope: stable code, short explanation, the caller's active mode when safe to reveal, retryable flag, one permitted next action and a correlation ID. The implementation review fixes exact JSON spelling and HTTP status mapping in OpenAPI; no response may contain a bearer, proof receipt, session handle or private membership of another actor. A task refusal never tells an assigned worker to retry using personal credentials.

| Code | Meaning | Next action |
| --- | --- | --- |
| invalid_credential | Missing, expired or revoked individual aimem credential | Renew or recover the individual credential through the authorized flow. |
| identity_link_required | Valid aimem actor has no verified aicrew link for requested team work | Complete the proof flow; do not invent a name match. |
| identity_mismatch | Proof/session hub, user or token does not match authentication | Stop and reconcile the configured identity; no automatic rebind. |
| context_missing | Team action has no verified session binding | Start or resume the aicrew session in this conversation. |
| context_stale | Session expired, generation changed or role/membership revoked | Revalidate with aicrew; reconcile pending work before retry. |
| context_unavailable | Aicrew verification cannot be reached or trusted | Retry verification later; team operation was not authorized or applied. |
| team_operation_unsupported | The operation is not served in team mode; the hub refuses it before contacting aicrew | Use the aicrew flow for this work; team mode does not serve this operation. |
| grant_denied | Active context lacks a current aimem resource grant | Request an authorized grant change; do not switch credentials. |
| role_forbidden | Verified role cannot perform this operation | Use the role's permitted aicrew flow. |
| work_outstanding | Leave/rotation transition has unreconciled attempt or request | Reconcile the named attempt through aicrew before switching. |
| reservation_conflict | Task is held by another fenced owner or the revision changed | Read the current task/reservation and follow the coordination flow. |

A response may identify the caller's active mode, project and own session/attempt reference, but not a secret. Lost replies are resolved through idempotent status/receipt reads before a new mutation. Server denials and client notices must distinguish denied, unavailable, stale and cached state.

## Verification gates and implementation split

The design is accepted only when reviewers can trace each allowed operation to authenticated actor, one selected context, a live resource grant and any required reservation. Before implementation, settle endpoint schemas, service authentication, proof retention, timeout behavior, storage migrations and the knowledge matrix in reviewed increments. Proposed bounded implementation slices are:

1. Stable hub/profile IDs and one-time identity proof with a fake aicrew verifier. Prove the original challenge ID survives issue and redemption, plus expiry, concurrent redemption, lost replies, wrong audience and token rotation without exposing the aimem bearer.
2. Aimem team-context verification with a fake aicrew introspection service. Prove no grant union, user/token mismatch, revocation, role downgrade, outage denial and simultaneous sessions through real HTTP/MCP routes.
3. Trusted local session binding and restart/recovery. Prove two local conversations cannot swap context, no broad fallback occurs, and stale state blocks until verified.
4. Integrate the separately reviewed knowledge matrix and task reservation contract, then a first-pilot end-to-end flow. Do not infer those approvals from this document.

Current source basis at f47dc5a: internal/access/store.go holds user IDs, grants, token IDs, expiry and revocation; internal/server/server.go limits ordinary tokens to task/identity routes; internal/taskcred/taskcred.go selects checkout or user-hub credentials; internal/teamstate/teamstate.go binds legacy state to checkout, hub URL and token ID; internal/store/tasks.go protects managed tasks. The current CanWriteToken path requires a personal user or access-group grant, so it cannot authorize team profile grants unchanged. These are the gaps the proposed contract addresses, not evidence that the new protocol already runs. No existing credential or live team state is changed by this design document.

### E1 internal storage boundary

Access schema 3 adds one immutable hub ID in the hub's `access.db`, a team access profile linked by immutable `(service ID, team ID)`, and profile grants keyed by the existing stable project access ID. The project ID moves with a rename; a deleted and recreated project gets a different ID. Profile records have no member list and cannot authenticate. The profile administration and team grant evaluator remain package-private until the peer, actor, role and selected session can be verified by later increments. Test-supplied IDs demonstrate ledger behavior only; they do not prove production authority. Existing `CanWriteToken` and HTTP/MCP task and knowledge routes continue to use standalone grants and never consult profile grants.

Schema 2 data is migrated additively in one access-store transaction. Existing users, groups, grants, tokens, secrets and project databases are preserved; new profile tables start empty and give no access by default. Schema 1 first migrates token scope as before, then adds schema 3. Back up the state root before upgrading. Older binaries reject schema 3, so rolling back the binary alone is not supported; rollback requires restoring a pre-upgrade `access.db` backup together with the corresponding hub state. Do not manually lower `user_version` or assume an old binary will preserve profile isolation. E1 does not issue credentials, establish a peer, or enable a team operation. E2–E5 must supply and test those boundaries before C5 consumes them.
