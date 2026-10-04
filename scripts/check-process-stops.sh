#!/bin/sh
# Refuses process stops by name in test, script and workflow code.
#
# On 2026-10-04 a throwaway aimem service started for CLI evidence was
# stopped by process name. The stop also ended the developer's installed
# aimem service and every MCP server that used it. A test or script stops
# only the processes it started, through the handle or PID it holds, and
# runs them under an isolated AIMEM_STATE_DIR and AIMEM_SOCKET
# (docs/DEVELOPMENT.md, "Processes and state in tests").
set -eu
cd "$(dirname "$0")/.."

pattern='(^|[^A-Za-z_-])(pkill|killall)([^A-Za-z_-]|$)|taskkill[^#]*/[Ii][Mm]|Stop-Process[^#]*-Name|Get-Process[^#|]*\|[[:space:]]*Stop-Process'

hits=$(git ls-files -- '*_test.go' 'scripts/*' '.github/workflows/*' '*.sh' '*.ps1' '*.cjs' '*.mjs' '*.js' \
	| grep -v '^scripts/check-process-stops.sh$' \
	| while IFS= read -r f; do grep -nIE "$pattern" "$f" /dev/null || true; done)

if [ -n "$hits" ]; then
	echo "process stops by name are not allowed in tests, scripts or workflows:"
	echo "$hits"
	echo
	echo "On 2026-10-04 a stop by process name ended the developer's installed aimem"
	echo "service along with the test instance. Stop only what you started, by its"
	echo "handle or PID; see docs/DEVELOPMENT.md, \"Processes and state in tests\"."
	exit 1
fi
echo "no process stops by name"
