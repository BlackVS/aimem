---
name: custom-codereview-guide
description: Repository-specific review guidance read by oh-code-review before every review of aimem: observed runtime contracts that settle otherwise speculative findings.
triggers:
- /oh-codereview
---

# Review guide for aimem

Read by `oh-code-review` before every review of this repository. Keep it short
and evidence-based; every entry names how it was established.

## Observed Runtime Contracts

Do not infer runtime behaviour from documentation or source alone. If the
reviewed route differs from a contract below, or the runtime has changed
since, request a focused probe instead of proposing speculative hardening.

| Contract | Runtime | Observed | Date · how |
| --- | --- | --- | --- |
| `Invoke-WebRequest` follows a redirect from https to http and downloads over http | Windows PowerShell 5.1 (5.1.26100, and the GitHub `windows-latest` runner) | the 302 to an http URL was followed and the http request made; `boot.ps1` therefore walks redirects itself and refuses a non-https hop | 2026-11-15 · a local https listener answering 302, and workflow runs 43704068123 and 43704265021 on a scratch branch |
| `Invoke-WebRequest` does **not** follow a redirect from https to http | PowerShell 7.6 on `windows-latest` | refused with an InvalidOperationException unless `-AllowInsecureRedirect` is given; https to https is followed | 2026-11-15 · runs 43704068123 and 43704265021 |
| A 3xx is returned to the caller with `-MaximumRedirection 0` | 5.1 and 7.6 | 5.1 returns it only with `-ErrorAction SilentlyContinue` (with `Stop` it throws); 7 only with `-SkipHttpErrorCheck` | 2026-11-15 · run 43704265021 |
| An unsigned script run with `-ExecutionPolicy RemoteSigned -File` from a share | 5.1 and 7.6 on `windows-latest` | 5.1 refuses a share named by an address or a dotted host name; 7 runs the address and refuses the dotted name; an undotted name runs in both. A `Copy-Item` copy in a local folder runs in both and carries no Zone.Identifier, so the documented export form copies `boot.ps1` first | 2026-11-15 · runs 43703910712 and 43704504385 |

## Rejected hypotheses

- "PowerShell 7 downgrades a download to http on a redirect, so `boot.ps1`
  needs a PowerShell 7 specific guard" — disproved 2026-11-15 (row two
  above). The redirect walk in `boot.ps1` is for Windows PowerShell 5.1;
  under 7 it is harmless and 7's own guard stays in place.
