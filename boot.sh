#!/bin/sh
# aimem bootstrap for Linux and macOS. A first install runs it inside a
# project directory; an upgrade runs it from anywhere.
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.0/boot.sh | bash
#
# It installs the release this script was fetched from (RELEASE below): the
# prebuilt static binary (no Go needed), checked against that release's
# SHA256SUMS, and the repository archive of the same tag for the installer
# and the OpenCode plugin. Then it runs `install.sh bootstrap` against the
# current directory: a user-level install or upgrade if aimem is missing or
# older, then wiring for this project. An upgrade backs up the state root
# first and rolls back if the new release does not come up (install.sh).
# When aimem was already installed, only a directory that already holds
# .aimem.json is wired; the home directory is never wired.
#
# Optional environment:
#   AIMEM_HUB_URL, AIMEM_HUB_TOKEN   register a hub for real-time push
#   AIMEM_GROUPS=a,b                 pre-declare shared knowledge groups
#   AIMEM_USER_ONLY=1                install or upgrade the user level only
#                                    and wire no project
#   AIMEM_REINSTALL=1                refresh the binary and hooks even if
#                                    the installed aimem is already current
#                                    (an older install is upgraded anyway)
#   AIMEM_REPO=owner/name            install from a fork
#   AIMEM_VERSION=vX.Y.Z             install another release than RELEASE
#   AIMEM_UPGRADE_WAIT=30            seconds an upgrade waits for health at
#                                    the new version before rolling back
set -e

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.10.0

for t in curl tar; do
  command -v "$t" >/dev/null 2>&1 ||
    { echo "ERROR: '$t' is required but not found - install it and re-run." >&2; exit 1; }
done

REPO=${AIMEM_REPO:-BlackVS/aimem}
BASE="https://github.com/$REPO"
TAG=${AIMEM_VERSION:-$RELEASE}
# Tell install.sh which release this is, so an older install gets upgraded.
AIMEM_TARGET_VERSION=$TAG
export AIMEM_TARGET_VERSION

DEST=$(mktemp -d)
trap 'rm -rf "$DEST"' EXIT

echo "Fetching aimem $TAG ..."
curl -fsSL "$BASE/archive/refs/tags/$TAG.tar.gz" | tar -xz -C "$DEST" --strip-components=1

# Prefer the release's prebuilt binary; fall back to building from source.
ASSET=aimem-linux-amd64
case "$(uname -s)" in Darwin) ASSET=aimem-darwin-amd64 ;; esac
case "$(uname -m)" in aarch64|arm64) ASSET=$(echo "$ASSET" | sed 's/amd64/arm64/') ;; esac
if curl -fsSL "$BASE/releases/download/$TAG/$ASSET" -o "$DEST/aimem-prebuilt"; then
  # Verify against the release's SHA256SUMS. A missing sums file or a
  # mismatch ABORTS — it must never degrade into the source-build
  # fallback, which is reserved for a release that has no binary at all.
  curl -fsSL "$BASE/releases/download/$TAG/SHA256SUMS" -o "$DEST/SHA256SUMS" || {
    echo "ERROR: release $TAG has no SHA256SUMS; refusing unverified binary." >&2
    exit 1
  }
  WANT=$(awk -v a="$ASSET" '$2==a{print $1}' "$DEST/SHA256SUMS")
  GOT=$( (sha256sum "$DEST/aimem-prebuilt" 2>/dev/null || shasum -a 256 "$DEST/aimem-prebuilt") | awk '{print $1}')
  if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
    echo "ERROR: checksum mismatch for $ASSET (want ${WANT:-absent}, got $GOT)." >&2
    exit 1
  fi
  echo "checksum OK: $ASSET"
  chmod 755 "$DEST/aimem-prebuilt"
  AIMEM_PREBUILT="$DEST/aimem-prebuilt"; export AIMEM_PREBUILT
else
  echo "NOTE: no $ASSET in release $TAG; building from source (needs Go)." >&2
  rm -f "$DEST/aimem-prebuilt"
fi

bash "$DEST/install.sh" bootstrap "$PWD"
