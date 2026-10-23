#!/bin/sh
# Refuses an unmarked mention of an old release in the documentation.
#
# On 2026-10-23 several guides still stated v0.7.x minimums, long after task
# credentials and the team flow needed 0.10.x. A version in a current guide
# reads as the current minimum. So a mention of v0.6 to v0.9 is allowed only
# where it is clearly not one: in CHANGELOG.md, docs/ROADMAP.md and
# docs/archive/, or in a file whose first ten lines carry the marker
# "**Historical versions.**".
set -eu
cd "$(dirname "$0")/.."

marker='**Historical versions.**'
bad=""
for f in $(git grep -lE 'v0\.(6|7|8|9)\.[0-9]' -- README.md 'docs/*.md' ':!docs/archive/' ':!docs/ROADMAP.md'); do
	if head -n 10 "$f" | grep -qF "$marker"; then
		continue
	fi
	bad="$bad$(git grep -nE 'v0\.(6|7|8|9)\.[0-9]' -- "$f")
"
done

if [ -n "$bad" ]; then
	echo "old release versions in current documentation:"
	printf '%s' "$bad"
	echo
	echo "Each mention must state the current minimum (with its reason), or the"
	echo "file must carry \"$marker\" in its first ten lines, or the file"
	echo "moves to docs/archive/. See docs/DEVELOPMENT.md."
	exit 1
fi
echo "no unmarked old release versions in the documentation"
