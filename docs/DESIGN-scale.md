# Design — scaling curated memory past 10^3 facts per project

Status: proposal 1 IMPLEMENTED (v0.1.73); 2-4 designed, not built.
Written 2026-08-28 from measurements on the
home hub. Companion to `DESIGN.md` and the multiprovider design.

## The ceiling is explicit, and we are approaching it

`docs/DESIGN.md` states the assumption behind brute-force
cosine: *"The store is small (10²–10³ facts/project), so brute-force
cosine in Go is exact and fast; no ANN index needed."* That assumption
is now load-bearing: the aimem project holds **177 facts and gains ~78
per day**, i.e. ~1,000 in under two weeks and ~10,000 in about four
months.

## What actually breaks (measured, not assumed)

Three paths scan EVERY stored vector, and a 3072-dim float32 vector is
12,288 bytes:

| path | when it runs | cost at 1k / 10k / 50k facts |
|---|---|---|
| `fuseVectorLeg` | every recall query | 12 / 123 / 614 MB scanned |
| `Nearest` | once per proposal (~10 per curate run) | same, ×10 per run |
| `Embeddings` + `DedupProject` | nightly 04:xx sweep | loads ALL vectors at once, then O(n²) cosine |

Against the hub unit's `MemoryHigh=256M` / `MemoryMax=512M`:

- **~1k**: fine — millisecond recall.
- **~5k**: 60 MB per recall (user-visible latency); 12M dedup pairs.
- **~10k**: dedup holds ~123 MB resident and does 50M pair comparisons
  over 3072 dims — minutes of CPU under a soft-capped unit.
- **~50k**: 614 MB exceeds `MemoryMax` — the curate/dedup unit is
  OOM-killed.

Two more bite around the same point: the design-doc generator puts
every fact of a chapter into one prompt (a 500-fact chapter is ~50k
tokens per section), and `GET /v1/projects/{p}/memories` returns the
entire set — the KB tab's paging is client-side only.

## 1. Shorten the vectors (IMPLEMENTED, v0.1.73)

`AIMEM_EMBED_DIM=768` sends OpenAI's `dimensions` parameter. Verified
honoured by BOTH providers in use (gemini-embedding-001 and
text-embedding-3-large returned exactly 768). This is **4× less** data
scanned, held and stored, for a modest recall-quality cost — the
cheapest possible lever, and it buys roughly a year of runway.

Key design point: vectors of different dimensions are NOT comparable
(`Cosine` returns 0 on a length mismatch). So the dimension is part of
the vector space's identity — embeddings are stored under
`<model>@<dim>`. Changing the dimension therefore makes
`NeedingEmbedding` treat every fact as unembedded and re-embed it,
rather than silently mixing two incompatible spaces under one name.
Old vectors stay on disk, unused, until pruned. `Embed` also refuses a
response whose width differs from the request, so a provider that
ignored `dimensions` cannot corrupt the space.

## 2. Bound the scans (do at ~2,000 facts/project)

- Recall: score the vector leg over a candidate pool (FTS top-N, plus
  recent and pinned facts) instead of the whole table. Keeps hybrid
  RRF meaningful while making the cost independent of KB size.
- `Nearest`: same candidate-pool treatment, or skip write-time dedup
  when a project exceeds a threshold and let the nightly sweep own it.
- `DedupProject`: stream in blocks instead of loading every vector, and
  compare only within a blocking key (e.g. shared tag or chapter) so
  the sweep stops being O(n²) over the whole store.

## 3. Vector index — sqlite-vec, NOT a vector server (DECIDED 2026-08-28)

**Decision (user-agreed 2026-08-28, corrected same day):** the index
stays INSIDE the per-project SQLite file — never a vector server. But
"sqlite-vec" is the wrong implementation for this codebase: the driver
is `modernc.org/sqlite` (pure Go) and every release cross-builds with
`CGO_ENABLED=0`, so a C extension cannot load at all (DESIGN.md already
said so). Adopting sqlite-vec would mean switching to a cgo driver and
giving up building both OS binaries from one machine.

So when an index is finally needed, in preference order:
1. **Pure-Go ANN (HNSW/IVF) over the existing float32 blobs** — same
   file, same driver, no new dependency, keeps CGO_ENABLED=0.
2. **sqlite-vec** — only if the project ever accepts cgo and per-OS
   build agents.
3. **Derived hub-side pgvector index** — only for cross-project search
   (see below); never as source of truth.

Do NOT build any of them yet: proposal 2 comes first at ~2,000
facts/project, and an index only pays off around 10^5 vectors.

The proposal names the escape hatch: *"prefer a vector extension inside
the same per-project database file (for example sqlite-vec) over a
separate vector server, so project isolation and single-file deletion,
export, and backup are preserved."* pgvector (or any server-side vector
DB) was reconsidered 2026-08-28 and rejected for this system, for
reasons that are about topology rather than performance:

- **Local-first recall.** aimem runs on workstations and laptops, not
  only hubs; recall goes to a local unix socket. A central Postgres
  makes semantic recall depend on reaching a server — reintroducing
  precisely the network dependency the design keeps off the read path.
  sqlite-vec leaves every machine self-sufficient offline.
- **Isolation is physical, not a WHERE clause.** One SQLite file per
  project means `drop-project` is a file delete, export/backup is a
  file copy, and an FTS/vector index can only ever cover one project.
  Centralising vectors turns each of those into a query predicate and
  splits a project's data across two stores.
- **Operational weight.** Today: one static Go binary + systemd on a
  1GB LXC capped at MemoryMax=512M. Postgres adds a server to install,
  tune, secure, back up and upgrade, plus a new failure mode, for a
  system whose ethos is no external dependency on the write path.
- **The numbers do not ask for it.** At 768 dims, 10k facts is a 31MB
  scan — brute-force cosine in Go is tens of milliseconds. ANN pays off
  around 10^5+ vectors per project, which is years away at ~78/day.

pgvector becomes the right answer if the goal changes: 10^6+ vectors,
heavy concurrent query load, or **one cross-project semantic index**
(searching all projects at once, which per-project SQLite cannot do
without fan-out). If that day comes, the clean shape is a hub-only
pgvector index that is DERIVED and rebuildable — per-project SQLite
stays the source of truth, nothing depends on the index, and it can be
dropped and rebuilt at will.

## Known limitation: some proxies cannot shorten vectors

`llm.example.com` fronts a third-party wrapper
(`openai-wrapper.sqdonline.cc`) and registers embedding models under
non-standard names (`openai/Text_Embedding_3_Large`), so LiteLLM does
not treat them as `dimensions`-capable and drops the parameter — the
width guard in `Embed` catches this and refuses rather than storing
mismatched vectors. It is a remote service outside this project's
control, so the work hub stays at 3072 dims and proposal 2 (bounded
scans) matters more there than proposal 1.

## 4. Page the API and cap synthesis (do when a scope feels slow)

- `GET /v1/projects/{p}/memories`: add `limit`/`offset` (or a cursor)
  and have the KB tab fetch pages instead of the whole set.
- `GenerateDoc`: cap facts per section (highest confidence /
  corroboration first) so a large chapter cannot blow the context
  window or the budget.

## Cheaper than all of the above

Fact volume itself is a choice: ~78 facts/day for one project is high,
and much of it folds later as near-duplicates. Tighter extraction
(fewer, more durable facts per run) and the dedup sweep keep the
working set small, which beats any indexing work.
