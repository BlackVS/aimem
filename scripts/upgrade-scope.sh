#!/usr/bin/env bash
# Whether this CI run runs the upgrade checks (client-upgrade and
# hub-upgrade in .github/workflows/ci.yml). They install into the runner's
# account, so they run where a change can break an upgrade:
#
# - on the schedule and on a manual run, always;
# - on a pull request from this repository's release/ branches (a release
#   prepare, docs/DEVELOPMENT.md "Release gate");
# - on any other pull request from this repository that changes one of the
#   files the upgrade runs or checks (the installers and the upgrade
#   checks): the pull request's own installers then run the upgrade, from
#   the previous release to this tree (scripts/upgrade-versions.sh);
# - never on a pull request from a fork, and nowhere else.
#
# It prints the reason, and writes run=true or run=false to $GITHUB_OUTPUT
# (or to stdout without it). The changed files are those between the pull
# request's base and head (git diff base...head, a full checkout);
# UPGRADE_SCOPE_FILES, one path per line, replaces that list in a test.
#
# Inputs (environment): EVENT, HEAD_REF, SAME_REPO (true or false),
# BASE_SHA, HEAD_SHA.
set -euo pipefail
cd "$(dirname "$0")/.."

# The files an upgrade runs or checks, as extended regular expressions.
installer_files='^(install\.sh|install-hub\.sh|install\.ps1|boot\.sh|boot\.ps1|scripts/check-client-upgrade\.(sh|ps1)|scripts/check-hub-upgrade\.sh|scripts/upgrade-versions\.sh|scripts/client-upgrade\.env|scripts/upgrade-scope\.sh)$'

decide() {
  case "${EVENT:-}" in
    schedule | workflow_dispatch)
      echo "run: a $EVENT run always runs the upgrade checks"
      return 0 ;;
    pull_request) ;;
    *)
      echo "skip: the upgrade checks do not run on a ${EVENT:-unnamed} event"
      return 1 ;;
  esac
  if [ "${SAME_REPO:-}" != true ]; then
    echo "skip: a pull request from a fork; the checks install into the runner's account"
    return 1
  fi
  case "${HEAD_REF:-}" in
    release/*)
      echo "run: a release prepare pull request ($HEAD_REF)"
      return 0 ;;
  esac
  local files hits
  if [ -n "${UPGRADE_SCOPE_FILES+set}" ]; then
    files=$UPGRADE_SCOPE_FILES
  else
    files=$(git diff --name-only "${BASE_SHA:?}...${HEAD_SHA:?}")
  fi
  hits=$(printf '%s\n' "$files" | grep -E "$installer_files" || true)
  if [ -n "$hits" ]; then
    echo "run: the pull request changes what the upgrade runs or checks:"
    printf '  %s\n' $hits
    return 0
  fi
  echo "skip: the pull request changes none of the installers or the upgrade checks"
  return 1
}

run=false
if decide; then run=true; fi
echo "run=$run" >> "${GITHUB_OUTPUT:-/dev/stdout}"
