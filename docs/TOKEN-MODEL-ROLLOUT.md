# Token-model rollout

The hub token model (PR #56), client overrides (PR #57), and installer
upgrade detection (PR #58) are merged. v0.6.0 is the release target.
Merged code is not evidence that any hub or workstation has been upgraded.
Keep additional-project onboarding on hold until the deployment checks below
are recorded in the rollout task. Keep private deployment details out of PRs.

## Release and upgrade order

1. Merge the release documentation, then tag the reviewed master commit as
   `v0.6.0`. Wait for the release workflow to test, build all platform assets
   and publish checksums. Do not tag an unmerged branch.
2. Before upgrading each hub, stop its service and take a consistent backup
   of the complete state root, including access and project databases. Retain
   the old binary and record the backup privately. Access schema becomes 2;
   project schema stays 13. An old binary cannot open access schema 2:
   rollback requires the matching pre-upgrade state, losing any subsequent
   writes. Never run old and new binaries against the same state concurrently.
3. Upgrade the hub using the established [administrator deployment
   procedure](ADMIN-MANUAL.md). Verify `aimem version` and `aimem health`
   as the service user. Check project counts, task availability and existing
   grants/token scopes against the pre-upgrade record. Existing project and
   read-only tokens must keep their restrictions.
4. Upgrade every participating client via [client setup](INSTALL-CLIENT.md),
   verify `aimem version`, and restart agent sessions. Older clients ignore
   the local marker. A marker cannot impose credential selection on them.
5. Follow the [Kanban quickstart](KANBAN-QUICKSTART.md) with disposable test
   projects and credentials approved for the deployment. Record versions and
   results below before using the feature for real project onboarding.

The installer re-run upgrades an older numeric version. An equal/newer
version or a nonnumeric development build may be retained; verify the actual
binary version instead of assuming success from the installer exit status.
Use the documented forced reinstall when deliberately replacing such a build.

## Verification matrix

Run destructive lifecycle cases only against disposable fixtures. Never
expire, revoke or disable a live agent identity merely to prove a check.

| Scenario | Required result |
|---|---|
| User token with grant on project A | Task creation and read-back succeed |
| Add project B grant after token issuance | Same token writes B without reissue |
| Remove B's last direct/group grant | Next write to B is refused |
| Project-scoped token for A | Writes A; cannot write B even when its user has a B grant |
| Read-only token | Can read ordinary tasks; no task writes |
| Disable tasks on A | Writes refused; existing task reads retained |
| Disabled user, expired or revoked token | Authentication/write refused |
| Two checkouts on one machine/hub | A selects `project-local`; B selects `user-hub` concurrently |
| Invalid local override with valid per-hub token | Task call refused, no broader-token fallback |
| Explicit local `clear` | Per-hub selection restored; hub token itself is not revoked |
| Clone/worktree containing local marker | Requires its own credential setup |

`aimem task-token show-source` identifies the locally selected source without
showing its secret; it is not a substitute for the actual task write/read-back.
Current grants and task enablement remain required for both write-capable
token scopes. Scope restricts writes, not cross-project task-read visibility.

## Evidence and completion

The merged implementation has automated coverage in `internal/access`,
`internal/server`, `internal/taskcred`, `internal/mcp`, and `cmd/aimem`.
Run `go test ./...` for that regression suite. Release-candidate verification
also exercises the built CLI, HTTP API and real stdio MCP against isolated
state; it does not prove deployment on a remote hub or another workstation.

In the rollout task, record the release/tag, successful workflow, private
backup reference, deployed hub/client versions, and the two-checkout results.
Revoke disposable credentials when finished. Lift the onboarding hold only
after those deployment checks pass, then select and verify the project's
process instructions as the next roadmap increment.
