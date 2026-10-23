# aimem process set

The manifest at `processes/aimem/manifest.json` is an aimem v0.6.0-compatible
process set. Its handbook and READY/DONE checklists form one bootstrap bounded
at 12 KiB. The two JSON templates are examples: replace their placeholders and
supply the project and a unique idempotency key when creating a task. Do not
submit placeholder text or treat a template as a ready-to-run request.

This set follows aimem's project policy. Other projects must review their own
rules before selecting it; a shared process does not override project rules or
owner instructions. Only oh-code-review is required. QA and writing skills may
be used when appropriate but are not mandatory for every task.

Review changes to all referenced files together. After merge, select the full
40-character merged commit, not a moving branch name. On the hub host:

```sh
aimem process select REPOSITORY_HTTPS_URL MERGED_COMMIT processes/aimem/manifest.json --ref main -p aimem
```

The initial selection expects no existing commit. To replace an existing
selection, add `--expect PREVIOUS_COMMIT`; re-read after a conflict instead of
retrying with a guessed value. Keep the previous reference for a deliberate
rollback, using the current commit as the expected value.

From each participating checkout on every agent machine:

```sh
aimem task-token show-source
aimem process show
aimem process show --template task
aimem process show --template investigation
```

Confirm the intended project, repository, commit and manifest; the complete
handbook; all six READY and six DONE items; and required-skill availability.
A successful command exit alone is insufficient: `process show` also prints
explicit unavailable notices with a successful exit. Test a fresh cache and a
session with no recalled facts. Use an isolated hub/state for denial, missing
assets and offline-cache tests; never interrupt a production hub to simulate
failure. Restart old agent sessions after upgrading clients. OpenCode clients
without a session-start integration must run `aimem process show` explicitly.

The hub stores only this Git reference. No skills are installed by selection or
by hooks, and cached process documents never authorize task writes. Templates,
handbook and checklists must all come from the same pinned commit.
