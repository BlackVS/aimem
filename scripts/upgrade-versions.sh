#!/usr/bin/env bash
# The two versions the client upgrade check moves between, printed as
# FROM=vX.Y.Z and TO=vX.Y.Z lines for scripts/check-client-upgrade.sh and
# scripts/check-client-upgrade.ps1. Nothing names them by hand:
#
# FROM is the previous release: the newest vX.Y.Z tag reachable from HEAD,
#   HEAD's own tag excluded, whose release publishes a SHA256SUMS. With
#   none the script fails and says why.
# TO is the label the tree under test is built with, standing for the
#   release that does not exist yet: HEAD's own vX.Y.Z tag at a tag; the
#   version in the branch name on a release/vX.Y.Z branch; otherwise FROM's
#   next patch. It must be later than FROM.
#
# It reads the tags of the checkout, so CI checks out the full history
# (fetch-depth: 0), and the releases from GitHub.
set -euo pipefail
cd "$(dirname "$0")/.."
semver='v[0-9]+\.[0-9]+\.[0-9]+'
dl=https://github.com/BlackVS/aimem/releases/download
fail() { echo "upgrade-versions: $*" >&2; exit 1; }
# later a b: release a is later than release b.
later() {
  awk -v a="${1#v}" -v b="${2#v}" 'BEGIN { split(a, x, "."); split(b, y, ".")
    for (i = 1; i <= 3; i++) { if (x[i] + 0 != y[i] + 0) exit !(x[i] + 0 > y[i] + 0) }; exit 1 }'
}

head=$(git rev-parse --short HEAD)
[ "$(git rev-parse --is-shallow-repository)" = false ] ||
  fail "the checkout is shallow, so the tags before $head cannot be read; check out the full history (fetch-depth: 0)"
own=$(git tag --points-at HEAD | grep -Ex "$semver" | sort -V | tail -n 1 || true)

FROM=
for t in $(git tag --merged HEAD --list 'v*' --sort=-v:refname | grep -Ex "$semver"); do
  [ "$t" = "$own" ] && continue
  if curl -fsSIL -o /dev/null "$dl/$t/SHA256SUMS"; then FROM=$t; break; fi
  echo "upgrade-versions: $t has no published SHA256SUMS; trying the tag before it" >&2
done
[ -n "$FROM" ] || fail "no vX.Y.Z tag reachable from $head${own:+ other than its own $own} has a published release (its SHA256SUMS), so there is no previous release to upgrade from"

branch=${GITHUB_HEAD_REF:-${GITHUB_REF_NAME:-$(git branch --show-current)}}
if [ -n "$own" ]; then
  TO=$own
elif echo "$branch" | grep -Eqx "release/$semver"; then
  TO=${branch#release/}
else
  IFS=. read -r ma mi pa <<EOF
${FROM#v}
EOF
  TO=v$ma.$mi.$((pa + 1))
fi
later "$TO" "$FROM" || fail "the tree's version $TO is not later than the previous release $FROM"
echo "FROM=$FROM"
echo "TO=$TO"
