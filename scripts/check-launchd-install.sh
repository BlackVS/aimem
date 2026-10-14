#!/usr/bin/env bash
# macOS CI: install.sh registers real launchd user agents. A fresh install
# answers health through the serve agent, a re-run upgrades in place
# through the agents, and uninstall leaves no agent loaded and no plist.
#
# It installs into the account it runs in, so it runs only on a CI runner.
set -euo pipefail
if [ "${CI:-}" != true ] || [ "$(uname -s)" != Darwin ]; then
  echo "check-launchd-install: runs only on a macOS CI runner (it installs into this account)" >&2
  exit 2
fi
cd "$(dirname "$0")/.."

tmp=$(mktemp -d)
label=io.github.blackvs.aimem
dom="gui/$(id -u)"
agents="$HOME/Library/LaunchAgents"
export AIMEM_BIN_DIR="$tmp/bin"

for v in v0.0.1 v0.0.2; do
  CGO_ENABLED=0 go build -ldflags "-X main.version=$v" -o "$tmp/aimem-$v" ./cmd/aimem
done
launchctl print "$dom" >/dev/null || { echo "FAIL: no $dom domain on this runner" >&2; exit 1; }

echo "--- fresh install"
AIMEM_PREBUILT="$tmp/aimem-v0.0.1" bash install.sh user | tee "$tmp/install.log"
for l in "$label" "$label.sync"; do
  plutil -lint "$agents/$l.plist"
  launchctl print "$dom/$l" >/dev/null || { echo "FAIL: $l is not loaded" >&2; exit 1; }
done
launchctl list | grep -F "$label"
"$AIMEM_BIN_DIR/aimem" health | grep -q '"version":"v0.0.1"' || { echo "FAIL: no health at v0.0.1" >&2; exit 1; }

echo "--- re-run upgrades in place"
AIMEM_PREBUILT="$tmp/aimem-v0.0.2" bash install.sh user | tee "$tmp/upgrade.log"
grep -q 'upgraded aimem v0.0.1 -> v0.0.2; service healthy at v0.0.2' "$tmp/upgrade.log" ||
  { echo "FAIL: the re-run did not upgrade through the agent" >&2; exit 1; }

echo "--- uninstall"
bash install.sh uninstall-user
for l in "$label" "$label.sync"; do
  if launchctl print "$dom/$l" >/dev/null 2>&1; then echo "FAIL: $l is still loaded" >&2; exit 1; fi
  if [ -e "$agents/$l.plist" ]; then echo "FAIL: $agents/$l.plist remains" >&2; exit 1; fi
done
rm -rf "$tmp"
echo "launchd agents: install, upgrade and uninstall OK"
