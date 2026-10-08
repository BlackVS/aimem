#!/usr/bin/env bash
# aimem hub installer: stand up a complete hub on a fresh Debian/Ubuntu
# host (LXC or VM). Run AS ROOT on the hub host:
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.9.1/install-hub.sh | bash
#
# What it does: creates the service user (with systemd linger), installs
# the release this script was fetched from (RELEASE below), checked
# against that release's SHA256SUMS, writes ~/.config/aimem/env, installs
# the serve unit + hourly curation timer (+ memory caps), starts
# everything, and prints the health check + bearer token to configure
# clients with. Idempotent: an existing env file is never overwritten.
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
#   AIMEM_HTTP_TOKEN=...           bearer token (generated if unset)
#   AIMEM_HUB_NAME=...             display name in the admin console and
#                                  browser tab (default: this hostname)
#   AIMEM_DOMAIN=hub.example.com   generate a self-signed cert for this name
#                                  into ~/.config/aimem/tls/ and serve TLS;
#                                  clients then connect with `aimem hub add
#                                  ... --insecure` until a real certificate
#                                  replaces it
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
set -euo pipefail

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.9.1

[ "$(id -u)" = 0 ] || { echo "ERROR: run as root." >&2; exit 1; }
for t in curl; do
  command -v "$t" >/dev/null 2>&1 || { echo "ERROR: '$t' is required — apt-get install -y $t" >&2; exit 1; }
done

HUB_USER=${AIMEM_HUB_USER:-sessiond}
LISTEN=${AIMEM_HTTP_LISTEN:-:8440}
REPO=${AIMEM_REPO:-BlackVS/aimem}
BASE="https://github.com/$REPO"
fetch() { curl -fsSL "$@"; }

# --- service user -----------------------------------------------------------
if ! id "$HUB_USER" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$HUB_USER"
  echo "created user $HUB_USER"
fi
loginctl enable-linger "$HUB_USER"
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
  local new=$1 old_v new_v root ts backup="" prev="$AIMEM_BIN.prev"
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
  svc_stop
  if [ -d "$root" ]; then
    backup="$root.backup-$ts"
    if ! txn_copy "$root" "$backup"; then
      rm -rf "$backup"
      svc_start
      echo "ERROR: could not copy the state root to $backup; nothing was changed." >&2
      return 1
    fi
    printf '==> state backup: %s\n' "$backup"
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
STATE_ROOT_DEFAULT="$HOME_DIR/.local/state/aimem"
# END hub-service-hooks

# --- binary -----------------------------------------------------------------
mkdir -p "$HOME_DIR/.local/bin" "$HOME_DIR/.local/sbin"
if [ -n "${AIMEM_PREBUILT:-}" ]; then
  install -m 755 "$AIMEM_PREBUILT" "$HOME_DIR/.local/bin/aimem.new"
else
  TAG=${AIMEM_VERSION:-$RELEASE}
  ASSET=aimem-linux-amd64
  case "$(uname -m)" in aarch64|arm64) ASSET=aimem-linux-arm64 ;; esac
  echo "installing aimem $TAG ($ASSET)"
  fetch "$BASE/releases/download/$TAG/$ASSET" -o "$HOME_DIR/.local/bin/aimem.new"
  # Verify against the release's SHA256SUMS — this script runs as root, so
  # an unverified binary is the worst place to skip it. Any failure aborts.
  fetch "$BASE/releases/download/$TAG/SHA256SUMS" -o "$HOME_DIR/.local/bin/aimem.sums" || {
    echo "ERROR: release $TAG has no SHA256SUMS; refusing unverified binary." >&2
    rm -f "$HOME_DIR/.local/bin/aimem.new"; exit 1
  }
  WANT=$(awk -v a="$ASSET" '$2==a{print $1}' "$HOME_DIR/.local/bin/aimem.sums")
  GOT=$(sha256sum "$HOME_DIR/.local/bin/aimem.new" | awk '{print $1}')
  rm -f "$HOME_DIR/.local/bin/aimem.sums"
  if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
    echo "ERROR: checksum mismatch for $ASSET (want ${WANT:-absent}, got $GOT)." >&2
    rm -f "$HOME_DIR/.local/bin/aimem.new"; exit 1
  fi
  echo "checksum OK: $ASSET"
  chmod 755 "$HOME_DIR/.local/bin/aimem.new"
fi

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
  FRESH_ENV=1
  echo "wrote $ENVF"
else
  TOKEN=$(sed -n 's/^AIMEM_HTTP_TOKEN=//p' "$ENVF")
  FRESH_ENV=""
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
chown -R "$HUB_USER:$HUB_USER" "$HOME_DIR/.local" "$HOME_DIR/.config"

sysuser daemon-reload
sysuser enable aimem.service aimem-curate.timer >/dev/null 2>&1 || true
if [ -e "$AIMEM_BIN" ]; then
  upgrade_txn "$AIMEM_BIN.new" || exit 1
else
  mv "$AIMEM_BIN.new" "$AIMEM_BIN"
  sysuser restart aimem.service   # restart, not start: a stale binary may be running
  sysuser start aimem-curate.timer
fi

# --- verify -----------------------------------------------------------------
sleep 1
PORT=${LISTEN##*:}
# On an env-less re-run (upgrade) the TLS setting lives only in the kept
# env file; probing a TLS listener with plain http yields a bogus 400.
[ -z "${AIMEM_TLS_CERT:-}" ] && AIMEM_TLS_CERT=$(sed -n 's/^AIMEM_TLS_CERT=//p' "$ENVF" | tr -d '"')
SCHEME=http; [ -n "${AIMEM_TLS_CERT:-}" ] && SCHEME=https
HOST=${AIMEM_DOMAIN:-<this-host>}
if curl -fsSk -H "Authorization: Bearer $TOKEN" "$SCHEME://127.0.0.1:$PORT/v1/health" >/dev/null; then
  echo "hub is up: $SCHEME://$HOST:$PORT ($(as_user "$HOME_DIR/.local/bin/aimem" version))"
else
  echo "WARNING: health check failed — inspect: journalctl --user -u aimem (as $HUB_USER)" >&2
fi
EXTRA=""
case "${AIMEM_TLS_CERT:-}" in */selfsigned.pem) EXTRA=" --insecure" ;; esac
echo
echo "Configure clients with:"
if [ -n "$FRESH_ENV" ]; then
  # The secret is shown once, at creation — the same rule named tokens
  # follow. Upgrades must not re-echo a live credential into logs.
  echo "  aimem hub add <name> $SCHEME://$HOST:$PORT $TOKEN$EXTRA"
else
  echo "  aimem hub add <name> $SCHEME://$HOST:$PORT <token>$EXTRA   # token kept in $ENVF (not re-shown)"
fi
echo "  (better: mint one per machine — \`aimem token add <machine>\` as $HUB_USER on this host)"
[ -n "$EXTRA" ] && echo "  (drop --insecure after a real certificate replaces the self-signed one)"
