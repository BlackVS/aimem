# Agent Instructions

<!--
  Installed by aimem (`install.sh project`). This is a starting point, not
  a finished file: keep the handoff protocol, and replace the Project
  context section at the bottom with your own.
-->

## Session handoff protocol

The canonical handoff is `docs/SESSION-STATE.md`. It is the single source
for current task state; Git, test output, and CI remain authoritative for
their respective facts.

1. Read `docs/SESSION-STATE.md` at session start and again after any
   compaction. (Claude Code injects it via a SessionStart hook; OpenCode
   loads it via `opencode.json` instructions — but reread it explicitly
   after compaction.)
2. Treat every claim in the handoff as unverified until its evidence
   command is re-run or checked against Git. The file is a lead, not a
   contract.
3. Update the handoff after a meaningful verified milestone, before an
   expected compaction, and before intentionally ending a session.
4. Keep plans separate from completed work; completed items decay to one
   line.
5. Never claim tested/committed/pushed/merged/deployed without evidence
   (command + observed result, commit hash, or PR URL) written next to the
   claim.
6. End-of-session landing protocol: run verification FIRST, then write the
   handoff from the verified results, never from intentions.
7. Single-writer: the handoff header names the driving session. Do not
   overwrite another session's handoff without taking over explicitly.

## Handoff file rules

Fixed section order, hard cap ~50 lines, empty sections omitted, ends with
a one-line "Pick up here". Reference code by path and commit — never paste
it. Durable policy lives here, not in the handoff.

## Pre-push review gate

STRICT: before pushing to the remote, the pending changes MUST be
reviewed with the `oh-code-review` skill and the review must come back
clean — every finding either fixed or explicitly waived by the user.
Run the review on the final state of the changes (after the last
edit), fix, re-verify tests, then push. No exceptions for "small"
changes; a docs-only edit still gets the review (it is cheap). If the
skill is unavailable in the running environment, say so and get the
user's explicit go-ahead before pushing.

## Working in this repo

Build and test exactly as CI does:

    CGO_ENABLED=0 go build -o aimem ./cmd/aimem
    go test ./...            # full output, check the pass/fail summary
    gofmt -l .               # must print nothing
    go run honnef.co/go/tools/cmd/staticcheck@latest ./...

Package map: `cmd/aimem` CLI + subcommand wiring; `internal/store`
SQLite layer (journal, memories, docs, collections, migrations);
`internal/curate` LLM distiller + dedup; `internal/server` hub HTTP
API + admin console (`admin.html`); `internal/adapter` hook-side
capture, spool, hub push, doc sync; `internal/mcp` MCP facade;
`internal/embed`/`llmrate`/`provider` embeddings, call pacing,
model bindings; `internal/ident` project identity and groups;
`internal/tui` dashboard; `internal/schema`/`redact`/`diff3`/`uuidv7`
event schema, secret scrubbing, three-way merge, time-ordered ids.

Conventions that bite: master is PR-only (ruleset — direct pushes are
rejected); releases build only from tags reachable from master; write
commit messages to a file and use `git commit -F` (PowerShell mangles
UTF-8 on the command line); never introduce a BOM; roll the CHANGELOG
`[Unreleased]` section into a version BEFORE tagging (the release body
is extracted from it); OpenAPI (`internal/server/openapi.json`) is
pinned to real routes by a parity test — update both together.

## Project context

<!-- Replace everything below with what an agent needs to know about THIS
     repository: what it is, the constraints that are not visible in the
     code, and where the authoritative design documents live. -->

aimem itself: session resilience and shared memory for AI coding
agents. Public repo; PolyForm Noncommercial from v0.2.0 (≤ v0.1.90
remains MIT). Design authority: docs/DESIGN.md and the
docs/DESIGN-*.md proposals; record contradictions found during
implementation as proposal corrections, not silent divergences. The
one-liner installers (boot.sh / boot.ps1 / install-hub.sh) and the
GitHub release workflow are the deployment path - keep them working.
This repo is public: never commit hostnames, tokens, or operator
specifics; .creds/ and docs/SESSION-STATE.md stay gitignored here.
