#!/bin/sh
# process-archive.sh AIMEM CHECKOUT OUT
#
# Writes OUT/aimem-process.tar.gz: the reference process set
# (processes/aimem) at CHECKOUT's HEAD commit, in the form
# `aimem process select --from-archive` reads, with that commit recorded.
# The release workflow runs it on the tag's checkout; a test runs it on a
# repository it creates.
#
# It is `aimem process export` of the public repository, with git told to
# read that URL from CHECKOUT (url.<checkout>.insteadOf): the archive holds
# the checked-out bytes and nothing is fetched. The archive records the
# commit, not the repository; the hub names the repository given to
# `select`. The proxy variables point at a closed port, so the export's
# fallback to the forge's tarball over https fails instead of reading the
# network. The file name is fixed, so every release ships the same one;
# the importer derives nothing from it.
set -eu

aimem=$1
checkout=$(cd "$2" && pwd)
out=$3
repo=https://github.com/BlackVS/aimem
commit=$(git -C "$checkout" rev-parse HEAD)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.file://$checkout.insteadOf" GIT_CONFIG_VALUE_0="$repo" \
HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 http_proxy=http://127.0.0.1:9 NO_PROXY= no_proxy= \
  "$aimem" process export "$repo" "$commit" processes/aimem/manifest.json --out "$tmp"
short=$(printf '%.12s' "$commit")
mv "$tmp/aimem-$short.tar.gz" "$out/aimem-process.tar.gz"
