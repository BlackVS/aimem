#!/bin/sh
# Refuses a stale name in the documentation's examples.
#
# An example that names a renamed or removed command, flag, route or MCP
# tool fails for every reader who copies it. TestDocExamples in docs/
# compares each such name in the operational documents with the code:
# the cmd/aimem source, internal/server/openapi.json and the MCP tool
# list. docs/DEVELOPMENT.md, "Examples in the documentation", states what
# it reads and what it skips.
set -eu
cd "$(dirname "$0")/.."

go test -count=1 -run '^TestDocExamples$' ./docs/
