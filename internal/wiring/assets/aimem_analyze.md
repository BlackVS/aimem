---
description: Analyze this repo into the shared aimem knowledge base, subsystem by subsystem
---
<!-- managed by aimem (`aimem wire`): regenerated from the aimem binary; change the source in the aimem repository, not this file -->
Analyze this repository to build durable knowledge, subsystem by subsystem.
Focus area for this run (optional): $ARGUMENTS

Before starting: read .aimem.json to learn this project's name and declared
knowledge groups. Call get_design_doc to load what the group's knowledge
base already knows, and recall_memory (scope "both") for anything about
this repo. Do not re-derive what is already recorded — verify and extend.

Then work through the codebase one subsystem at a time: entry points, core
data flow, then supporting modules. For each, understand and summarize in
your replies: what it does, its key invariants and assumptions, gotchas,
cross-module contracts, and why non-obvious decisions were made.

Record knowledge, don't transcribe code: use the remember tool for the most
important findings (one self-contained sentence each; kind fact/decision/
convention; tags = component names). Facts that hold framework-wide across
the group's repos: scope "group:<name>". Facts specific to this repo:
default project scope. Never record what is trivially readable from the
code (signatures, field lists) — record behavior, invariants, and reasons.

End each work chunk with a short summary reply of what you established —
those summaries are what curation distills into the knowledge base.
