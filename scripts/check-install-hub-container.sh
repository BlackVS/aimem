#!/usr/bin/env bash
# CI: install-hub.sh's install-only mode (AIMEM_NO_SERVICE=1) in a Debian
# container without systemd. Run as root from AIMEM_PREBUILT_DIR, so nothing
# is downloaded, a fresh install calls no systemctl or loginctl and its last
# line starts a hub that answers health. While that hub runs, an upgrade
# refuses and changes nothing. Once the hub is stopped, the upgrade goes
# through, and the printed command starts the new release.
#
# It needs docker and go, and touches nothing outside the container and a
# temporary directory.
set -euo pipefail
cd "$(dirname "$0")/.."

# debian:stable-slim, pinned by digest.
IMAGE=${IMAGE:-debian@sha256:9e3624ad785d0d322f0c69194c2c0ebba7bc5fda3e2c6523ac095b4f08a12ba7}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for v in v0.0.1 v0.0.2; do
  mkdir -p "$tmp/$v"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=$v" -o "$tmp/$v/aimem-linux-amd64" ./cmd/aimem
  printf '%s  aimem-linux-amd64\n' "$(sha256sum < "$tmp/$v/aimem-linux-amd64" | cut -d' ' -f1)" > "$tmp/$v/SHA256SUMS"
done
cp install-hub.sh "$tmp/install-hub.sh"

cat > "$tmp/run.sh" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive
apt-get -qq update >/dev/null && apt-get -qq install -y --no-install-recommends curl ca-certificates >/dev/null

# Any systemctl or loginctl call is recorded, and fails.
for t in systemctl loginctl; do
  printf '#!/bin/sh\necho "$0 $*" >> /tmp/systemd-calls\nexit 1\n' > "/usr/local/bin/$t"
  chmod 755 "/usr/local/bin/$t"
done

H=/home/sessiond
ENVF=$H/.config/aimem/env
install_hub() { # release log
  AIMEM_NO_SERVICE=1 AIMEM_PREBUILT_DIR=/rel/$1 bash /rel/install-hub.sh > "$2" 2>&1
}
health() { # prints the HTTP status of /v1/health with the env token, 000 when nothing answers
  curl -s -m 2 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(sed -n 's/^AIMEM_HTTP_TOKEN=//p' "$ENVF")" http://127.0.0.1:8440/v1/health || true
}
serve() { # log: start the hub with the installer's last line
  local cmd
  cmd=$(tail -n 1 "$1")
  case "$cmd" in "runuser -u sessiond -- "*" serve'") ;; *) fail "the last line is not the serve command: $cmd" ;; esac
  eval "$cmd" > /tmp/serve.log 2>&1 &
  for _ in $(seq 1 60); do [ "$(health)" = 200 ] && return 0; sleep 0.5; done
  cat /tmp/serve.log; fail "the printed command did not start a hub that answers /v1/health"
}
stop_hub() {
  local p
  for p in /proc/[0-9]*; do
    tr '\0' ' ' < "$p/cmdline" 2>/dev/null | grep -q '^/home/sessiond/.local/bin/aimem serve' && kill "${p#/proc/}" 2>/dev/null || true
  done
  for _ in $(seq 1 40); do [ "$(health)" = 000 ] && return 0; sleep 0.5; done
  fail "the hub did not stop"
}
version() { runuser -u sessiond -- env HOME=$H $H/.local/bin/aimem version | awk '{print $2}'; }

echo "--- fresh install"
install_hub v0.0.1 /tmp/install.log || { cat /tmp/install.log; fail "the fresh install failed"; }
cat /tmp/install.log
[ ! -e /tmp/systemd-calls ] || fail "systemd was called: $(cat /tmp/systemd-calls)"
for f in $H/.local/bin/aimem $ENVF $H/.local/sbin/curate-all.sh; do [ -e "$f" ] || fail "$f was not written"; done
[ ! -e $H/.config/systemd ] || fail "unit files were written"
[ "$(version)" = v0.0.1 ] || fail "installed $(version), want v0.0.1"
grep -q 'not started (AIMEM_NO_SERVICE=1)' /tmp/install.log || fail "no install-only notice"
serve /tmp/install.log
echo "hub answers at v0.0.1"

echo "--- upgrade while the hub runs"
# An indented listen line, which aimem reads as written, must still be found.
sed -i 's/^AIMEM_HTTP_LISTEN=/  AIMEM_HTTP_LISTEN=/' $ENVF
sum=$(sha256sum $H/.local/bin/aimem $ENVF)
curate=$(stat -c %y $H/.local/sbin/curate-all.sh)
if install_hub v0.0.2 /tmp/refused.log; then cat /tmp/refused.log; fail "the upgrade ran while the hub answers"; fi
cat /tmp/refused.log
grep -q 'ERROR: aimem answers at http://127.0.0.1:8440 (AIMEM_HTTP_LISTEN=:8440), and with AIMEM_NO_SERVICE=1 this installer does not stop it.' /tmp/refused.log || fail "the refusal does not name the running hub"
[ "$(sha256sum $H/.local/bin/aimem $ENVF)" = "$sum" ] || fail "the refused upgrade changed the binary or the env file"
[ "$(stat -c %y $H/.local/sbin/curate-all.sh)" = "$curate" ] || fail "the refused upgrade rewrote curate-all.sh"
[ ! -e $H/.local/bin/aimem.new ] || fail "the refused upgrade left a staged binary"
[ "$(health)" = 200 ] || fail "the hub stopped answering"

echo "--- upgrade with the hub stopped"
stop_hub
install_hub v0.0.2 /tmp/upgrade.log || { cat /tmp/upgrade.log; fail "the upgrade failed"; }
cat /tmp/upgrade.log
[ ! -e /tmp/systemd-calls ] || fail "systemd was called: $(cat /tmp/systemd-calls)"
grep -q 'upgraded aimem v0.0.1 -> v0.0.2' /tmp/upgrade.log || fail "no upgrade line"
ls -d $H/.local/state/aimem.backup-* >/dev/null || fail "no state backup"
[ "$(version)" = v0.0.2 ] || fail "installed $(version), want v0.0.2"
serve /tmp/upgrade.log
echo "hub answers at v0.0.2"
stop_hub
echo "PASS"
EOS

docker run --rm -v "$tmp:/rel:ro" "$IMAGE" timeout 300 bash /rel/run.sh
