#!/usr/bin/env bash
# A real client upgrade on a hosted Linux or macOS runner, from published
# releases: the one-liner installs FROM, `aimem upgrade --release TO` moves
# it to TO, and a failed upgrade rolls back. The versions come from
# scripts/client-upgrade.env.
#
# The failure run comes first: TO's real installer with a stand-in binary
# that answers `version` and nothing else, so the new service never becomes
# healthy. The upgrade must exit non-zero, say ROLLED BACK, and leave FROM's
# binary serving. Then the real upgrade: the binary and the OpenCode plugin
# must be TO's published files, the Claude Code hooks must still be wired,
# and the service (systemd user unit or launchd agent) must answer health
# at TO.
#
# The order is fixed, and nothing ever goes back from TO to FROM: a binary
# refuses a database newer than it understands, so a downgrade fails as
# soon as the pair spans a schema change. The rollback restores FROM on its
# own state, and the account ends at TO.
#
# It installs into the account it runs in, so it runs only on a CI runner.
set -euo pipefail
case "$(uname -s)" in
  Linux) asset=aimem-linux-amd64 ;;
  Darwin) asset=aimem-darwin-arm64 ;;
  *) asset= ;;
esac
if [ "${CI:-}" != true ] || [ -z "$asset" ]; then
  echo "check-client-upgrade: runs only on a Linux or macOS CI runner (it installs into this account)" >&2
  exit 2
fi
cd "$(dirname "$0")/.."

FROM=$(sed -n 's/^FROM=//p' scripts/client-upgrade.env)
TO=$(sed -n 's/^TO=//p' scripts/client-upgrade.env)
for v in "$FROM" "$TO"; do
  echo "$v" | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+' || { echo "FAIL: scripts/client-upgrade.env: bad version '$v'" >&2; exit 1; }
done
repo=BlackVS/aimem
dl=https://github.com/$repo/releases/download
bin="$HOME/.local/bin/aimem"
plugin="$HOME/.config/opencode/plugins/aimem.ts"
tmp=$(mktemp -d)

fail() { echo "FAIL: $*" >&2; exit 1; }
sha() { shasum -a 256 "$1" | awk '{print $1}'; }
sum_of() { awk -v n="$2" '$2==n || $2=="*"n {print $1}' "$tmp/SHA256SUMS.$1"; }
serving() {
  "$bin" health > "$tmp/health" 2>&1 || true
  grep -qF "\"version\":\"$1\"" "$tmp/health" || { cat "$tmp/health"; fail "no health at $1 ($2)"; }
  if [ "$asset" = aimem-linux-amd64 ]; then
    systemctl --user is-active --quiet aimem || fail "the aimem user unit is not active ($2)"
  else
    launchctl print "gui/$(id -u)/io.github.blackvs.aimem" >/dev/null || fail "the aimem launchd agent is not loaded ($2)"
  fi
  echo "ok: serving $1 ($2)"
}
for v in "$FROM" "$TO"; do curl -fsSL -o "$tmp/SHA256SUMS.$v" "$dl/$v/SHA256SUMS"; done

echo "--- one-liner install of $FROM"
curl -fsSL "https://raw.githubusercontent.com/$repo/$FROM/boot.sh" | AIMEM_USER_ONLY=1 bash
serving "$FROM" "after install"

echo "--- $TO with a binary that never answers health: the upgrade must roll back"
x="$tmp/export/aimem/$TO"
mkdir -p "$x"
for f in install.sh install.ps1 aimem-opencode.ts; do
  curl -fsSL -o "$x/$f" "$dl/$TO/$f"
  [ "$(sha "$x/$f")" = "$(sum_of "$TO" "$f")" ] || fail "$f does not match $TO's SHA256SUMS"
done
cat > "$x/$asset" <<EOF
#!/bin/sh
[ "\${1:-}" = version ] && { echo "aimem $TO"; exit 0; }
exit 1
EOF
chmod 755 "$x/$asset"
(cd "$x" && shasum -a 256 install.sh install.ps1 aimem-opencode.ts "$asset" > SHA256SUMS)
printf 'aiforge-export 1\naimem %s\n' "$TO" > "$tmp/export/MANIFEST"
rc=0
AIMEM_BASE="$tmp/export" AIMEM_UPGRADE_WAIT=10 "$bin" upgrade --release "$TO" > "$tmp/fail.log" 2>&1 || rc=$?
cat "$tmp/fail.log"
[ "$rc" -ne 0 ] || fail "the upgrade to a binary that never serves exited 0"
grep -q 'ROLLED BACK' "$tmp/fail.log" || fail "the failed upgrade did not say ROLLED BACK"
[ "$(sha "$bin")" = "$(sum_of "$FROM" "$asset")" ] || fail "the binary after the rollback is not $FROM's $asset"
serving "$FROM" "after the rollback"

echo "--- aimem upgrade --release $TO"
"$bin" upgrade --release "$TO" 2>&1 | tee "$tmp/upgrade.log"
grep -qF "upgraded aimem $FROM -> $TO; service healthy at $TO" "$tmp/upgrade.log" ||
  fail "the upgrade did not report a healthy service at $TO"
[ "$(sha "$bin")" = "$(sum_of "$TO" "$asset")" ] || fail "the binary is not $TO's $asset"
[ "$(sha "$plugin")" = "$(sum_of "$TO" aimem-opencode.ts)" ] || fail "the OpenCode plugin is not $TO's aimem-opencode.ts"
grep -qF 'aimem submit-claude' "$HOME/.claude/settings.json" || fail "the Claude Code hooks are gone after the upgrade"
serving "$TO" "after upgrade"

echo "PASS: client upgrade $FROM -> $TO, and the rollback"
