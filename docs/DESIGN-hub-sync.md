# Anti-entropy sync over the hub API, and named tokens

Status: **implemented** 2026-08-29, same day as drafted (user approved
the build). Corrections found during implementation, per protocol:

- The OpenAPI spec is **JSON, not YAML**, and lives embedded at
  `internal/server/openapi.json`: both the served endpoint
  (`/v1/openapi.json`) and the parity test want a format the stdlib
  parses, and a YAML dependency for one file was backwards.
- The role split gained two admin routes the draft did not enumerate:
  `POST /v1/projects/{p}/retention` (destructive) and the
  chapter-proposal pair (they drive an LLM with the operator's spend).
- No dedicated probe request: the FIRST sync call returning 404 is the
  probe; `syncHub` then falls back to ssh or says to upgrade the hub.
- The server keeps a 30s **header** timeout but widens body timeouts to
  15 minutes (open question 3 resolved: a first sync legitimately
  streams for minutes; slowloris is a header-phase attack).
- The route table (`Server.Routes()`) became the single source of
  truth: the mux, the admin gating, AND the OpenAPI parity test are all
  derived from it, so a route cannot exist ungated or undocumented.

Companion to DESIGN.md.

## The problem

Real-time capture rides one clean channel: HTTPS to the hub, bearer
token, TLS, per-hub spool. Anti-entropy — the leg that actually
*distributes* knowledge — rides a completely different one: six
`bash -c` shell pipelines over ssh (`syncOne`, `cmd/aimem/main.go`),
each assuming a remote shell account, an authorized key for the service
user, a remote binary path (`AIMEM_REMOTE_BIN`), and quoting that
survives two shells. The consequences are not hypothetical; every one
of these happened in this deployment's first week:

- **Windows machines never receive knowledge.** `enable-sync` is
  Linux-only, so a Windows workstation pushes events up but has no pull
  path at all: curated facts live on the hub, and local recall on that
  machine sees only what it created itself. "Shared memory across
  machines" is currently a Linux feature.
- **A second credential system.** The hub already has bearer auth, yet
  sync needs ssh keys authorized for the service account — provisioned
  per machine, interacting with system-wide ssh config (a stray
  `PKCS11Provider` broke every sync on one machine), and granting far
  more than sync needs: a shell as the user that owns every project's
  memory.
- **Fragile transport.** Two shells' worth of quoting, base64 lessons
  learned the hard way, `$HOME` expansion on the right side only, and
  best-effort legs that must tolerate old remote binaries.

## What already exists

Everything except the routes. The primitives are CLI subcommands
(`export-events --since` / `import-events`, `export-memories` /
`import-memories`, `export-group-config` / `import-group-config`) that
already stream JSONL and import idempotently; per-peer cursors with the
1-hour `ShiftBack` overlap; the `hubProjects` partition (bound projects
+ the `user` DB + declared groups); merge semantics (staleness-wins for
memories, fill-only for config). The hub speaks authenticated HTTPS and
`import-events` already lands via `SubmitLocal` so imports never
re-broadcast (v0.1.74).

## Proposal

### 1. Four sync routes on the hub

```
GET  /v1/sync/events?since=<cursor>&projects=a,b     JSONL stream out
POST /v1/sync/events                                  JSONL stream in
GET  /v1/sync/memories?projects=a,b    POST /v1/sync/memories
GET  /v1/sync/group-config?projects=…  POST /v1/sync/group-config
```

Thin wrappers over the same store primitives the CLI subcommands call —
no new merge logic, no new formats. Streamed line by line with the
existing per-record size caps; POST responds with counts (imported,
skipped) so the client can report like today's sync does.

One asymmetry is new and must be explicit: over ssh the pull legs are
unfiltered because "the peer only holds its own". A hub holds *many*
machines' projects, so **the pull legs carry the same `projects` filter
as the push legs** — this machine's bound projects, `user`, and its
declared groups. That is the recall rule applied to sync, and it is
what keeps a stolen writer token (below) from vacuuming the whole hub
partition by accident. Doc bodies do NOT ride these routes: shared
documents have their own CAS path, and sync semantics would destroy
them (DESIGN-shared-docs, "not a second transport").

### 2. `aimem sync` selects the transport

A hub entry with a URL and token syncs over HTTPS; `--sync <ssh-dest>`
remains as the legacy fallback, selected only when the hub predates the
routes (probe: 404 on `/v1/sync/events` → fall back, warn). Cursors
move from ssh-dest-keyed files to hub-name-keyed ones (same directory,
same `ShiftBack` overlap; a one-time cursor reset costs one idempotent
full overlap, nothing else). `--all-hubs` behaves identically.

Windows gets `aimem-sync` as a scheduled task from `install.ps1`, the
same cadence as the systemd timer. That closes the platform gap: facts
reach every machine, not every Linux machine.

### 3. Named tokens

One `AIMEM_HTTP_TOKEN` is currently the entire trust model: every
writer is indistinguishable, `updated_by` is explicitly advisory, and
revoking one machine means re-keying the fleet. With sync joining the
same channel, the token becomes the only credential — worth making it
plural first.

`<state-root>/tokens.json` on the hub (0600, host-local, never synced —
same posture as `providers.json`):

```json
{"tokens": [
  {"name": "dmbunker", "role": "writer", "sha256": "<hex>"},
  {"name": "ops",      "role": "admin",  "sha256": "<hex>"}
]}
```

- Tokens are stored **hashed**; comparison hashes the presented token
  and constant-time-compares digests. `AIMEM_HTTP_TOKEN` keeps working
  as an implicit `admin` named `env` — zero-migration back-compat.
- Two roles only in v1. **writer**: events, sync, recall/search, docs
  read+write, MCP. **admin**: everything writers have plus config,
  providers, rename, drop, refile, logs. The split follows what a
  workstation actually needs versus what only an operator should hold.
- **Attribution becomes real.** The server stamps the token name into
  what it already records: `updated_by` on doc writes becomes
  `<token-name>/<client-supplied>`, hub log lines carry the name, and
  "who overwrote my handoff" is answered by authentication instead of
  honor. The COMPARISON non-goal ("not attribution you can trust
  adversarially") flips to a feature.
- CLI on the hub: `aimem token add <name> [--role writer]` (secret
  printed once, only the hash lands on disk), `aimem token list`,
  `aimem token rm <name>` (revocation = deleting one line, not
  re-keying a fleet). Console management was considered and DEFERRED
  (decided with the user 2026-08-29): minting happens once per machine
  at client-install time — the installer already takes the secret as
  env — and keeping creation CLI-only preserves the strongest property
  here: credentials can only be minted with shell access to the hub, so
  a leaked bearer can never self-perpetuate. Revisit if the deployment
  gains operators who onboard machines without hub ssh; a list/revoke
  view (nothing secret to leak) would be the safe first slice.

### 4. Deployment order

1. Hub release with routes + tokens file (both dormant until used).
2. Clients switch transport on their next update; ssh path warns it is
   deprecated when taken.
3. Fleet migrated → `install-hub.sh` stops authorizing sync ssh keys;
   one release later the ssh legs are removed.

## Non-goals

- **Not peer-to-peer.** Workstations sync with hubs only; two machines
  converge through their shared hub. (Today's ssh path is nominally
  peer-agnostic but is only ever pointed at hubs.)
- **Not mTLS or a PKI.** Bearer over TLS stays; named tokens fix
  identity and revocation, which is what actually hurt.
- **Not per-project ACLs** in v1 (open question 2).
- **Not a change to real-time push**, spools, or docs — those channels
  already work and stay as they are.

## Open questions

1. Should a `writer` pull group DBs it has not declared? Leaning no —
   the declared-groups rule is the sharing consent everywhere else.
2. Per-project token scopes (`"projects": ["a", "b"]` on a token) —
   deferred until a deployment actually shares a hub across trust
   domains; the partition between hubs covers today's cases.
3. Rate/size limits on the sync POSTs beyond the per-record caps — the
   30s server timeouts may need a carve-out for large first syncs.
4. Does `sync` still run the post-sync embed backfill? Yes — that
   behavior is transport-independent and stays.

## OpenAPI (decided with the user, 2026-08-29)

The full `/v1` surface gets a **hand-written `openapi.yaml`** in the
repo — not generated: the server is framework-free stdlib and comment-
driven codegen would add a dependency and noise for no accuracy gain.
What keeps it accurate instead is a **parity test** in
`internal/server`: every path in the spec must resolve in the real mux
and every mux route must appear in the spec, so drift is a CI failure
(the same pin-docs-to-reality move as `TestAdminPageScriptParses`).
The spec is served at `GET /v1/openapi.json` **behind the bearer
token** — the three-route unauthenticated surface is an invariant —
and no Swagger UI is embedded (the console is the human interface;
external viewers render the file fine). Sequencing: written after the
sync routes and named tokens land, so per-route roles are in it from
the first version.

## Why this is worth building

It finishes the product's own sentence. The hub is already the
rendezvous, already authenticated, already reachable from every
platform; sync is the one leg still pretending it is 2005. Moving it
onto the hub API gives Windows machines knowledge parity, deletes a
whole credential class and its incident surface, replaces six quoted
shell pipelines with four routes over primitives that already exist —
and named tokens make every write on that channel attributable and
individually revocable.
