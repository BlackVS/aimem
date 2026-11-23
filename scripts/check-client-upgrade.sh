#!/usr/bin/env bash
# A real client upgrade on a hosted Linux or macOS runner, from the previous
# release to the tree under test (docs/DEVELOPMENT.md, "Release gate").
# scripts/upgrade-versions.sh derives FROM, the previous release, and TO,
# the label the tree is built with. The previous release's one-liner
# installs FROM; FROM's own `aimem upgrade --release TO` then installs an
# export of the tree (AIMEM_BASE): the tree's installers, OpenCode plugin and
# binary. So only the first install is a published installer; the upgrade
# runs the installers the next release will ship.
#
# Before the upgrade, FROM's binary serves a throwaway hub on this runner,
# and FROM wires two checkouts to it and joins a team in one of them. After
# the upgrade the tree's MCP server lists, in the joined checkout, the team
# work tools from the record FROM wrote, and in the other the standalone
# set: what a client's next session sees, with no restart beyond the
# upgrade's own.
#
# The failure run comes first: the tree's installers with a stand-in binary
# that answers `version` and nothing else, so the new service never becomes
# healthy. The upgrade must exit non-zero, say ROLLED BACK, and leave FROM's
# binary serving. Then the real upgrade: the binary and the OpenCode plugin
# must be the tree's, the Claude Code hooks must still be wired, and the
# service (systemd user unit or launchd agent) must answer health at TO.
#
# Nothing ever goes back from TO to FROM, so the check does not depend on a
# downgrade working. It installs into the account it runs in, so it runs
# only on a CI runner, and it needs go (to build the tree) and jq.
set -euo pipefail
export LC_ALL=C # one sort order for the tool lists
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
src=$(pwd)

versions=$(bash scripts/upgrade-versions.sh)
FROM=$(echo "$versions" | sed -n 's/^FROM=//p')
TO=$(echo "$versions" | sed -n 's/^TO=//p')
SINCE=$(sed -n 's/^ROLLBACK_LINE_SINCE=//p' scripts/client-upgrade.env)
for v in "$FROM" "$TO" "$SINCE"; do
  echo "$v" | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+' || { echo "FAIL: bad version '$v'" >&2; exit 1; }
done
echo "upgrade: $FROM (the previous release) -> $TO (this tree, $(git rev-parse --short HEAD))"
repo=BlackVS/aimem
dl=https://github.com/$repo/releases/download
bin="$HOME/.local/bin/aimem"
plugin="$HOME/.config/opencode/plugins/aimem.ts"
tmp=$(mktemp -d)
hubpid=
trap '[ -z "$hubpid" ] || kill "$hubpid" 2>/dev/null || true' EXIT

# unit_state prints what systemd holds about the service: its restarts,
# its result (start-limit-hit when it refused a start) and its journal.
unit_state() {
  [ "$asset" = aimem-linux-amd64 ] || return 0
  echo "--- the aimem user unit at the failure" >&2
  systemctl --user show aimem.service -p ActiveState -p Result -p NRestarts -p StartLimitBurst -p StartLimitIntervalUSec >&2 2>/dev/null || true
  journalctl --user -u aimem.service --no-pager -n 40 >&2 2>/dev/null || true
}
fail() { echo "FAIL: $*" >&2; unit_state; exit 1; }
# start_limit: restart the unit until systemd refuses a start, as a crash
# loop leaves it, and check that it says so. The installers reset this
# before a start of their own (svc_reset); without that, the upgrade's or
# the rollback's start is refused here (task 01a208ef-65c1).
start_limit() {
  local i
  for i in 1 2 3 4 5 6; do systemctl --user restart aimem.service 2>/dev/null || true; done
  [ "$(systemctl --user show aimem.service -p Result --value)" = start-limit-hit ] ||
    fail "six restarts did not bring the unit to its start limit"
  echo "ok: the unit is at its start limit (Result=start-limit-hit)"
}
# at_least a b: release a is b or later.
at_least() {
  awk -v a="${1#v}" -v b="${2#v}" 'BEGIN { split(a, x, "."); split(b, y, ".")
    for (i = 1; i <= 3; i++) { if (x[i] + 0 != y[i] + 0) exit !(x[i] + 0 > y[i] + 0) } }'
}
sha() { shasum -a 256 "$1" | awk '{print $1}'; }
sum_of() { awk -v n="$1" '$2==n || $2=="*"n {print $1}' "$tmp/SHA256SUMS.$FROM"; }
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
# export_tree DIR BINARY: an aiforge export of release TO in DIR, with the
# tree's installers and plugin and BINARY as the platform's asset.
export_tree() {
  local x="$1/aimem/$TO"
  mkdir -p "$x"
  cp "$src/install.sh" "$src/install.ps1" "$x/"
  cp "$src/.opencode/plugin/aimem.ts" "$x/aimem-opencode.ts"
  cp "$2" "$x/$asset"
  chmod 755 "$x/$asset"
  (cd "$x" && shasum -a 256 install.sh install.ps1 aimem-opencode.ts "$asset" > SHA256SUMS)
  printf 'aiforge-export 1\naimem %s\n' "$TO" > "$1/MANIFEST"
}
curl -fsSL -o "$tmp/SHA256SUMS.$FROM" "$dl/$FROM/SHA256SUMS"

echo "--- the tree, built as $TO"
mkdir -p "$tmp/tree"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$TO" -o "$tmp/tree/aimem" ./cmd/aimem
"$tmp/tree/aimem" version | grep -qx "aimem $TO" || fail "the tree's build does not report $TO"
export_tree "$tmp/export" "$tmp/tree/aimem"
cat > "$tmp/stand-in" <<EOF
#!/bin/sh
[ "\${1:-}" = version ] && { echo "aimem $TO"; exit 0; }
exit 1
EOF
export_tree "$tmp/fail-export" "$tmp/stand-in"

if [ "$asset" = aimem-linux-amd64 ]; then
  echo "--- reset-failed on a unit that is not loaded yet: fails, and changes nothing"
  rc=0
  systemctl --user reset-failed aimem.service 2>&1 || rc=$?
  [ "$rc" -ne 0 ] || fail "reset-failed on a unit not loaded exited 0; the installers' svc_reset drops this failure"
  echo "ok: reset-failed exited $rc; svc_reset ends in || true"
fi

echo "--- one-liner install of $FROM"
curl -fsSL "https://raw.githubusercontent.com/$repo/$FROM/boot.sh" | AIMEM_USER_ONLY=1 bash
serving "$FROM" "after install"
[ "$(sha "$bin")" = "$(sum_of "$asset")" ] || fail "the installed binary is not $FROM's $asset"

echo "--- a throwaway hub on $FROM's binary; two checkouts wired by $FROM, one joined to a team"
h=$tmp/hub
mkdir -p "$h/home" "$h/state" "$h/tls"
cp "$bin" "$h/aimem"
(cd "$h/tls" && go run "$(go env GOROOT)/src/crypto/tls/generate_cert.go" --host 127.0.0.1 --ca --duration 2h)
port=47941
hubenv=(env -u XDG_RUNTIME_DIR AIMEM_STATE_DIR="$h/state" AIMEM_SOCKET="/tmp/aimem-upgrade-hub-$$.sock" HOME="$h/home")
hub() { "${hubenv[@]}" "$h/aimem" "$@"; }
"${hubenv[@]}" AIMEM_HTTP_LISTEN="127.0.0.1:$port" AIMEM_HTTP_TOKEN="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
  AIMEM_TLS_CERT="$h/tls/cert.pem" AIMEM_TLS_KEY="$h/tls/key.pem" "$h/aimem" serve > "$h/serve.log" 2>&1 &
hubpid=$!
for _ in $(seq 1 50); do hub health > /dev/null 2>&1 && break; sleep 0.2; done
hub health > /dev/null 2>&1 || { cat "$h/serve.log"; fail "the throwaway hub does not answer health"; }
expiry=$(date -u -d '+30 days' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+30d +%Y-%m-%dT%H:%M:%SZ)
hub tasks on -p upgrade-scenario > /dev/null
# The secret is the one line of hex; it goes to a file and is never printed.
hub token add upgrade-workstation | { grep -Ex '[0-9a-f]{32,}' || true; } > "$h/workstation.token"
chmod 600 "$h/workstation.token"
[ "$(wc -l < "$h/workstation.token")" -eq 1 ] || fail "aimem token add did not print one token line"
hub teams provision create upgrade-scenario --team Pilot --coordinator upgrade-coordinator --create-user \
  --label upgrade-coordinator --expiry "$expiry" --output "$h/coordinator.token" > /dev/null
hub teams provision add upgrade-scenario --team Pilot --member upgrade-worker --role worker --create-user \
  --label upgrade-worker --expiry "$expiry" --output "$h/worker.token" > /dev/null
"$bin" hub add upgrade "https://127.0.0.1:$port" --token-file "$h/workstation.token" --ca-file "$h/tls/cert.pem"
for c in joined alone; do
  mkdir -p "$tmp/$c"
  git -C "$tmp/$c" init -q
  tok=$h/coordinator.token
  [ "$c" = alone ] && tok=$h/worker.token
  (cd "$tmp/$c" && "$bin" wire --project upgrade-scenario --hub upgrade > /dev/null && "$bin" task-token set < "$tok")
done
(cd "$tmp/joined" && "$bin" teams setup Pilot coordinator --platform claude-code > "$tmp/setup.log" 2>&1) || true
grep -q '^Status: joined' "$tmp/setup.log" || { cat "$tmp/setup.log"; fail "$FROM's teams setup did not join"; }
echo "ok: $FROM joined team Pilot in one checkout"

# tools CHECKOUT: the names the checkout's MCP server lists, one per line,
# from a session started now (initialize, then tools/list).
tools() {
  (cd "$tmp/$1" && printf '%s\n%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"upgrade-check","version":"1"}}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | "$bin" mcp) | jq -r 'select(.id == 2) | .result.tools[].name' | sort
}
for c in joined alone; do echo "$FROM lists $(tools "$c" | wc -l | tr -d ' ') tools in the $c checkout"; done

if [ "$asset" = aimem-linux-amd64 ]; then
  echo "--- the unit at its start limit, then $TO with a binary that never answers health: the rollback must bring $FROM back"
  start_limit
  rc=0
  AIMEM_BASE="$tmp/fail-export" AIMEM_UPGRADE_WAIT=2 "$bin" upgrade --release "$TO" > "$tmp/fail-limit.log" 2>&1 || rc=$?
  cat "$tmp/fail-limit.log"
  [ "$rc" -ne 0 ] || fail "the upgrade to a binary that never serves exited 0"
  grep -qF "ROLLED BACK: aimem $FROM is running again" "$tmp/fail-limit.log" ||
    fail "the rollback from a unit at its start limit did not bring $FROM back"
  [ "$(sha "$bin")" = "$(sum_of "$asset")" ] || fail "the binary after the rollback is not $FROM's $asset"
  serving "$FROM" "after the rollback from the start limit"
fi

echo "--- $TO with a binary that never answers health: the upgrade must roll back"
rc=0
AIMEM_BASE="$tmp/fail-export" AIMEM_UPGRADE_WAIT=10 "$bin" upgrade --release "$TO" > "$tmp/fail.log" 2>&1 || rc=$?
cat "$tmp/fail.log"
[ "$rc" -ne 0 ] || fail "the upgrade to a binary that never serves exited 0"
grep -q 'ROLLED BACK' "$tmp/fail.log" || fail "the failed upgrade did not say ROLLED BACK"
if at_least "$FROM" "$SINCE"; then
  case "$(tail -n 1 "$tmp/fail.log")" in
    "aimem: the upgrade to $TO failed and was rolled back: aimem $FROM is running again"*) ;;
    *) fail "the failed upgrade did not end with the line that says it was rolled back" ;;
  esac
fi
[ "$(sha "$bin")" = "$(sum_of "$asset")" ] || fail "the binary after the rollback is not $FROM's $asset"
serving "$FROM" "after the rollback"

if [ "$asset" = aimem-linux-amd64 ]; then
  echo "--- the unit at its start limit before the upgrade"
  start_limit
fi
echo "--- aimem upgrade --release $TO, from the tree's export"
AIMEM_BASE="$tmp/export" "$bin" upgrade --release "$TO" 2>&1 | tee "$tmp/upgrade.log"
grep -qF "upgraded aimem $FROM -> $TO; service healthy at $TO" "$tmp/upgrade.log" ||
  fail "the upgrade did not report a healthy service at $TO"
[ "$(sha "$bin")" = "$(sha "$tmp/tree/aimem")" ] || fail "the installed binary is not the tree's build"
[ "$(sha "$plugin")" = "$(sha "$src/.opencode/plugin/aimem.ts")" ] || fail "the OpenCode plugin is not the tree's"
grep -qF 'aimem submit-claude' "$HOME/.claude/settings.json" || fail "the Claude Code hooks are gone after the upgrade"
serving "$TO" "after upgrade"

echo "--- the lists of a session started after the upgrade"
tools joined > "$tmp/joined.tools"
tools alone > "$tmp/alone.tools"
echo "$TO lists $(wc -l < "$tmp/joined.tools" | tr -d ' ') tools in the joined checkout, $(wc -l < "$tmp/alone.tools" | tr -d ' ') in the other"
entry='team_context team_continue team_list team_setup'
[ "$(grep '^team_' "$tmp/alone.tools" | tr '\n' ' ' | sed 's/ $//')" = "$entry" ] ||
  fail "the checkout that never joined lists other team tools than the four a join needs: $(grep '^team_' "$tmp/alone.tools" | tr '\n' ' ')"
! grep -q '^task_reservation_' "$tmp/alone.tools" || fail "the checkout that never joined lists reservation tools"
grep -qx update_task "$tmp/alone.tools" || fail "the checkout that never joined does not list the task tools"
[ -z "$(comm -13 "$tmp/joined.tools" "$tmp/alone.tools")" ] || fail "the joined checkout lacks tools the other lists"
work=$(comm -23 "$tmp/joined.tools" "$tmp/alone.tools")
[ -n "$work" ] || fail "the joined checkout lists no team work tools after the upgrade"
! echo "$work" | grep -qv '^team_' || fail "the joined checkout adds tools that are not team tools: $(echo "$work" | grep -v '^team_' | tr '\n' ' ')"
for t in team_inbox team_heartbeat team_ack team_members; do
  echo "$work" | grep -qx "$t" || fail "the joined checkout does not list $t"
done
echo "ok: the joined checkout lists $(echo "$work" | wc -l | tr -d ' ') team work tools from $FROM's record; the other lists the standalone set"

echo "PASS: client upgrade $FROM -> $TO (this tree), the rollback, and the lists after it"
