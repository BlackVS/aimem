// Package installer tests the one-line installers as the upgrade path: the
// release pin every boot script and documented one-liner carries, and the
// upgrade transaction (state backup, swap, health at the new version,
// rollback) that install.sh, install-hub.sh and install.ps1 run on an
// existing installation. The transaction is a block between marker
// comments in each installer, extracted and run against real aimem
// binaries on a disposable state root.
package installer
