<!-- GENERATED from hub collection "api" — do not edit; regenerate with `aimem col render api` -->

# api

## admin

### get

- **method**: GET
- **path**: /admin
- **role**: public
- **summary**: Admin console shell (HTML, holds no data; asks for the token in the browser)

## config

### models

#### get

- **method**: GET
- **path**: /v1/config/models
- **role**: admin
- **summary**: Curate/embed model configuration

#### put

- **method**: PUT
- **path**: /v1/config/models
- **role**: admin
- **summary**: Update model configuration (atomic env rewrite)

### providers

#### get

- **method**: GET
- **path**: /v1/config/providers
- **role**: admin
- **summary**: Provider registry, tokens masked to last 4

#### models

##### get

- **method**: GET
- **path**: /v1/config/providers/{name}/models
- **role**: admin
- **summary**: Proxy the provider's model list (token stays server-side)

#### put

- **method**: PUT
- **path**: /v1/config/providers
- **role**: admin
- **summary**: Update providers/bindings (blank token keeps stored)

#### test

##### post

- **method**: POST
- **path**: /v1/config/providers/test
- **role**: admin
- **summary**: Live probe of one model through its resolved provider (server-side; nothing secret reaches the caller)

## events

### post

- **method**: POST
- **path**: /v1/events
- **role**: writer
- **summary**: Append one checkpoint event (idempotent on idempotency_key); groups and hub binding ride along into project meta

## health

### get

- **method**: GET
- **path**: /v1/health
- **role**: writer
- **summary**: Service health: project count, state root, version, resources

## logs

### get

- **method**: GET
- **path**: /v1/logs
- **role**: admin
- **summary**: Service log ring buffer

## openapi.json

### get

- **method**: GET
- **path**: /v1/openapi.json
- **role**: writer
- **summary**: This specification

## overview

### get

- **method**: GET
- **path**: /v1/overview
- **role**: writer
- **summary**: Per-project stats for dashboards

## projects

### audit

#### get

- **method**: GET
- **path**: /v1/projects/{p}/audit
- **role**: writer
- **summary**: Knowledge audit trail, newest first

### chapter-proposal

#### apply

##### post

- **method**: POST
- **path**: /v1/projects/{p}/chapter-proposal/apply
- **role**: admin
- **summary**: Apply the human-approved subset of a chapter proposal

#### post

- **method**: POST
- **path**: /v1/projects/{p}/chapter-proposal
- **role**: admin
- **summary**: LLM-propose chapter filings for unfiled facts (plan lands in meta for review)

### collections

#### get

- **method**: GET
- **path**: /v1/projects/{p}/collections
- **role**: writer
- **summary**: List structured collections (name, live record count, last update)

#### records

##### delete

- **method**: DELETE
- **path**: /v1/projects/{p}/collections/{c}/records/{id...}
- **role**: writer
- **summary**: Tombstone one record (?base_rev=&by=; CAS like any write)

##### get

- **method**: GET
- **path**: /v1/projects/{p}/collections/{c}/records
- **role**: writer
- **summary**: List a collection's records in id (= tree) order; ?bodies=1 includes bodies (the render fetch)

##### get-one

- **method**: GET
- **path**: /v1/projects/{p}/collections/{c}/records/{id...}
- **role**: writer
- **summary**: Read one record (current, or ?rev= for a retained revision); id is a slash path like api/messages/create

##### put

- **method**: PUT
- **path**: /v1/projects/{p}/collections/{c}/records/{id...}
- **role**: writer
- **summary**: Compare-and-swap write of one record ({body: <JSON object>, base_rev, updated_by}); the CAS unit is the record, so disjoint edits never conflict

### curate-runs

#### get

- **method**: GET
- **path**: /v1/projects/{p}/curate-runs
- **role**: writer
- **summary**: Curation run history (the token/cost meter)

#### import

##### post

- **method**: POST
- **path**: /v1/projects/{p}/curate-runs/import
- **role**: writer
- **summary**: Import one curation-run record (id-idempotent)

### delete

- **method**: DELETE
- **path**: /v1/projects/{p}
- **role**: admin
- **summary**: Drop a project (files deleted; a read never resurrects it)

### docs

#### delete

- **method**: DELETE
- **path**: /v1/projects/{p}/docs/{name}
- **role**: writer
- **summary**: Tombstone a document (?base_rev=&by=; CAS like any write)

#### get

- **method**: GET
- **path**: /v1/projects/{p}/docs
- **role**: writer
- **summary**: List shared documents (name, rev, writer; tombstones flagged)

#### get-one

- **method**: GET
- **path**: /v1/projects/{p}/docs/{name}
- **role**: writer
- **summary**: Read a shared document (current, or ?rev= for a retained revision)

#### log

##### get

- **method**: GET
- **path**: /v1/projects/{p}/docs/{name}/log
- **role**: writer
- **summary**: Recent revisions of a shared document (bounded history)

#### merge

##### post

- **method**: POST
- **path**: /v1/projects/{p}/docs/{name}/merge
- **role**: writer
- **summary**: Compute-only three-way merge: {body, base_rev} against the current document -> {merged, conflicts, against_rev, base_found}. The hub never merges on write; saving the result is an ordinary CAS PUT

#### put

- **method**: PUT
- **path**: /v1/projects/{p}/docs/{name}
- **role**: writer
- **summary**: Compare-and-swap write ({body, base_rev, updated_by}); named tokens prefix updated_by; identical body is a no-op

### get

- **method**: GET
- **path**: /v1/projects
- **role**: writer
- **summary**: List project ids

### memories

#### confirm

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/{id}/confirm
- **role**: writer
- **summary**: Record a review verdict of still-true: audited touch (drops the fact from the queue) plus a modest confidence reinforcement

#### forget

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/{id}/forget
- **role**: writer
- **summary**: Expire a memory (bi-temporal; refused if pinned)

#### get

- **method**: GET
- **path**: /v1/projects/{p}/memories
- **role**: writer
- **summary**: List memories (?all=1 includes expired)

#### import

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/import
- **role**: writer
- **summary**: Merge one synced memory (union, staleness-wins; idempotent)

#### link

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/{id}/link
- **role**: writer
- **summary**: Relate two memories

#### pin

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/{id}/pin
- **role**: writer
- **summary**: Pin or unpin (pinned facts are protected and rank first)

#### post

- **method**: POST
- **path**: /v1/projects/{p}/memories
- **role**: writer
- **summary**: Remember a fact through the audited write path (reassert folds into an identical active fact)

#### recall

##### get

- **method**: GET
- **path**: /v1/projects/{p}/memories/recall
- **role**: writer
- **summary**: Hybrid recall: BM25 + cosine fused by RRF, pinned first, token-budget trimmed; query embedding is server-side and fail-open

#### review

##### get

- **method**: GET
- **path**: /v1/projects/{p}/memories/review
- **role**: writer
- **summary**: Staleness review queue: active unpinned facts whose last assertion/validation predates ?days= with corroboration <= ?max_corroboration= (derived, never stored)

#### supersede

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/{id}/supersede
- **role**: writer
- **summary**: Replace a fact keeping lineage (old row expires and points at the new)

#### tag

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/{id}/tag
- **role**: writer
- **summary**: Attach a tag (explicit filing path; chapters capped at 3 per fact)

#### untag

##### post

- **method**: POST
- **path**: /v1/projects/{p}/memories/{id}/untag
- **role**: writer
- **summary**: Detach a tag (the correction path for a mis-filed fact)

### meta

#### get

- **method**: GET
- **path**: /v1/projects/{p}/meta/{key}
- **role**: writer
- **summary**: Read one allow-listed project meta key

#### put

- **method**: PUT
- **path**: /v1/projects/{p}/meta/{key}
- **role**: writer
- **summary**: Write one allow-listed project meta key

### rename

#### post

- **method**: POST
- **path**: /v1/projects/{p}/rename
- **role**: admin
- **summary**: Rename a project: moves journal, facts, embeddings, curate cursor; relabels project:<id> citations in group DBs; refuses reserved ids and existing targets

### retention

#### post

- **method**: POST
- **path**: /v1/projects/{p}/retention
- **role**: admin
- **summary**: Trim old journal events

### search

#### get

- **method**: GET
- **path**: /v1/projects/{p}/search
- **role**: writer
- **summary**: FTS5 search over the journal (?q=)

### sessions

#### get

- **method**: GET
- **path**: /v1/projects/{p}/sessions
- **role**: writer
- **summary**: Sessions seen in a project

#### latest

##### get

- **method**: GET
- **path**: /v1/projects/{p}/sessions/{s}/latest
- **role**: writer
- **summary**: Most recent event of one session

#### timeline

##### get

- **method**: GET
- **path**: /v1/projects/{p}/sessions/{s}/timeline
- **role**: writer
- **summary**: Events of one session, oldest first

## root

### get

- **method**: GET
- **path**: /
- **role**: public
- **summary**: Public status page (HTML)

## status

### get

- **method**: GET
- **path**: /v1/status
- **role**: public
- **summary**: Public status JSON

## sync

### events

#### get

- **method**: GET
- **path**: /v1/sync/events
- **role**: writer
- **summary**: Anti-entropy pull: JSONL event stream (?since=<cursor>&projects=a,b); opens existing projects only

#### post

- **method**: POST
- **path**: /v1/sync/events
- **role**: writer
- **summary**: Anti-entropy push: JSONL event stream in (idempotent per line)

### group-config

#### get

- **method**: GET
- **path**: /v1/sync/group-config
- **role**: writer
- **summary**: Anti-entropy pull: hub bindings, group charter keys, design docs (?projects=)

#### post

- **method**: POST
- **path**: /v1/sync/group-config
- **role**: writer
- **summary**: Anti-entropy push: fill-only config, newest-wins design docs

### memories

#### get

- **method**: GET
- **path**: /v1/sync/memories
- **role**: writer
- **summary**: Anti-entropy pull: JSONL of {project_id, memory|curate_run} (?projects=)

#### post

- **method**: POST
- **path**: /v1/sync/memories
- **role**: writer
- **summary**: Anti-entropy push: memories merge union/staleness-wins, runs id-idempotent
