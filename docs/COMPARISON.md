# How aimem compares

Facts below were checked against each project's repository, license
file, and documentation on 2026-08-29 (lemmalog added 2026-09-04);
star counts are approximate as of those dates. Corrections welcome — a comparison that misstates another
project is worse than none, and each of these tools is genuinely good
at what it optimizes for. Claims we could not verify are marked
*unverified* rather than guessed.

## Three different categories

"AI memory" names three kinds of software that get compared as if they
were one:

1. **Memory SDKs and platforms** you build into an agent application
   you are writing: Mem0, Zep/Graphiti, Cognee. They assume *you* own
   the agent loop and give it a memory API.
2. **Agent runtimes** where memory is one subsystem: Letta (the MemGPT
   lineage). You install it *instead of* your coding agent — its own
   docs position it against the Claude Agent SDK and OpenCode SDK — so
   its memory cannot be bolted onto a loop you already run.
3. **Deployed memory for a coding agent you already use** — tools that
   attach to Claude Code, OpenCode, or similar via hooks, plugins, or
   MCP: claude-mem, Engram, Basic Memory, Mem0's Claude Code plugin,
   Cognee's plugin, mcp-memory-service, lemmalog, and aimem.

aimem is in the third category. Within it, the split that predicts
behavior is **hook-based** (capture is mechanical, fires every turn
whether or not the model thinks of it) versus **MCP-tool-based** (the
model must remember to call the memory tool — capture is as reliable as
the model's judgment that turn).

## The baseline: Claude Code's native memory

Claude Code ships two mechanisms: CLAUDE.md instruction files (loaded
every session, project-root copy re-injected after compaction) and
auto-memory (Claude writes its own Markdown notes per project, with an
index re-injected after compaction). Both are free and local. Their two
documented limits are the reason this category exists: capture is
**model-decided** ("Claude doesn't save something every session"), and
memory is **machine-local** ("files are not shared across machines").

## Category 3: memory for coding agents you already use

The two capabilities that define the category — capturing every turn
automatically, and surviving context compaction — plus the axes where
the tools genuinely differ:

| | Auto per-turn capture | Survives compaction | Works with zero LLM calls | Multi-machine | Storage |
|---|---|---|---|---|---|
| **aimem** | yes — mechanical hooks, verbatim | yes — `PreCompact` marker + `SessionStart` re-injection | yes — capture, recall (BM25) and recovery are LLM-free; curation and embeddings are opt-in layers | yes — real-time hub push with spool fallback, anti-entropy sync, partitioned multi-hub | one static Go binary, one SQLite file per project |
| **claude-mem** (~92k★) | yes — `Stop` + `PostToolUse` hooks | yes — `SessionStart` incl. `compact` | no — LLM in the capture path | no | local |
| **Mem0 Claude Code plugin** | yes — `Stop` hook, but LLM-*summarized*, not verbatim | yes — `PreCompact` + `SessionStart` | no — cloud key required | via Mem0's cloud | Mem0 platform |
| **Engram** (~6.2k★) | yes — `UserPromptSubmit` + `Stop`, no LLM | yes — dedicated post-compaction hook | yes | local-first; optional cloud replication | Go + SQLite |
| **Basic Memory** (~3.8k★, AGPL-3.0) | no — transcript read once, at `PreCompact` | yes — its headline feature (extractive, no LLM) | yes | via its cloud ($15/mo) | plain Markdown files |
| **mcp-memory-service** (~1.9k★) | partial — stores only ≥300-char decision/error/learning matches | no — compaction re-injection is off by default | degraded without extras (hash pseudo-vectors) | partial | SQLite |
| **lemmalog** (~270★, MIT) | no — facts enter through an extraction boundary (LLM extractor, or the agent asserting via MCP); source episodes kept as provenance, not a turn journal | no — persistent memory, but no compaction hook or handoff re-injection | engine, Datalog queries and context assembly are LLM-free; extraction is not | no — snapshot files shareable by path, no sync | Rust crate, TSV snapshots, event-sourced (derived facts rebuilt on load) |

What is genuinely rare is the combination aimem was built around:
**mechanical verbatim capture on every turn + zero LLM dependency for
capture and recovery + multi-machine sync with partitioned hubs**.
Engram is the nearest neighbor — per-turn, LLM-free, Go + SQLite — and
if you want exactly that on one machine with optional replication, it
is a fine choice.

Where the others are stronger than aimem, plainly:

- **Licensing**: most tools in this category are MIT or Apache-2.0;
  aimem is source-available under PolyForm Noncommercial from v0.2.0
  (free for noncommercial use, commercial use licensed by the author).
  If an OSI-approved license is a requirement, the others qualify and
  aimem does not.

- **claude-mem**: adoption, community, and the polish ~92k stars buy.
- **Basic Memory**: memories are Markdown you can read and edit in
  Obsidian — its whole store is a vault. aimem's human-editable surface
  is narrower by design: shared docs are ordinary files, and wiki
  collections render to committed markdown, but the knowledge base
  itself is structured rows, not a folder of notes.
- **Mem0 plugin**: inherits a mature hosted platform, dashboards, and
  memory shared with non-coding agents.
- **supermemory** (~2.7k★ plugin): LLM fact extraction with explicit
  supersession and expiry — the strongest conflict/temporal story among
  the hook tools besides aimem's. Two caveats from its own materials:
  "hybrid search" there means RAG+graph (no lexical leg), and the
  self-hosted server is "supermemory lite," capped at 10,000 documents
  enforced at the API (the repo license is MIT; the cap is stated in
  the server release notes).
- **lemmalog** (~270★, Rust, MIT): the deepest *reasoning* story of
  any tool on this page — memory as a **deductive database**. Datalog
  rules derive consequences from asserted facts incrementally, `why()`
  returns proof trees back to source episodes, facts are bi-temporal
  with point-in-time queries, `what_if` runs hypotheticals with exact
  store restoration, entities canonicalize through aliasing, and it
  publishes real LongMemEval/LoCoMo numbers (self-reported but
  methodologically explicit). aimem's KB stores, ranks, corroborates
  and supersedes knowledge, but it cannot *infer* from it — lemmalog
  can, mechanically and explainably. The shapes are complementary
  rather than competing: lemmalog answers "what follows from what we
  know, and prove it"; aimem answers "what happened, survive the
  compaction, and share it across machines" — and does its capture
  with no LLM in the path, which lemmalog's extraction boundary needs
  (an LLM extractor, or a disciplined agent asserting by hand).

## Categories 1–2: SDKs, platforms, and runtimes

Not alternatives to aimem — you reach for these when you are *building*
an agent, not deploying memory onto one you already use. Summarized
here because the names come up in every comparison.

| | License / stars | Self-host | Needs an LLM key | Conflict & temporal model | Retrieval |
|---|---|---|---|---|---|
| **Mem0** | Apache-2.0, ~64k★ | yes — embedded Qdrant + SQLite by default | yes by default (`infer=False` skips extraction; embeddings still needed) | **v3 removed conflict resolution**: single-pass ADD-only, "memories accumulate; nothing is overwritten"; graph memory is paid-cloud-only now | vector + BM25 + entity matching, fused |
| **Zep** (service) / **Graphiti** (library) | Graphiti Apache-2.0, ~30k★; Zep CE discontinued 2025 — no free self-hosted Zep | Graphiti yes (needs a graph DB: Neo4j / FalkorDB / Neptune) | yes — LLM + embeddings mandatory for ingest | **best-in-class**: genuinely bi-temporal (4 timestamps per edge), contradictions invalidated rather than deleted, point-in-time queries | hybrid: cosine + BM25 + graph traversal, rerankers |
| **Letta** | Apache-2.0, ~24k★ (the coding-agent CLI is the separate TS `letta-code`, ~3k★) | yes | yes — it *is* the agent | self-editing memory blocks; memory is a **git repo of Markdown** per agent; no extraction, no contradiction engine — conflict handling is git's | none by default ("normal file-search and read tools"); vector search is an opt-in mod |
| **Cognee** | Apache-2.0, ~30k★ | yes — fully embedded default (SQLite + LanceDB + Kuzu) | yes, but can delegate to the MCP host's model — no key at all in that mode | knowledge graph with ontologies; contradiction detection opt-in and off by default (records, doesn't resolve); bi-temporal + conflict resolution are **Enterprise-only** | 20 search types: vector, graph, lexical, hybrids |

Honest flags on the numbers you will see quoted: Mem0's and Zep's
LoCoMo/LongMemEval scores measure their *managed platforms*, not the
open-source code (Mem0's README says so; Zep's are vendor-published and
LLM-judged), and Mem0's scoring of LoCoMo is disputed by Zep. Treat
benchmark tables in this space accordingly.

## The axes aimem optimizes

- **Capture must not depend on a model.** Journaling is a hook writing
  SQLite: milliseconds, deterministic, works offline. Recovery from a
  crash never waits on an API.
- **Verbatim journal first, curation second.** The raw turn is always
  kept; fact extraction is an asynchronous, budgeted, optional layer on
  top — never the capture path. The lifecycle closes at the other end
  too: a **staleness review loop** queues old, thinly-corroborated
  facts for an audited verdict (confirm / supersede / expire), with the
  queue derived from the audit trail rather than stored.
- **Context arrives before the first mistake.** The session handoff is
  re-injected at every session start, and — opt-in — a token-budgeted
  slice of recalled facts rides with it, matched against the previous
  session's activity. Mechanical and local, like the rest of the
  capture path: no LLM call, zero egress without embeddings.
- **Multi-machine is first-class.** Real-time hub push with spool
  fallback, anti-entropy sync, and *partitioned* hubs so work and
  personal memory can live on physically separate servers. It extends
  past facts: **shared documents** version the session handoff and
  runbooks on the hub with compare-and-swap (refuse-and-merge, never
  newest-wins), so the next machine reads the same current handoff
  without a git push — none of the tools above moves authored working
  documents at all.
- **One static binary, one file per project.** No Docker, no external
  vector DB, no runtime; embeddings are blobs in the same SQLite file.

If those are not your axes — you want a hosted platform, a temporal
knowledge graph, human-editable notes, or an SDK for an agent you are
building — one of the projects above serves you better, and the tables
say which.
