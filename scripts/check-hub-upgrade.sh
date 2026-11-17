#!/usr/bin/env bash
# CI: a hub's upgrade from the previous release to the tree under test.
#
# In the pinned Debian container the hub installer's container check uses,
# as root and without systemd (AIMEM_NO_SERVICE=1):
#   1. the previous release's own hub installer, fetched from its tag as the
#      published one-liner does, installs that release's published binary;
#   2. that hub writes data: a project with tasks on, a task with a
#      comment, a memory, and a team with a member;
#   3. the tree's install-hub.sh upgrades it from an export of the tree,
#      built as the release it stands for;
#   4. the tree's hub reads back what the previous release wrote. The
#      installer's state copy is the previous state at the previous schema,
#      and each project's journal is at the tree's schema: a real migration
#      when the tree's schema is newer, said so when it is the same.
#
# The versions come from scripts/upgrade-versions.sh (full history needed).
# A manual run may name the release to start from (AIMEM_UPGRADE_FROM) to
# run the migration branch now; that is no supported skip-release path.
# The systemd part of an upgrade stays with check-install-hub-container.sh.
#
# It needs docker, go and git, and touches nothing outside the container
# and a temporary directory.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C
fail() { echo "FAIL: $*" >&2; exit 1; }

# debian:stable-slim, pinned by digest, as check-install-hub-container.sh.
IMAGE=${IMAGE:-debian@sha256:9e3624ad785d0d322f0c69194c2c0ebba7bc5fda3e2c6523ac095b4f08a12ba7}
eval "$(bash scripts/upgrade-versions.sh | grep -E '^(FROM|TO)=v[0-9]+\.[0-9]+\.[0-9]+$')"
[ -n "${FROM:-}" ] && [ -n "${TO:-}" ] || fail "scripts/upgrade-versions.sh printed no FROM and TO"
schema_of() { # source of internal/store/store.go: its currentSchema
  sed -n 's/^const currentSchema = \([0-9][0-9]*\)$/\1/p'
}
from_schema=$(git show "$FROM:internal/store/store.go" | schema_of)
to_schema=$(schema_of < internal/store/store.go)
[ -n "$from_schema" ] && [ -n "$to_schema" ] || fail "cannot read the schema version of $FROM or of the tree"
[ "$to_schema" -ge "$from_schema" ] || fail "the tree's schema $to_schema is older than $FROM's $from_schema"
echo "hub upgrade: $FROM (the previous release, schema $from_schema) -> $TO (this tree, $(git rev-parse --short HEAD), schema $to_schema)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
x=$tmp/export/aimem/$TO
mkdir -p "$x"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$TO" -o "$x/aimem-linux-amd64" ./cmd/aimem
printf '%s  aimem-linux-amd64\n' "$(sha256sum < "$x/aimem-linux-amd64" | cut -d' ' -f1)" > "$x/SHA256SUMS"
printf 'aiforge-export 1\naimem %s\n' "$TO" > "$tmp/export/MANIFEST"
cp install-hub.sh "$tmp/install-hub.sh"

cat > "$tmp/run.sh" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
fail() { echo "FAIL: $*" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive LC_ALL=C
apt-get -qq update >/dev/null && apt-get -qq install -y --no-install-recommends curl ca-certificates jq sqlite3 >/dev/null

# Any systemctl or loginctl call is recorded, and fails.
for t in systemctl loginctl; do
  printf '#!/bin/sh\necho "$0 $*" >> /tmp/systemd-calls\nexit 1\n' > "/usr/local/bin/$t"
  chmod 755 "/usr/local/bin/$t"
done

H=/home/sessiond
BIN=$H/.local/bin/aimem
ENVF=$H/.config/aimem/env
STATE=$H/.local/state/aimem
P=upgrade-scenario
URL=http://127.0.0.1:8440
dl=https://github.com/BlackVS/aimem/releases/download/$FROM

# The CLI as the service user, in the environment the serve line gives it.
cli() { runuser -u sessiond -- env -u XDG_RUNTIME_DIR HOME=$H "$BIN" "$@"; }
# api METHOD PATH [BODY]: the hub's HTTP API with the env file's admin token,
# which is read from the file and never printed. Fails on a non-2xx answer.
api() {
  local auth body=()
  auth="Authorization: Bearer $(sed -n 's/^AIMEM_HTTP_TOKEN=//p' "$ENVF")"
  [ $# -lt 3 ] || body=(-H 'Content-Type: application/json' -H "Idempotency-Key: upgrade-$RANDOM-$RANDOM" --data "$3")
  curl -sS --fail-with-body -m 20 -X "$1" -H @<(echo "$auth") "${body[@]}" "$URL$2"
}
health() {
  curl -s -m 2 -o /dev/null -w '%{http_code}' -H @<(echo "Authorization: Bearer $(sed -n 's/^AIMEM_HTTP_TOKEN=//p' "$ENVF")") "$URL/v1/health" || true
}
serve() { # log: start the hub with the installer's last line
  local cmd
  cmd=$(tail -n 1 "$1")
  case "$cmd" in "runuser -u sessiond -- "*" serve'") ;; *) fail "the last line is not the serve command: $cmd" ;; esac
  eval "$cmd" > /tmp/serve.log 2>&1 &
  for _ in $(seq 1 60); do [ "$(health)" = 200 ] && return 0; sleep 0.5; done
  cat /tmp/serve.log; fail "the printed command did not start a hub that answers /v1/health"
}
stop_hub() {
  local p
  for p in /proc/[0-9]*; do
    tr '\0' ' ' < "$p/cmdline" 2>/dev/null | grep -q "^$BIN serve" && kill "${p#/proc/}" 2>/dev/null || true
  done
  for _ in $(seq 1 40); do [ "$(health)" = 000 ] && return 0; sleep 0.5; done
  fail "the hub did not stop"
}
version() { cli version | awk '{print $2}'; }
user_id() { api GET /v1/access | jq -r --arg n "$1" '.users[] | select(.name == $n) | .id'; }
# enrolled TEAM USER: the team record enrolls USER, not as its coordinator.
enrolled() {
  api GET "/v1/projects/$P/teams/$1" | jq -e --arg u "$2" '[.enrollment[] | select(.user_id == $u and .coordinator == false)] | length == 1' > /dev/null
}
# schemas ROOT: "<project id> <schema>" for each project journal under ROOT.
schemas() {
  local db
  for db in "$1"/projects/*/journal.db; do
    [ -e "$db" ] || continue
    echo "$(basename "$(dirname "$db")") $(sqlite3 -readonly "$db" "SELECT value FROM meta WHERE key='schema_version'")"
  done
}
# has ROOT TABLE ID: the row ID is in TABLE of some project journal under ROOT.
has() {
  local db
  for db in "$1"/projects/*/journal.db; do
    [ "$(sqlite3 -readonly "$db" "SELECT count(*) FROM $2 WHERE id='$3'")" = 1 ] && return 0
  done
  return 1
}

echo "--- $FROM's own hub installer, from its tag"
curl -fsSL "https://raw.githubusercontent.com/BlackVS/aimem/$FROM/install-hub.sh" -o /tmp/from-install-hub.sh
AIMEM_NO_SERVICE=1 bash /tmp/from-install-hub.sh > /tmp/install.log 2>&1 || { cat /tmp/install.log; fail "$FROM's installer failed"; }
cat /tmp/install.log
[ ! -e /tmp/systemd-calls ] || fail "systemd was called: $(cat /tmp/systemd-calls)"
[ "$(version)" = "$FROM" ] || fail "installed $(version), want $FROM"
curl -fsSL -o /tmp/SHA256SUMS "$dl/SHA256SUMS"
[ "$(sha256sum < "$BIN" | cut -d' ' -f1)" = "$(awk '$2 == "aimem-linux-amd64" { print $1 }' /tmp/SHA256SUMS)" ] ||
  fail "the installed binary is not $FROM's published aimem-linux-amd64"
serve /tmp/install.log
echo "ok: $FROM's published hub answers"

echo "--- $FROM writes: a project, a task with a comment, a memory, a team with a member"
cli tasks on -p "$P" > /dev/null
task=$(api POST "/v1/projects/$P/tasks" '{"title":"Written by the previous release","objective":"Read back after the upgrade.","acceptance_criteria":"Survives the upgrade.","non_goals":"","state":"BACKLOG","blocker":"","dependencies":[],"candidate_refs":[],"evidence_refs":[],"next_action":"","archived":false}' | jq -r '.id // .task.id')
[ -n "$task" ] && [ "$task" != null ] || fail "$FROM did not return the new task's id"
comment=$(api POST "/v1/tasks/$task/comments" '{"body":"A comment the previous release wrote."}' | jq -r '.id // .comment.id')
[ -n "$comment" ] && [ "$comment" != null ] || fail "$FROM did not return the new comment's id"
memory=$(api POST "/v1/projects/$P/memories" '{"text":"The hub upgrade scenario wrote this memory with the previous release.","kind":"fact"}' | jq -r '.id')
[ -n "$memory" ] && [ "$memory" != null ] || fail "$FROM did not return the new memory's id"
expiry=$(date -u -d '+30 days' +%Y-%m-%dT%H:%M:%SZ)
cli teams provision create "$P" --team Pilot --coordinator upgrade-coordinator --create-user \
  --label upgrade-coordinator --expiry "$expiry" --output /tmp/coordinator.token > /dev/null
cli teams provision add "$P" --team Pilot --member upgrade-worker --role worker --create-user \
  --label upgrade-worker --expiry "$expiry" --output /tmp/worker.token > /dev/null
worker=$(user_id upgrade-worker)
[ -n "$worker" ] || fail "$FROM's access list has no user upgrade-worker"
team=$(api GET "/v1/projects/$P/teams" | jq -r '.teams[] | select(.name == "Pilot") | .id')
[ -n "$team" ] || fail "$FROM lists no team Pilot"
enrolled "$team" "$worker" || fail "$FROM's team Pilot does not enroll upgrade-worker as a worker"
echo "ok: task $task, comment $comment, memory $memory, team Pilot ($team) with upgrade-worker ($worker)"

echo "--- the tree's installer upgrades $FROM to $TO"
stop_hub
before=$(schemas "$STATE")
[ -n "$before" ] || fail "$FROM wrote no project journal under $STATE"
echo "$before" | while read -r id v; do [ "$v" = "$FROM_SCHEMA" ] || fail "project $id is at schema $v under $FROM, want $FROM_SCHEMA"; done
AIMEM_NO_SERVICE=1 AIMEM_BASE=/rel/export bash /rel/install-hub.sh > /tmp/upgrade.log 2>&1 || { cat /tmp/upgrade.log; fail "the upgrade failed"; }
cat /tmp/upgrade.log
[ ! -e /tmp/systemd-calls ] || fail "systemd was called: $(cat /tmp/systemd-calls)"
grep -q "upgraded aimem $FROM -> $TO" /tmp/upgrade.log || fail "no upgrade line"
[ "$(version)" = "$TO" ] || fail "installed $(version), want $TO"
[ "$(sha256sum < "$BIN" | cut -d' ' -f1)" = "$(cut -d' ' -f1 /rel/export/aimem/$TO/SHA256SUMS)" ] || fail "the installed binary is not the tree's build"
serve /tmp/upgrade.log
echo "ok: the hub answers at $TO"

echo "--- $TO reads what $FROM wrote"
[ "$(api GET "/v1/tasks/$task" | jq -r '(.task // .).title')" = "Written by the previous release" ] || fail "$TO does not read $FROM's task $task"
api GET "/v1/tasks/$task/comments" | jq -e --arg c "$comment" '[(.comments // .)[] | select(.id == $c and .body == "A comment the previous release wrote.")] | length == 1' > /dev/null ||
  fail "$TO does not read $FROM's comment $comment"
api GET "/v1/projects/$P/memories" | jq -e --arg m "$memory" '[.memories[] | select(.id == $m)] | length == 1' > /dev/null ||
  fail "$TO does not read $FROM's memory $memory"
[ "$(user_id upgrade-worker)" = "$worker" ] || fail "$TO does not read $FROM's user upgrade-worker ($worker)"
enrolled "$team" "$worker" || fail "$TO does not read team Pilot ($team) with upgrade-worker enrolled"
echo "ok: the task, its comment, the memory and team Pilot with upgrade-worker read back"
after_memory=$(api POST "/v1/projects/$P/memories" '{"text":"Written by the tree after the upgrade.","kind":"fact"}' | jq -r '.id')

echo "--- the state copy and the schema"
stop_hub
copies=$(ls -d "$STATE".backup-* 2>/dev/null || true)
[ "$(echo "$copies" | grep -c .)" = 1 ] || fail "want one state copy $STATE.backup-*, found: ${copies:-none}"
[ "$(schemas "$copies")" = "$before" ] || fail "the state copy is not $FROM's state: $(schemas "$copies") (want $before)"
has "$copies" tasks "$task" || fail "the state copy does not hold $FROM's task"
has "$copies" memories "$memory" || fail "the state copy does not hold $FROM's memory"
! has "$copies" memories "$after_memory" || fail "the state copy holds a memory written after the upgrade"
echo "ok: $copies is $FROM's state, at schema $FROM_SCHEMA"
schemas "$STATE" | while read -r id v; do [ "$v" = "$TO_SCHEMA" ] || fail "project $id is at schema $v after the upgrade, want $TO_SCHEMA"; done
if [ "$TO_SCHEMA" -gt "$FROM_SCHEMA" ]; then
  echo "migration: schema $FROM_SCHEMA -> $TO_SCHEMA, and $TO read what $FROM wrote"
else
  echo "no schema step: $FROM and the tree are both at schema $TO_SCHEMA"
fi
echo "PASS: hub upgrade $FROM -> $TO (this tree), the data read back, the state copy and the schema"
EOS

docker run --rm -e FROM="$FROM" -e TO="$TO" -e FROM_SCHEMA="$from_schema" -e TO_SCHEMA="$to_schema" \
  -v "$tmp:/rel:ro" "$IMAGE" timeout 600 bash /rel/run.sh
