#!/bin/sh
# aimem bootstrap for Linux and macOS. A first install runs it inside a
# project directory; an upgrade runs it from anywhere.
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.22/boot.sh | bash
#
# In a closed network, from an aiforge export (a local directory or an
# internal https server holding the release files):
#
#   AIMEM_BASE=/media/aiforge-export bash /media/aiforge-export/aimem/<tag>/boot.sh
#
# It installs the release this script was fetched from (RELEASE below), or
# the one the export's MANIFEST names: the prebuilt static binary, the
# installer (install.sh) and the OpenCode plugin, each a release asset
# checked against that release's SHA256SUMS before it is used. Then it
# runs `install.sh bootstrap` against the current directory: a user-level
# install or upgrade if aimem is missing or older, then wiring for this
# project. An upgrade backs up the state root first and rolls back if the
# new release does not come up (install.sh). When aimem was already
# installed, only a directory that already holds .aimem.json is wired; the
# home directory is never wired.
#
# A release named by AIMEM_VERSION that was published before its installer
# files became release assets (its SHA256SUMS lists no install.sh) is
# installed by that release's own boot.sh, fetched from the repository at
# its tag and run with the same environment, as its own one-liner would.
# An export holds only releases with the assets, so with AIMEM_BASE there
# is no hand-over.
#
# Optional environment:
#   AIMEM_HUB_URL, AIMEM_HUB_TOKEN   register a hub for real-time push
#   AIMEM_GROUPS=a,b                 pre-declare shared knowledge groups
#   AIMEM_USER_ONLY=1                install or upgrade the user level only
#                                    and wire no project
#   AIMEM_REINSTALL=1                refresh the binary and hooks even if
#                                    the installed aimem is already current
#                                    (an older install is upgraded anyway)
#   AIMEM_BASE=/abs/dir|https://...  install from an aiforge export instead
#                                    of github.com: <base>/MANIFEST names
#                                    the release, <base>/aimem/<tag>/ holds
#                                    its files; no other host is contacted
#   AIMEM_REPO=owner/name            install from a fork
#   AIMEM_VERSION=vX.Y.Z             install another release than RELEASE
#                                    (or than the export's MANIFEST names)
#   AIMEM_UPGRADE_WAIT=30            seconds an upgrade waits for health at
#                                    the new version before rolling back
#
# Everything runs from main, called on the last line, so a download cut
# short runs nothing.
set -e

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.10.22

# BEGIN release-source
# Where the release's files come from, and their verification. boot.sh
# and install-hub.sh carry the same block (internal/installer keeps the
# copies equal).
# Connected: the release download URL on github.com. With AIMEM_BASE: an
# aiforge export, format 1 — <base>/MANIFEST is `aiforge-export 1` and
# then `<product> <tag>` lines, <base>/aimem/<tag>/ holds the release's
# assets under their published names with its SHA256SUMS. An explicit
# AIMEM_VERSION wins over MANIFEST. Every file is checked against
# SHA256SUMS before it is used; any failure stops with the path and
# installs nothing. The caller defines fetch (an https download to -o).
die() { echo "ERROR: $*; nothing was installed." >&2; exit 1; }
get_file() { # source dest
  case "$1" in
    https://*) fetch "$1" -o "$2" ;;
    *) [ -f "$1" ] && cp "$1" "$2" ;;
  esac
}
valid_tag() {
  case "$1" in
    v[0-9]*) case "$1" in */* | *..* | *[!A-Za-z0-9._+-]*) return 1 ;; esac ;;
    *) return 1 ;;
  esac
}
resolve_source() { # workdir: sets TAG and SRC
  if [ -z "${AIMEM_BASE:-}" ]; then
    TAG=${AIMEM_VERSION:-$RELEASE}
    valid_tag "$TAG" || die "'$TAG' is not a release tag"
    SRC="https://github.com/$REPO/releases/download/$TAG"
    return 0
  fi
  case "$AIMEM_BASE" in
    https://* | /*) ;;
    *) die "AIMEM_BASE must be an absolute directory or an https:// URL, not '$AIMEM_BASE'" ;;
  esac
  rs_base=${AIMEM_BASE%/}
  if [ -n "${AIMEM_VERSION:-}" ]; then
    TAG=$AIMEM_VERSION
  else
    get_file "$rs_base/MANIFEST" "$1/MANIFEST" ||
      die "$rs_base/MANIFEST is missing (or set AIMEM_VERSION to name the release)"
    rs_format=$(head -n 1 "$1/MANIFEST" | tr -d '\r')
    [ "$rs_format" = "aiforge-export 1" ] ||
      die "$rs_base/MANIFEST is '$rs_format', not a format this installer reads (aiforge-export 1)"
    TAG=$(tail -n +2 "$1/MANIFEST" | tr -d '\r' | awk '$1 == "aimem" { print $2; exit }')
    [ -n "$TAG" ] || die "$rs_base/MANIFEST names no aimem release"
  fi
  valid_tag "$TAG" || die "'$TAG' is not a release tag"
  SRC="$rs_base/aimem/$TAG"
}
fetch_sums() { # workdir
  get_file "$SRC/SHA256SUMS" "$1/SHA256SUMS" || die "$SRC/SHA256SUMS is missing; refusing unverified files"
}
fetch_verified() { # workdir asset: the asset, checked against SHA256SUMS
  get_file "$SRC/$2" "$1/$2" ||
    die "$SRC/$2 is missing (a release published before its installer files became release assets installs with its own boot script)"
  fv_want=$(awk -v a="$2" '$2 == a { print $1 }' "$1/SHA256SUMS")
  fv_got=$( (sha256sum "$1/$2" 2>/dev/null || shasum -a 256 "$1/$2") | awk '{ print $1 }')
  if [ -z "$fv_want" ] || [ "$fv_want" != "$fv_got" ]; then
    die "checksum mismatch for $SRC/$2 (want ${fv_want:-absent}, got $fv_got)"
  fi
  echo "checksum OK: $2"
}
# END release-source

fetch() { curl -fsSL --proto =https --proto-redir =https "$@"; }

# hand_over runs the boot.sh of release $TAG, which predates the installer
# assets, from the repository at that tag.
hand_over() {
  ho_url="https://raw.githubusercontent.com/$REPO/$TAG/boot.sh"
  echo "aimem $TAG has no installer assets; handing over to that release's own boot script ($ho_url)"
  fetch "$ho_url" -o "$DEST/boot-$TAG.sh" || die "$ho_url could not be fetched"
  AIMEM_VERSION=$TAG bash "$DEST/boot-$TAG.sh"
}

main() {
  for t in curl awk; do
    command -v "$t" >/dev/null 2>&1 ||
      { echo "ERROR: '$t' is required but not found - install it and re-run." >&2; exit 1; }
  done
  REPO=${AIMEM_REPO:-BlackVS/aimem}
  DEST=$(mktemp -d)
  trap 'rm -rf "$DEST"' EXIT

  resolve_source "$DEST"
  # Tell install.sh which release this is, so an older install gets upgraded.
  AIMEM_TARGET_VERSION=$TAG
  export AIMEM_TARGET_VERSION
  echo "Fetching aimem $TAG from $SRC ..."

  ASSET=aimem-linux-amd64
  case "$(uname -s)" in Darwin) ASSET=aimem-darwin-amd64 ;; esac
  case "$(uname -m)" in aarch64 | arm64) ASSET=$(echo "$ASSET" | sed 's/amd64/arm64/') ;; esac
  fetch_sums "$DEST"
  if [ -z "${AIMEM_BASE:-}" ] && ! awk '$2 == "install.sh" { f = 1 } END { exit !f }' "$DEST/SHA256SUMS"; then
    hand_over
    return
  fi
  for f in "$ASSET" install.sh aimem-opencode.ts; do
    fetch_verified "$DEST" "$f"
  done
  # install.sh reads the plugin beside itself, where a checkout has it.
  mkdir -p "$DEST/.opencode/plugin"
  mv "$DEST/aimem-opencode.ts" "$DEST/.opencode/plugin/aimem.ts"
  mv "$DEST/$ASSET" "$DEST/aimem-prebuilt"
  chmod 755 "$DEST/aimem-prebuilt"
  AIMEM_PREBUILT="$DEST/aimem-prebuilt"
  export AIMEM_PREBUILT

  bash "$DEST/install.sh" bootstrap "$PWD"
}

main "$@"
