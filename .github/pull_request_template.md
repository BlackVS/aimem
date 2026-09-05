## What

<!-- One paragraph: what changed and why. Reference the design doc
     (docs/DESIGN*.md) if this implements or corrects a proposal. -->

## Evidence

<!-- Command + observed result, never intentions. Full test output tail
     (with the pass/fail summary visible), or the exact commands that
     exercised the change end to end. -->

## Checklist

- [ ] `go test ./...` green — full output checked, not truncated
- [ ] Pre-push review gate passed (`oh-code-review` — findings fixed or explicitly waived; see AGENTS.md)
- [ ] CHANGELOG entry under `[Unreleased]` for user-facing changes (roll into a version before tagging)
- [ ] No hostnames, tokens, or operator specifics — this repo is public
- [ ] Docs updated where behavior changed (README, docs/, ADMIN-MANUAL, OpenAPI parity)
