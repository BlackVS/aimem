# AIForge scoped knowledge access for the same agent context

Status: reviewed decisions for task 19a0 (01a0d6d7-19a0). The operator approved K1 to K10 on the task's scope (comments seq229 and seq231). **Nothing here is implemented yet**: task 19d8 (01a0d6d7-19d8) implements the first-pilot set in [The pilot set](#the-pilot-set-for-19d8), and every other row of the matrix stays as it is today. This document is the knowledge matrix the [context contract](DESIGN-AIFORGE-CONTEXT.md) defers to ("Detailed read/contribute permissions … belong to the knowledge access matrix"). If the context contract disagrees with this document, the context contract wins.

## Outcome and boundary

One agent context reads the knowledge its **effective access profile** is granted, through the tools and routes aimem already has. There is no second agent credential and no duplicate interface. The first pilot (one coordinator, one worker, one project) gets a small read-only set. Everything else stays refused for scoped callers, and legacy behavior is unchanged. This is not a general RBAC redesign: it adds one read check to three existing routes.

## Identity and effective profile

The hub authenticates exactly one **identity** per request. The **effective profile** is whose grants decide.

| Identity | How it is authenticated | Effective profile |
| --- | --- | --- |
| Legacy registry or env token | `writer` / `admin` bearer | None: legacy broad authority (unchanged) |
| Ordinary individual token, personal | `aimem_user_` bearer, no team header | The user's own grants (direct or through an access group), capped by the token's scope |
| Ordinary individual token, team mode | The same bearer plus `X-Aimem-Team-Context`, verified online through aicrew on every request (E4) | The linked team access profile's grants only, never the user's personal grants |
| Peer credential | `aimem_peer_` bearer | No agent knowledge access |

The authenticated identity attributes a request. It never widens the effective profile: a team context cannot fall back to the user's personal grants, and a personal request cannot use a profile's grants.

## Inventory and matrix

Each cell says what a caller gets **after 19d8**. ✅ allowed · 🟡 the pilot set, allowed under the scoped read check · ⛔ refused (code in [Refusals](#refusals)).

| Feature | Tools / routes today | Sensitivity | Legacy writer/admin | Personal user token | Team context |
| --- | --- | --- | --- | --- | --- |
| Recall | `recall_memory` → `GET /v1/projects/{p}/memories/recall` | Curated, distilled memories | ✅ | 🟡 project scope only | 🟡 project scope only |
| Memory list, review queue | `GET …/memories`, `…/memories/review`, `review_memories` | Curated, including unconfirmed | ✅ | ⛔ | ⛔ |
| Memory writes | `remember`, confirm, forget, supersede, pin, tag, link, import | Contributes to shared knowledge | ✅ | ⛔ | ⛔ |
| Docs / wiki: list and read | `list_docs`, `read_doc` → `GET /v1/projects/{p}/docs`, `…/docs/{name}` | Shared, often design material | ✅ | 🟡 the project's own docs | 🟡 the project's own docs |
| Docs: history, write, merge, delete | `…/docs/{name}/log`, `update_doc`, `PUT`, `…/merge`, `DELETE` | Shared publishing | ✅ | ⛔ | ⛔ |
| Group design doc | `get_design_doc` → `…/meta/design_doc` on a `group-*` project | Shared space | ✅ | ⛔ | ⛔ |
| Records / collections | `list_records`, `get_record`, `put_record`, collection routes | Structured, shared | ✅ | ⛔ | ⛔ |
| Raw journal read | `search_journal`; `…/sessions`, `…/timeline`, `…/latest`, `…/search` | **Raw**: prompts, tool output; secrets redacted best-effort | ✅ | ⛔ | ⛔ |
| Journal capture | Submit hooks → `POST /v1/events` | **Raw** | ✅ (local and hub push) | ⛔ | ⛔ (E5b: team conversations capture nothing) |
| Injected context | The session-start hook: handoff, recall, process | Mixed | ✅ | Unchanged local behavior | ⛔ (E5b: only the checkout's `docs/SESSION-STATE.md`) |
| Personal user store | Recall scope `user` (project `user`) | Personal | ✅ | ⛔ in v1 | ⛔ |
| Group spaces | `group:` scopes, `group-*` projects, `.aimem.json` `groups` | Shared spaces | ✅ | ⛔ | ⛔ |
| Sync / curation | `/v1/sync/*`, curate imports, retention, chapters | Infrastructure | ✅ | ⛔ (not agent-facing) | ⛔ |
| Meta, audit, overview | `…/meta/{key}`, `…/audit`, `/v1/overview` | Operational | ✅ | ⛔ | ⛔ |

## The pilot set (for 19d8)

**Decision K1(a): read-only and project-scoped.** Three existing routes, and the three tools that call them. No new route or tool (K3).

| Tool | Route | Arguments a scoped caller may use |
| --- | --- | --- |
| `recall_memory` | `GET /v1/projects/{p}/memories/recall` | `query`, `token_budget`, `tag`, `kind`; the tool's `scope` must be `project` or absent |
| `list_docs` | `GET /v1/projects/{p}/docs` | the project scope only |
| `read_doc` | `GET /v1/projects/{p}/docs/{name}` | the project scope only |

**The scoped read check (K2).** For an ordinary user token, personal or team, each of the three routes checks, on every request:
1. **The project is ordinary.** `{p}` is not a reserved project: not `user`, the personal store, and not `group-*`, a group space. A reserved project is refused whatever grant exists (K4, K6).
2. **The live grant of the effective profile:**
   - **Team mode.** The verified context's profile has a live grant on the project's access instance, and the individual token is live. This is the same `TeamGrantAllows` check E4 applies to task reads. Personal and access-group grants never count.
   - **Personal.** The token is live, from an enabled user, and scoped to the user or to this project. The user holds a live direct or access-group grant on the project's access instance: `CanWriteToken`'s membership predicate, used here for a read. Legacy `read-only`-scope tokens get no knowledge in v1.
3. **Answer, or refuse.** The answer is the route's existing response. Recall is bounded by its existing token budget.

Legacy writer and admin tokens keep today's behavior on the same routes (K9).

**Where the tools run.**
- **In a team conversation** (`AIMEM_TEAM_SESSION`, E5a), the MCP lists the three tools next to the task reads and `session_context`. It calls the hub routes with the pinned context header, never a local answer. `recall_memory` refuses any scope other than `project`.
- **On the hub's `/mcp` for an ordinary user token**, the three tools are no longer hidden (`tasksOnly`). They call the scoped routes with the caller's own identity, never the hub's trusted local client.
- **The local stdio facade in personal mode** is unchanged.

"Local group declarations cannot authorize extra access": a project's `.aimem.json` `groups` list selects candidate group spaces for the legacy facade only. It is never an authorization for a scoped caller, and a `group:` scope from a scoped caller is refused before any hub call (K4).

## Cache, offline and revocation (K7)

- **Team mode keeps no knowledge cache.** Every read goes to the hub online, with the verified context, and the hub rechecks the live grant. A revoked token, an ended or changed session, a disabled profile or a removed grant stops the **next** read.
- **Offline means no knowledge in team mode.** The MCP refuses. It never answers from a local copy as if it were authorized.
- **Knowledge already delivered cannot be recalled.** This is the context contract's trust assumption: revocation stops new reads only.
- **Personal and legacy.** The local daemon's cache and offline behavior is unchanged. A personal user token's reads through the hub are online and grant-checked in the same way.

## Raw journal and shared-space publishing (K5, K4)

- **The raw journal is never readable by a scoped caller** in v1: `search_journal`, sessions, timeline, latest and search are refused. **Capture stays off in team conversations** (E5b). A redacted journal view would be a later decision.
- **No scoped caller publishes into a shared space:** the pilot set has no write. Doc, record and memory writes, group design docs and group spaces stay legacy-only.

## Audit (K8)

Knowledge reads are not audited per call, as today. A refusal is logged with its code. In team mode, a refusal is also audited as `team.refused.<code>` (E4), with the denied action and role (19f6). The pilot set has no writes, so there is no write attribution to add.

## Compatibility (K9)

Legacy registry and env tokens, the local socket, the personal stdio facade, the hooks, the local daemon and sync behave exactly as before. Project-scoped ordinary tokens keep their one project, and never become team credentials. An ordinary user token gains only the pilot set's reads, under its own grants.

## Sync and curation (K10)

Sync and curation are hub-internal writer paths (`/v1/sync/*`, curate imports, retention, chapters). They are not agent-facing and not integrated with team profiles. They stay refused for scoped callers, and nothing in them reads a profile's grants.

## Refusals

| Case | Personal user token | Team context |
| --- | --- | --- |
| A knowledge route or tool outside the pilot set | Today's ordinary-token refusal (403) | `team_operation_unsupported` (403), before aicrew is asked, with `denied_action` |
| A reserved project (`user`, `group-*`) | 400, reserved scope (as the task routes) | `grant_denied` (403): no profile is ever granted a reserved project |
| No live grant on the project | 403 | `grant_denied` (403), with `active_role` |
| A `group:` scope or recall scope other than `project` | Refused by the tool, no hub call | Refused by the tool, no hub call |
| Team context stale, missing or unavailable | n/a | The context contract's codes (`context_stale`, `context_missing`, `context_unavailable`) |

No refusal names another project's contents or another actor. No team-mode next action advises another credential (19f6).

## Delivery

- **19a0 (this document)** is the decided matrix.
- **19d8** implements the pilot set, with tests for:
  - each of the three routes in personal and team mode: allowed with a grant, refused without one, refused on `user` and `group-*`;
  - revocation and profile disable taking effect on the next read;
  - a team conversation's MCP listing exactly the task reads, the three tools and `session_context`;
  - `recall_memory` refusing any non-`project` scope;
  - the hub's `/mcp` serving the three tools to a user token under its own grants, and nothing else;
  - legacy writer behavior unchanged;
  - seeded faults for the grant check, the reserved-project check and a personal-grant fallback in team mode.
- **Beyond the pilot, each needs its own decision:** memory contribution, group grants on profiles, a redacted journal view, records, and the personal store in personal mode.
