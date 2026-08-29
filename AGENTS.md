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
