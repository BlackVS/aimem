#!/bin/sh
# Refuses tool caches committed to the repository.
#
# On 2026-10-07 the Windows installer tests ran PowerShell with a sandbox
# USERPROFILE. PowerShell found no local application data folder there and
# wrote its module analysis cache relative to the working directory, the
# test package, from where a commit carried it onto master. A cache is
# machine-local state: it never belongs in the tree.
set -eu
cd "$(dirname "$0")/.."

hits=$(git ls-files | grep -E '(^|/)ModuleAnalysisCache$|(^|/)Microsoft/Windows/PowerShell/' || true)

if [ -n "$hits" ]; then
	echo "tool caches are not allowed in the repository:"
	echo "$hits"
	echo
	echo "Remove them with git rm. A test that runs PowerShell sets"
	echo "PSModuleAnalysisCachePath and its working directory outside the tree."
	exit 1
fi
echo "no tool caches"
