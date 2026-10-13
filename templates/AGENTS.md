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

The handoff holds only the current state. Keep that state above the line
`<!-- aimem: history below -->`; finished items that are no longer needed
to continue go below it. Run `aimem handoff compact` to move everything
below the line to `docs/SESSION-STATE-ARCHIVE.md` under a dated heading
(without the line, it keeps the head within 50 lines and 8 KiB and a
closing section of up to 10 lines headed Pick up here, Pick up, Exact
next action, Next action, Next or Next steps, in any case), instead
of cutting the file by hand; `--summarize` also adds a short summary of
what moved, written by the curation backend. At each milestone, rewrite
the current state yourself; the command only moves history out. The
archive is never injected into a session; read it only when you need the
history. The session-start hook (Claude Code and Codex) injects at most
the first 8 KiB of the handoff and then a one-line note that it was cut,
so anything past that point does not reach the session.

## Environment blockers

Before you report a blocker caused by the environment (credentials, SSH,
hardware keys, network, missing tools), recall aimem with the error's text
(the `recall_memory` MCP tool, or `aimem recall -q "<error text>"`, adding
`-u` for the user's own scope) and apply what it finds. Report the blocker
only when nothing there resolves it, and say what you searched for. In
Claude Code, `aimem failure-recall` already adds matching memories after a
failed Bash command; read them before you decide.

## Project context

<!-- Replace everything below with what an agent needs to know about THIS
     repository: what it is, the constraints that are not visible in the
     code, and where the authoritative design documents live. -->

(Describe this project here.)
