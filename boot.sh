#!/bin/sh
# aimem bootstrap for Linux and macOS. Run it INSIDE a project directory.
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh | bash
#
# It downloads the latest release (a prebuilt static binary — no Go needed),
# unpacks the repository archive for the installer and the OpenCode plugin,
# and runs `install.sh bootstrap` against the current directory: a
# user-level install if aimem is missing, then wiring for this project.
#
# Optional environment:
#   AIMEM_HUB_URL, AIMEM_HUB_TOKEN   register a hub for real-time push
#   AIMEM_GROUPS=a,b                 pre-declare shared knowledge groups
#   AIMEM_REINSTALL=1                refresh the binary and hooks even if
#                                    aimem is already installed
#   AIMEM_REPO=owner/name            install from a fork
#   AIMEM_VERSION=vX.Y.Z             pin a release instead of the latest
set -e

for t in curl tar; do
  command -v "$t" >/dev/null 2>&1 ||
    { echo "ERROR: '$t' is required but not found - install it and re-run." >&2; exit 1; }
done

REPO=${AIMEM_REPO:-BlackVS/aimem}
BASE="https://github.com/$REPO"

# Resolve the latest tag without jq: /releases/latest redirects to the tag
# page, so the effective URL names the version. One less dependency to ask
# a new user to install.
TAG=${AIMEM_VERSION:-}
if [ -z "$TAG" ]; then
  URL=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$BASE/releases/latest" 2>/dev/null || true)
  case "$URL" in
    */releases/tag/*) TAG=${URL##*/releases/tag/} ;;
  esac
fi
[ -n "$TAG" ] || TAG=master   # no releases yet: install from the branch

DEST=$(mktemp -d)
trap 'rm -rf "$DEST"' EXIT

echo "Fetching aimem $TAG ..."
if [ "$TAG" = master ]; then
  ARCHIVE="$BASE/archive/refs/heads/master.tar.gz"
else
  ARCHIVE="$BASE/archive/refs/tags/$TAG.tar.gz"
fi
curl -fsSL "$ARCHIVE" | tar -xz -C "$DEST" --strip-components=1

# Prefer the release's prebuilt binary; fall back to building from source.
if [ "$TAG" != master ]; then
  ASSET=aimem-linux-amd64
  case "$(uname -s)" in Darwin) ASSET=aimem-darwin-amd64 ;; esac
  case "$(uname -m)" in aarch64|arm64) ASSET=$(echo "$ASSET" | sed 's/amd64/arm64/') ;; esac
  if curl -fsSL "$BASE/releases/download/$TAG/$ASSET" -o "$DEST/aimem-prebuilt"; then
    chmod 755 "$DEST/aimem-prebuilt"
    AIMEM_PREBUILT="$DEST/aimem-prebuilt"; export AIMEM_PREBUILT
  else
    echo "NOTE: no $ASSET in release $TAG; building from source (needs Go)." >&2
    rm -f "$DEST/aimem-prebuilt"
  fi
fi

bash "$DEST/install.sh" bootstrap "$PWD"
