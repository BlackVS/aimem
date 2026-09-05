# Security Policy

aimem stores journals of coding sessions and serves them over an
authenticated hub — vulnerability reports are taken seriously.

## Reporting a vulnerability

**Do not open a public issue.** Report privately via GitHub's security
advisory form:

https://github.com/BlackVS/aimem/security/advisories/new

Include the version (`aimem version`), whether the issue affects the
client, the hub server, or the sync path, and reproduction steps.
You should receive an initial response within a week.

## Supported versions

The latest release is supported. Versions ≤ 0.1.90 (the MIT-licensed
line) receive no fixes.

## Scope notes for researchers

- Hub authentication is bearer-token (see `docs/DESIGN.md`, Security /
  egress); tokens are stored as SHA-256 digests on the hub.
- Secrets are redacted on write before storage or indexing
  (`internal/redact`) — redaction bypasses are in scope and
  particularly welcome.
- The admin console and `/v1` API surface is described by
  `/v1/openapi.json`, pinned to the real routes by a CI parity test.
