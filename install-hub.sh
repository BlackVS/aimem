#!/usr/bin/env bash
# aimem hub installer: stand up a complete hub on a fresh Debian/Ubuntu
# host (LXC or VM). Run AS ROOT on the hub host:
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.14/install-hub.sh | bash
#
# What it does: creates the service user (with systemd linger), installs
# the release this script was fetched from (RELEASE below), checked
# against that release's SHA256SUMS, writes ~/.config/aimem/env, installs
# the serve unit + hourly curation timer (+ memory caps), starts
# everything, and prints the health check and how to configure clients:
# each with its own writer token, and the self-signed certificate's pin.
# The env token is this host's admin credential and is never printed.
# Idempotent: an existing env file is never overwritten.
#
# Upgrades: re-run the one-liner of the newer release. On an existing hub
# it stops this hub's service and curation, copies the state root (without
# the socket) to <state root>.backup-<UTC time>, swaps the binary, and
# waits for health at the new version. If the new release does not come
# up, the previous binary and the state copy are both put back.
#
# Environment knobs:
#   AIMEM_HUB_USER=sessiond        service account (created if missing)
#   AIMEM_HTTP_LISTEN=:8440        listen address
#   AIMEM_HTTP_TOKEN=...           this host's admin bearer (generated if
#                                  unset); it belongs on no client
#   AIMEM_HUB_NAME=...             display name in the admin console and
#                                  browser tab (default: this hostname)
#   AIMEM_DOMAIN=hub.example.com   generate a self-signed cert for this name
#                                  into ~/.config/aimem/tls/ and serve TLS;
#                                  clients then connect with `aimem hub add
#                                  ... --pin sha256-...` (printed at the end)
#                                  until a real certificate replaces it
#   AIMEM_TLS_CERT= AIMEM_TLS_KEY= explicit cert paths (override AIMEM_DOMAIN;
#                                  both empty and no domain = plain HTTP)
#   AIMEM_OPENAI_API_KEY=          enable nightly curation + embeddings
#   AIMEM_OPENAI_BASE_URL=         OpenAI-compatible endpoint (the vendor
#                                  API, or a LiteLLM / vLLM / Ollama proxy)
#   AIMEM_CURATE_MODEL=gpt-4o-mini
#   AIMEM_REPO=owner/name          install from a fork
#   AIMEM_VERSION=vX.Y.Z           install another release than RELEASE
#   AIMEM_UPGRADE_WAIT=30          seconds to wait for health after a swap
#   AIMEM_EMBED_MODEL="Text Embedding 3 Large"
#   AIMEM_PREBUILT=                path to a prebuilt binary (skips download)
#   AIMEM_PREBUILT_DIR=            a directory holding aimem-linux-<arch> and
#                                  SHA256SUMS under the release names: the
#                                  binary is verified, nothing is downloaded
#   AIMEM_NO_SERVICE=1             install-only: no systemd unit, linger or
#                                  start; a supervisor of your choice runs it
#
# Install-only mode (AIMEM_NO_SERVICE=1), for a host or container without
# systemd: the same binary, env file, certificate and curation script,
# without linger, the systemd units, a start or the health check, and no
# systemctl or loginctl call. The last line printed is the command a
# supervisor runs to start the hub. An upgrade refuses while the hub
# answers on its configured address, since nothing here can stop or start
# it: stop it, run the installer again, then start it.
set -euo pipefail

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.10.14

[ "$(id -u)" = 0 ] || { echo "ERROR: run as root." >&2; exit 1; }
NO_SERVICE=""
[ "${AIMEM_NO_SERVICE:-}" != 1 ] || NO_SERVICE=1
for t in curl; do
  command -v "$t" >/dev/null 2>&1 || { echo "ERROR: '$t' is required — apt-get install -y $t" >&2; exit 1; }
done

HUB_USER=${AIMEM_HUB_USER:-sessiond}
LISTEN=${AIMEM_HTTP_LISTEN:-:8440}
REPO=${AIMEM_REPO:-BlackVS/aimem}
BASE="https://github.com/$REPO"
fetch() { curl -fsSL "$@"; }

# BEGIN git-check
# The hub service never runs git, but the admin commands that read a
# process set on this host do (aimem process select and refresh, which an
# upgrade runs). Without git they read the forge's commit tarball over
# https instead. Said at the start, not after the upgrade; never fatal.
check_git() {
  command -v git >/dev/null 2>&1 && return 0
  echo "WARNING: git is not installed on this host. aimem process select and refresh (run after an upgrade) will read process sets from the forge's commit tarball over https instead. A private process repository needs AIMEM_PROCESS_FORGE_TOKEN in ~/.config/aimem/env; one reached only over ssh, or on a forge this host cannot reach over https, needs the archive import: aimem process export on a machine with access, then aimem process refresh -p <project> --from-archive <file> here. The hub service itself needs neither. To use git: apt-get install -y git" >&2
  return 0
}
# END git-check
check_git

# --- service user -----------------------------------------------------------
if ! id "$HUB_USER" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$HUB_USER"
  echo "created user $HUB_USER"
fi
[ -n "$NO_SERVICE" ] || loginctl enable-linger "$HUB_USER"
HOME_DIR=$(getent passwd "$HUB_USER" | cut -d: -f6)
as_user() { runuser -u "$HUB_USER" -- "$@"; }
sysuser() { runuser -u "$HUB_USER" -- env "XDG_RUNTIME_DIR=/run/user/$(id -u "$HUB_USER")" systemctl --user "$@"; }

# BEGIN upgrade-transaction
# The upgrade transaction, kept identical in install.sh and install-hub.sh
# (internal/installer checks the two copies match). A schema move is
# one-way, so the state root is copied before the new binary first opens
# it, and a new binary that does not come up healthy at its own version is
# undone: the previous binary and the state copy both go back.
#
# The caller defines svc_stop and svc_start (this installation's service
# and timers, nothing else), aimem_as (runs a binary as the service's user,
# with the service's environment), AIMEM_BIN (the installed binary),
# STATE_ROOT_DEFAULT (the state root when neither the service nor the
# installed binary names one) and TXN_MANAGED=1 when svc_start really
# starts a service it can wait for.
txn_field() { # binary field: a string field of the running service's health
  aimem_as "$1" health 2>/dev/null | sed -n 's/.*"'"$2"'": *"\([^"]*\)".*/\1/p' | head -n 1
}
txn_copy() { # src dst: copy a state root without the service socket, modes kept
  (umask 077 && mkdir -p "$2") && (cd "$1" && tar -cf - --exclude=aimem.sock .) | (cd "$2" && tar -xpf -)
}
# txn_backup src dst: the pre-upgrade copy, taken with the service stopped.
# Another process of the service's user (an aimem command, a curation run
# started by hand) can still write the state root. A copy that changed
# while tar read it is never kept, not even when tar only warns ("file
# changed as we read it", exit 1): a database copied mid-write can be torn,
# and a torn backup is worse than none. Each try starts from an empty
# directory. The installer does not run lsof itself: on macOS a `lsof +D`
# could stall the upgrade for minutes, so the hint names the command.
txn_backup() {
  local try=1
  while :; do
    rm -rf "$2"
    if txn_copy "$1" "$2"; then return 0; fi
    [ "$try" -lt 3 ] || break
    echo "==> the copy failed (try $try of 3); copying again" >&2
    sleep 2
    try=$((try + 1))
  done
  rm -rf "$2"
  echo "If tar says a file changed as it read it, a process of the service's user writes $1 (an aimem command, a curation run; lsof +D $1 lists them): stop it, then re-run." >&2
  return 1
}
txn_wait() { # version: wait until the service answers health at that version
  local i=0 n=$(( ${AIMEM_UPGRADE_WAIT:-30} * 2 )) v
  while [ "$i" -lt "$n" ]; do
    v=$(txn_field "$AIMEM_BIN" version)
    if [ -n "$v" ] && { [ -z "$1" ] || [ "$v" = "$1" ]; }; then return 0; fi
    sleep 0.5
    i=$((i + 1))
  done
  return 1
}
upgrade_txn() { # new-binary
  local new=$1 old_v new_v root ts t0 backup="" prev="$AIMEM_BIN.prev"
  new_v=$(aimem_as "$new" version 2>/dev/null | awk '{print $2}') || true
  old_v=$(aimem_as "$AIMEM_BIN" version 2>/dev/null | awk '{print $2}') || true
  # The running service knows its state root; a stopped one's binary
  # resolves it the same way it would (environment, then its env file).
  root=$(txn_field "$AIMEM_BIN" state_root) || true
  [ -n "$root" ] || root=$(aimem_as "$AIMEM_BIN" state-root 2>/dev/null | head -n 1) || true
  if [ "$TXN_MANAGED" != 1 ] && [ -n "$(txn_field "$AIMEM_BIN" status)" ]; then
    echo "ERROR: an aimem serve this installer does not manage is running; stop it, then re-run." >&2
    return 1
  fi
  [ -n "$root" ] || root=$STATE_ROOT_DEFAULT
  ts=$(date -u +%Y%m%dT%H%M%SZ)
  printf '==> upgrading aimem %s -> %s (state root %s)\n' "${old_v:-unknown}" "${new_v:-unknown}" "$root"
  [ "$TXN_MANAGED" != 1 ] || [ ! -d "$root" ] || printf '==> stopping the service for the state backup (%s MiB of state)\n' "$(du -sm "$root" 2>/dev/null | awk '{print $1}')"
  svc_stop
  if [ "$TXN_MANAGED" = 1 ] && [ -n "$(txn_field "$AIMEM_BIN" status)" ]; then
    svc_start
    echo "ERROR: an aimem serve still answers after the service was stopped (one started by hand?); stop it, then re-run. Nothing was changed." >&2
    return 1
  fi
  if [ -d "$root" ]; then
    backup="$root.backup-$ts"
    t0=$(date +%s)
    if ! txn_backup "$root" "$backup"; then
      svc_start
      echo "ERROR: could not copy the state root to $backup; nothing was changed." >&2
      return 1
    fi
    printf '==> state backup: %s (%ss)\n' "$backup" "$(( $(date +%s) - t0 ))"
  fi
  cp -p "$AIMEM_BIN" "$prev" && mv "$new" "$AIMEM_BIN" || {
    svc_start
    echo "ERROR: could not swap $AIMEM_BIN; the previous binary is still in place." >&2
    return 1
  }
  if [ "$TXN_MANAGED" != 1 ]; then
    rm -f "$prev"
    printf '==> upgraded aimem %s -> %s; no managed service here: start `aimem serve` yourself.\n' "${old_v:-unknown}" "${new_v:-unknown}"
    [ -z "$backup" ] || printf '==> to undo: put the previous release back and replace %s with %s\n' "$root" "$backup"
    return 0
  fi
  svc_start
  if txn_wait "$new_v"; then
    rm -f "$prev"
    printf '==> upgraded aimem %s -> %s; service healthy at %s\n' "${old_v:-unknown}" "$new_v" "$new_v"
    [ -z "$backup" ] || printf '==> the state backup stays at %s (remove it once satisfied)\n' "$backup"
    return 0
  fi
  echo "ERROR: aimem ${new_v:-(new)} did not answer health at its version within ${AIMEM_UPGRADE_WAIT:-30}s; rolling back." >&2
  svc_stop
  mv "$prev" "$AIMEM_BIN"
  if [ -n "$backup" ]; then
    if ! { mv "$root" "$root.failed-$ts" && txn_copy "$backup" "$root"; }; then
      echo "ERROR: the previous binary is back, but the state root could not be restored: copy $backup to $root by hand before starting the service." >&2
      return 1
    fi
  fi
  svc_start
  if txn_wait "$old_v"; then
    echo "ROLLED BACK: aimem ${old_v:-unknown} is running again on the state copied before the upgrade${backup:+ ($backup; the state the failed upgrade left is in $root.failed-$ts)}." >&2
  else
    echo "ROLLED BACK: the previous binary and state are restored${backup:+ from $backup}, but the service does not answer health; check it." >&2
  fi
  return 1
}
# END upgrade-transaction

# BEGIN hub-service-hooks
# The hub's service hooks for upgrade_txn: this hub's serve unit and its
# curation, run as the service user. They read no file: on a fresh host
# nothing of the service user's configuration exists yet.
AIMEM_BIN="$HOME_DIR/.local/bin/aimem"
TXN_MANAGED=1
svc_stop() { sysuser stop aimem-curate.timer aimem-curate.service aimem.service 2>/dev/null || true; }
svc_start() {
  sysuser restart aimem.service || true
  sysuser start aimem-curate.timer || true
}
aimem_as() { runuser -u "$HUB_USER" -- env "HOME=$HOME_DIR" "XDG_RUNTIME_DIR=/run/user/$(id -u "$HUB_USER")" "$@"; }
# Install-only mode: no service to stop or start, and no user manager, so
# no XDG_RUNTIME_DIR. The serve a supervisor starts (SERVE_CMD) has none
# either, and the socket path follows it: both sides must agree.
SERVE_CMD="runuser -u $HUB_USER -- sh -c 'umask 077 && exec env -u XDG_RUNTIME_DIR HOME=$HOME_DIR PATH=$HOME_DIR/.local/bin:/usr/local/bin:/usr/bin:/bin $AIMEM_BIN serve'"
if [ -n "${NO_SERVICE:-}" ]; then
  TXN_MANAGED=0
  svc_stop() { :; }
  svc_start() { :; }
  aimem_as() { runuser -u "$HUB_USER" -- env -u XDG_RUNTIME_DIR "HOME=$HOME_DIR" "$@"; }
fi
STATE_ROOT_DEFAULT="$HOME_DIR/.local/state/aimem"
# END hub-service-hooks

# BEGIN no-service-preflight
# no_service_preflight env-file: an install-only upgrade cannot stop the
# hub, and nothing would start the new release after the swap, so it
# refuses while anything answers HTTP on the address the kept env file
# names. Any status counts: a wrong token still means a hub is running.
# It runs before anything is staged, so a refusal changes nothing.
no_service_preflight() {
  local listen cert scheme=http host port code
  [ -f "$1" ] || return 0
  listen=$(sed -n 's/^AIMEM_HTTP_LISTEN=//p' "$1" | tr -d '"')
  cert=$(sed -n 's/^AIMEM_TLS_CERT=//p' "$1" | tr -d '"')
  [ -z "$cert" ] || scheme=https
  port=${listen##*:}
  host=${listen%:*}
  case "$host" in ""|0.0.0.0|"[::]") host=127.0.0.1 ;; esac
  [ -n "$port" ] || return 0
  code=$(curl -sk -m 5 -o /dev/null -w '%{http_code}' "$scheme://$host:$port/v1/health" 2>/dev/null) || true
  if [ -n "$code" ] && [ "$code" != 000 ]; then
    echo "ERROR: aimem answers at $scheme://$host:$port (AIMEM_HTTP_LISTEN=$listen), and with AIMEM_NO_SERVICE=1 this installer does not stop it. Stop it, run this again, then start it. Nothing was changed." >&2
    return 1
  fi
}
# END no-service-preflight
if [ -n "$NO_SERVICE" ] && [ -e "$AIMEM_BIN" ]; then
  no_service_preflight "$HOME_DIR/.config/aimem/env" || exit 1
fi

# BEGIN process-refresh
# After an upgrade: store the process sets of the selections made before
# the hub stored sets, from their recorded repository, commit, manifest and
# ref, so an upgrade needs no manual step. The report is printed per
# project; a set Git cannot read is reported with the commands that supply
# it, and nothing here fails the upgrade.
refresh_processes() { # aimem-binary
  echo "==> process sets: aimem process refresh"
  if ! aimem_as "$1" process refresh; then
    echo "WARNING: aimem process refresh did not complete; as $HUB_USER on this host run: aimem process refresh" >&2
  fi
}
# END process-refresh

# --- binary -----------------------------------------------------------------
# BEGIN binary-stage
# stage_binary dest: the binary this run installs. AIMEM_PREBUILT is the
# operator's own file, installed as given. The release asset, from
# AIMEM_PREBUILT_DIR or downloaded, is verified against the SHA256SUMS
# beside it: this script runs as root, so an unverified binary is the
# worst place to skip the check. Any failure removes the staged file and
# aborts before anything is installed. The caller defines fetch, BASE and
# RELEASE; the version installed is the staged binary's own (upgrade_txn).
verify_asset() { # binary sums asset
  local want got
  want=$(awk -v a="$3" '$2==a{print $1}' "$2")
  got=$(sha256sum "$1" | awk '{print $1}')
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "ERROR: checksum mismatch for $3 (want ${want:-absent}, got $got)." >&2
    return 1
  fi
  echo "checksum OK: $3"
}
stage_binary() { # dest
  local dest=$1 asset=aimem-linux-amd64 dir tag ok=0
  case "$(uname -m)" in aarch64|arm64) asset=aimem-linux-arm64 ;; esac
  if [ -n "${AIMEM_PREBUILT:-}" ] && [ -n "${AIMEM_PREBUILT_DIR:-}" ]; then
    echo "ERROR: AIMEM_PREBUILT and AIMEM_PREBUILT_DIR are both set; set only one of them." >&2
    exit 1
  fi
  if [ -n "${AIMEM_PREBUILT:-}" ]; then
    install -m 755 "$AIMEM_PREBUILT" "$dest"
    return 0
  fi
  if [ -n "${AIMEM_PREBUILT_DIR:-}" ]; then
    dir=$AIMEM_PREBUILT_DIR
    echo "installing aimem from $dir ($asset)"
    [ -f "$dir/$asset" ] || { echo "ERROR: $dir has no $asset; nothing was installed." >&2; exit 1; }
    [ -f "$dir/SHA256SUMS" ] || { echo "ERROR: $dir has no SHA256SUMS; refusing unverified binary." >&2; exit 1; }
    cp "$dir/$asset" "$dest"
    verify_asset "$dest" "$dir/SHA256SUMS" "$asset" || ok=1
  else
    tag=${AIMEM_VERSION:-$RELEASE}
    echo "installing aimem $tag ($asset)"
    fetch "$BASE/releases/download/$tag/$asset" -o "$dest"
    fetch "$BASE/releases/download/$tag/SHA256SUMS" -o "$dest.sums" || {
      echo "ERROR: release $tag has no SHA256SUMS; refusing unverified binary." >&2
      rm -f "$dest" "$dest.sums"; exit 1
    }
    verify_asset "$dest" "$dest.sums" "$asset" || ok=1
    rm -f "$dest.sums"
  fi
  [ "$ok" = 0 ] || { rm -f "$dest"; exit 1; }
  chmod 755 "$dest"
}
# END binary-stage
mkdir -p "$HOME_DIR/.local/bin" "$HOME_DIR/.local/sbin"
stage_binary "$HOME_DIR/.local/bin/aimem.new"

# --- self-signed TLS (optional, until a real cert is enrolled) ---------------
if [ -n "${AIMEM_DOMAIN:-}" ] && [ -z "${AIMEM_TLS_CERT:-}" ]; then
  TLS_DIR="$HOME_DIR/.config/aimem/tls"
  if [ ! -f "$TLS_DIR/selfsigned.pem" ]; then
    mkdir -p "$TLS_DIR"
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
      -keyout "$TLS_DIR/selfsigned.key" -out "$TLS_DIR/selfsigned.pem" \
      -days 3650 -nodes -subj "/CN=$AIMEM_DOMAIN" \
      -addext "subjectAltName=DNS:$AIMEM_DOMAIN" 2>/dev/null
    chmod 600 "$TLS_DIR/selfsigned.key"
    echo "generated self-signed cert for $AIMEM_DOMAIN (replace with a real one when enrolled)"
  fi
  AIMEM_TLS_CERT="$TLS_DIR/selfsigned.pem"
  AIMEM_TLS_KEY="$TLS_DIR/selfsigned.key"
fi

# --- env file (never overwritten) -------------------------------------------
ENVF="$HOME_DIR/.config/aimem/env"
if [ ! -f "$ENVF" ]; then
  TOKEN=${AIMEM_HTTP_TOKEN:-$(head -c 32 /dev/urandom | sha256sum | cut -c1-48)}
  mkdir -p "$(dirname "$ENVF")"
  {
    echo "AIMEM_HTTP_LISTEN=$LISTEN"
    echo "AIMEM_HTTP_TOKEN=$TOKEN"
    [ -n "${AIMEM_TLS_CERT:-}" ] && echo "AIMEM_TLS_CERT=$AIMEM_TLS_CERT"
    [ -n "${AIMEM_TLS_KEY:-}" ]  && echo "AIMEM_TLS_KEY=$AIMEM_TLS_KEY"
    [ -n "${AIMEM_CURATE_BACKEND:-}" ] && echo "AIMEM_CURATE_BACKEND=$AIMEM_CURATE_BACKEND"
    [ -n "${AIMEM_HUB_NAME:-}" ] && echo "AIMEM_HUB_NAME=\"$AIMEM_HUB_NAME\""
    if [ -n "${AIMEM_OPENAI_API_KEY:-}" ]; then
      echo "AIMEM_OPENAI_API_KEY=$AIMEM_OPENAI_API_KEY"
      echo "AIMEM_OPENAI_BASE_URL=${AIMEM_OPENAI_BASE_URL:-https://api.openai.com/v1}"
      echo "AIMEM_CURATE_MODEL=\"${AIMEM_CURATE_MODEL:-gpt-4o-mini}\""
      echo "AIMEM_EMBED_MODEL=\"${AIMEM_EMBED_MODEL:-text-embedding-3-large}\""
    fi
    # claude backend: install the Claude Code CLI on this host and add
    # CLAUDE_CODE_OAUTH_TOKEN=<token from `claude setup-token`> here.
  } > "$ENVF"
  chmod 600 "$ENVF"
  echo "wrote $ENVF"
else
  TOKEN=$(sed -n 's/^AIMEM_HTTP_TOKEN=//p' "$ENVF")
  echo "kept existing $ENVF"
fi

# --- curation script + systemd user units -----------------------------------
cat > "$HOME_DIR/.local/sbin/curate-all.sh" <<'EOS'
#!/usr/bin/env bash
# Hourly knowledge curation; the 04:xx UTC run also dedups and
# regenerates design docs for groups with feature doc enabled.
# Backend from the unit's EnvironmentFile: openai (LiteLLM/OpenAI key)
# or claude (headless Claude Code CLI, subscription-covered).
# Embedding steps only run when an embed model is configured.
set -uo pipefail
export PATH="$HOME/.local/bin:$PATH"
BACKEND=${AIMEM_CURATE_BACKEND:-openai}
aimem curate --backend "$BACKEND" --all
[ -n "${AIMEM_EMBED_MODEL:-}" ] && aimem embed --all
if [ "$(date -u +%H)" = "04" ]; then
  [ -n "${AIMEM_EMBED_MODEL:-}" ] && aimem dedup --all
  aimem doc --all --backend "$BACKEND"
fi
exit 0
EOS
chmod 755 "$HOME_DIR/.local/sbin/curate-all.sh"

# Install-only mode writes no unit: nothing would read it.
if [ -z "$NO_SERVICE" ]; then
UNITS="$HOME_DIR/.config/systemd/user"
mkdir -p "$UNITS/aimem.service.d" "$UNITS/aimem-curate.service.d"
cat > "$UNITS/aimem.service" <<'EOS'
[Unit]
Description=aimem - session journal hub

[Service]
EnvironmentFile=-%h/.config/aimem/env
# ~/.local/bin on PATH: the serve process execs the claude CLI for
# kind=claude provider tests (curate-all.sh exports this itself).
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/.local/bin/aimem serve
Restart=on-failure
RestartSec=2
UMask=0077

[Install]
WantedBy=default.target
EOS
cat > "$UNITS/aimem-curate.service" <<'EOS'
[Unit]
Description=aimem daily knowledge curation (LiteLLM backend)

[Service]
Type=oneshot
EnvironmentFile=-%h/.config/aimem/env
ExecStart=%h/.local/sbin/curate-all.sh
EOS
cat > "$UNITS/aimem-curate.timer" <<'EOS'
[Unit]
Description=hourly aimem curation (deep pass at 04:xx UTC)

[Timer]
OnCalendar=*-*-* *:15:00
OnStartupSec=5min
RandomizedDelaySec=5min
Persistent=true

[Install]
WantedBy=timers.target
EOS
for d in aimem.service.d aimem-curate.service.d; do
  cat > "$UNITS/$d/memory.conf" <<'EOS'
[Service]
MemoryHigh=256M
MemoryMax=512M
EOS
done
fi
chown -R "$HUB_USER:$HUB_USER" "$HOME_DIR/.local" "$HOME_DIR/.config"

[ -n "$NO_SERVICE" ] || sysuser daemon-reload
[ -n "$NO_SERVICE" ] || sysuser enable aimem.service aimem-curate.timer >/dev/null 2>&1 || true
UPGRADED=
if [ -e "$AIMEM_BIN" ]; then
  UPGRADED=1
  upgrade_txn "$AIMEM_BIN.new" || exit 1
else
  mv "$AIMEM_BIN.new" "$AIMEM_BIN"
  if [ -z "$NO_SERVICE" ]; then
    sysuser restart aimem.service   # restart, not start: a stale binary may be running
    sysuser start aimem-curate.timer
  fi
fi

# --- verify -----------------------------------------------------------------
[ -n "$NO_SERVICE" ] || sleep 1
PORT=${LISTEN##*:}
# On an env-less re-run (upgrade) the TLS setting lives only in the kept
# env file; probing a TLS listener with plain http yields a bogus 400.
[ -z "${AIMEM_TLS_CERT:-}" ] && AIMEM_TLS_CERT=$(sed -n 's/^AIMEM_TLS_CERT=//p' "$ENVF" | tr -d '"')
SCHEME=http; [ -n "${AIMEM_TLS_CERT:-}" ] && SCHEME=https
HOST=${AIMEM_DOMAIN:-<this-host>}
if [ -n "$NO_SERVICE" ]; then
  : # nothing was started; the supervisor starts it with SERVE_CMD below
elif curl -fsSk -H "Authorization: Bearer $TOKEN" "$SCHEME://127.0.0.1:$PORT/v1/health" >/dev/null; then
  echo "hub is up: $SCHEME://$HOST:$PORT ($(as_user "$HOME_DIR/.local/bin/aimem" version))"
  if [ -n "$UPGRADED" ]; then
    refresh_processes "$AIMEM_BIN"
  fi
else
  echo "WARNING: health check failed — inspect: journalctl --user -u aimem (as $HUB_USER)" >&2
fi
# BEGIN client-hint
# How to configure a client. The env token is this host's admin credential:
# it is never printed and belongs on no client, so each client gets its own
# writer token. A self-signed certificate is verified by its SPKI pin, which
# is public and printed on every run.
spki_pin() {
  local pub b64
  pub=$(openssl x509 -in "$1" -noout -pubkey 2>/dev/null) || return 1
  [ -n "$pub" ] || return 1
  b64=$(printf '%s\n' "$pub" | openssl pkey -pubin -outform der 2>/dev/null | openssl dgst -sha256 -binary | base64) || return 1
  [ "${#b64}" -eq 44 ] || return 1
  printf 'sha256-%s\n' "$b64"
}
TRUST=""
SELF_SIGNED=""
case "${AIMEM_TLS_CERT:-}" in */selfsigned.pem) SELF_SIGNED=1 ;; esac
if [ -n "$SELF_SIGNED" ] && PIN=$(spki_pin "$AIMEM_TLS_CERT"); then
  TRUST=" --pin $PIN"
fi
echo
echo "Configure each client with its own writer token:"
echo "  1. on this host, as $HUB_USER:  aimem token add <machine>    (prints that machine's token once)"
echo "  2. on the client:  aimem hub add <name> $SCHEME://$HOST:$PORT \"<writer-token>\"$TRUST"
echo "  AIMEM_HTTP_TOKEN in $ENVF is this host's admin credential: keep it on this host, never on a client."
if [ -n "$TRUST" ]; then
  echo "  --pin verifies the self-signed certificate. --insecure would skip verification: use it only as a last resort."
  echo "  When a real certificate replaces the self-signed one, the pin stops matching: on each client run"
  echo "  aimem hub add <name> $SCHEME://$HOST:$PORT \"<writer-token>\" --system-roots, which drops the pin and keeps the rest of the entry."
elif [ -n "$SELF_SIGNED" ]; then
  echo "WARNING: could not compute the self-signed certificate's pin. Compute it on this host with" >&2
  echo "  openssl x509 -in $AIMEM_TLS_CERT -noout -pubkey | openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | base64" >&2
  echo "and give clients --pin sha256-<that value>. --insecure skips verification: use it only as a last resort." >&2
fi
if [ "$SCHEME" = http ]; then
  echo "WARNING: this hub serves plain http, so every bearer token crosses the network in clear." >&2
  echo "  Set AIMEM_DOMAIN (a self-signed certificate that clients pin) or AIMEM_TLS_CERT and AIMEM_TLS_KEY, and re-run." >&2
fi
# END client-hint

# BEGIN no-service-finish
# Install-only mode: what a supervisor runs. The serve command is the last
# line, so a caller can take it with tail -n 1.
if [ -n "${NO_SERVICE:-}" ]; then
  echo
  [ -z "$UPGRADED" ] || echo "After the hub is up, as $HUB_USER run: aimem process refresh   (stores the process sets of earlier selections)"
  echo "Curation: have the supervisor run $HOME_DIR/.local/sbin/curate-all.sh hourly as $HUB_USER, with the variables of $ENVF in its environment."
  echo "aimem is installed and not started (AIMEM_NO_SERVICE=1). Have a supervisor run, as root:"
  echo "$SERVE_CMD"
fi
# END no-service-finish
