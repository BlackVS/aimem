# Writing text that is kept

This rule covers text that outlives the conversation: the body an agent sends
to a write tool (task fields and comments, shared documents, memories,
records, team messages, submissions and reasons) and the Markdown files an
agent saves (handoffs, reports, documentation). Other people and later
sessions read this text without the conversation around it. Chat replies are
not covered. For general style, use the `oh-technical-writing` skill where it
is installed; this rule is the minimum every kept text must meet.

## The rule

- Write complete, grammatical sentences with normal spacing between words.
- Name who acts and what they do: "the worker pushed the fix to the branch",
  not "fix pushed".
- Explain an uncommon abbreviation the first time it appears.
- Shorten by removing repetition and by linking to evidence instead of
  restating it. Never shorten by joining words or numbers, by dropping
  articles and verbs, or by writing prose as slash-separated fragments.
- Keep exact text exact: code, commands, identifiers, paths, hashes,
  versions, quoted errors and machine-readable payloads stay as they are.
  The rule applies to the sentences that explain them.
- Time pressure, a long session, a small context window and compaction do
  not relax any of this. A short, clear text is always possible; a
  compressed one is not clear.

## Check before each write

Read the exact text you are about to send or save, not your summary of it:

1. Are any words or numbers joined together?
2. Is any sentence missing its subject or verb?
3. Is any shorthand left unexplained?
4. Could an operator who did not see this session tell the outcome, its
   limits and the next action?

Fix what the check finds, then write. The check needs no extra tool call or
model call.

## Examples

These are invented.

A task comment, compressed:

> rootcause=cachekey/missing tenant;fixed+tests,deploy pending(ops)

The same comment, kept readable:

> The cache key omitted the tenant, so two tenants could read each other's
> entries. The fix adds the tenant to the key, with a test for each tenant.
> It is not deployed yet: the operations team deploys it after review.

A handoff line, compressed:

> next: rerun e2e w/ pin bump->verify flakes gone,then PR

The same line, kept readable:

> Next: move the end-to-end tests to the new pin, run them three times to
> confirm the intermittent failure is gone, then open the pull request.

## Review checklist

When reviewing kept text, written by you or by another agent:

- Words are separated, and sentences have a subject and a verb.
- Every abbreviation is common or explained.
- Code, paths, identifiers and quoted evidence are exact.
- The outcome, its limits and the next action are stated plainly.
- Nothing was shortened by compression; long material is linked, not
  squeezed.
