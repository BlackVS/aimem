#!/usr/bin/env bash
# aimem installer. Idempotent; safe to re-run.
#
#   ./install.sh user               user-level install: binary -> ~/.local/bin,
#                                   systemd user unit, Claude Code user hooks,
#                                   OpenCode global plugin
#   ./install.sh project [dir]      wire one project: handoff template,
#                                   session-start handoff loading for both
#                                   clients, AGENTS.md protocol stub
#   ./install.sh uninstall-user     remove everything `user` installed
#
# After `user`: restart running OpenCode/Claude Code sessions to activate.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${AIMEM_BIN_DIR:-$HOME/.local/bin}"
CLAUDE_SETTINGS="${AIMEM_CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
OC_PLUGIN_DIR="${AIMEM_OC_PLUGIN_DIR:-$HOME/.config/opencode/plugins}"
UNIT_DIR="${AIMEM_UNIT_DIR:-$HOME/.config/systemd/user}"
SUBMIT_CMD='command -v aimem >/dev/null 2>&1 && aimem submit-claude || true'

say() { printf '==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "error: $1 is required" >&2; exit 1; }; }

# Merge one checkpoint hook entry into a Claude settings file, keyed on the
# marker string so re-runs and uninstalls find it. Never touches other hooks.
add_claude_hook() { # file event command status
  local file=$1 event=$2 cmd=$3 status=$4
  mkdir -p "$(dirname "$file")"
  [ -s "$file" ] || echo '{}' > "$file"
  jq -e . "$file" >/dev/null || { echo "error: $file is not valid JSON; fix it first" >&2; exit 1; }
  local tmp
  tmp=$(mktemp)
  jq --arg ev "$event" --arg cmd "$cmd" --arg st "$status" '
    .hooks[$ev] = ((.hooks[$ev] // []) |
      if ([.[] | .hooks[]? | .command // ""] | any(contains("aimem submit-claude")))
      then .
      else . + [{"hooks":[{"type":"command","command":$cmd,"timeout":10,"statusMessage":$st}]}]
      end)' "$file" > "$tmp" && mv "$tmp" "$file"
}

remove_claude_hooks() { # file
  local file=$1
  [ -s "$file" ] || return 0
  local tmp
  tmp=$(mktemp)
  jq '
    if .hooks then
      .hooks |= with_entries(
        .value |= map(select(
          ([.hooks[]? | .command // ""] | any(contains("aimem submit-claude"))) | not
        ))
      ) | .hooks |= with_entries(select(.value | length > 0))
      | if (.hooks | length) == 0 then del(.hooks) else . end
    else . end' "$file" > "$tmp" && mv "$tmp" "$file"
}

install_user() {
  need jq
  mkdir -p "$BIN_DIR"
  # Prebuilt binary (dropped in by boot.sh from a release asset) wins; the
  # Go toolchain is only needed when building from source.
  local prebuilt="${AIMEM_PREBUILT:-$REPO_DIR/bin/linux-amd64/aimem}"
  if [ -x "$prebuilt" ]; then
    say "installing prebuilt binary"
    cp "$prebuilt" "$BIN_DIR/aimem.new"
  else
    need go
    say "building aimem"
    (cd "$REPO_DIR" && go build \
      -ldflags "-X main.version=$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)" \
      -o "$BIN_DIR/aimem.new" ./cmd/aimem)
  fi
  mv "$BIN_DIR/aimem.new" "$BIN_DIR/aimem"
  say "installed $BIN_DIR/aimem"
  case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "warning: $BIN_DIR is not on PATH" >&2 ;; esac

  say "Claude Code user hooks -> $CLAUDE_SETTINGS"
  add_claude_hook "$CLAUDE_SETTINGS" Stop        "$SUBMIT_CMD" "Checkpointing turn"
  add_claude_hook "$CLAUDE_SETTINGS" StopFailure "$SUBMIT_CMD" "Checkpointing failed turn"
  add_claude_hook "$CLAUDE_SETTINGS" PreCompact  "$SUBMIT_CMD" "Journaling compaction marker"

  say "OpenCode global plugin -> $OC_PLUGIN_DIR/aimem.ts"
  mkdir -p "$OC_PLUGIN_DIR"
  cp "$REPO_DIR/.opencode/plugin/aimem.ts" "$OC_PLUGIN_DIR/aimem.ts"

  if [ "${AIMEM_NO_SYSTEMD:-0}" != 1 ] && command -v systemctl >/dev/null 2>&1; then
    say "systemd user unit -> $UNIT_DIR/aimem.service"
    mkdir -p "$UNIT_DIR"
    cat > "$UNIT_DIR/aimem.service" <<EOF
[Unit]
Description=aimem - local coding-session recovery and memory service

[Service]
EnvironmentFile=-%h/.config/aimem/env
ExecStart=$BIN_DIR/aimem serve
Restart=on-failure
RestartSec=2
UMask=0077

[Install]
WantedBy=default.target
EOF
    systemctl --user daemon-reload
    systemctl --user enable aimem
    systemctl --user restart aimem   # enable --now would keep a stale binary/unit running
    sleep 0.5
    "$BIN_DIR/aimem" health >/dev/null && say "service healthy" \
      || echo "warning: service not answering yet; check: systemctl --user status aimem" >&2
    "$BIN_DIR/aimem" spool-flush >/dev/null 2>&1 || true
  else
    say "skipping systemd (unavailable or AIMEM_NO_SYSTEMD=1); run 'aimem serve' manually"
  fi

  say "user install done. Restart running OpenCode and Claude Code sessions to activate."
}

install_project() {
  need jq
  local dir="${1:-.}"
  dir="$(cd "$dir" && pwd)"
  say "wiring project $dir"

  if [ ! -f "$dir/docs/SESSION-STATE.md" ]; then
    mkdir -p "$dir/docs"
    cat > "$dir/docs/SESSION-STATE.md" <<'EOF'
# Session State

Updated: (date) | branch: (branch) | HEAD: (sha) | by: (client/session)

## Objective

(current objective)

## Next actions (ready)

1. (first action)

## Pick up here

(one line)
EOF
    say "created docs/SESSION-STATE.md template"
  fi

  # Claude Code: load the handoff at session start (project-scoped). The
  # `aimem session-start` adapter is portable (same hook works on Windows).
  local psettings="$dir/.claude/settings.json"
  mkdir -p "$dir/.claude"
  [ -s "$psettings" ] || echo '{}' > "$psettings"
  local loadcmd='command -v aimem >/dev/null 2>&1 && aimem session-start || true'
  local tmp
  tmp=$(mktemp)
  jq --arg cmd "$loadcmd" '
    .hooks.SessionStart = ((.hooks.SessionStart // []) |
      if ([.[] | .hooks[]? | .command // ""]
          | any(contains("SESSION-STATE.md") or contains("aimem session-start")))
      then .
      else . + [{"hooks":[{"type":"command","command":$cmd,"timeout":10,"statusMessage":"Loading session handoff"}]}]
      end)' "$psettings" > "$tmp" && mv "$tmp" "$psettings"
  say "Claude Code SessionStart handoff hook wired"

  # MCP recall facade for both clients.
  local mcpjson="$dir/.mcp.json"
  [ -s "$mcpjson" ] || echo '{}' > "$mcpjson"
  tmp=$(mktemp)
  jq '.mcpServers.aimem //= {"command":"aimem","args":["mcp"]}' "$mcpjson" > "$tmp" && mv "$tmp" "$mcpjson"

  # OpenCode: load the handoff as instructions + register the MCP facade.
  local ocjson="$dir/opencode.json"
  [ -s "$ocjson" ] || printf '{\n  "$schema": "https://opencode.ai/config.json"\n}\n' > "$ocjson"
  tmp=$(mktemp)
  jq '.instructions = ((.instructions // []) |
      if index("docs/SESSION-STATE.md") then . else . + ["docs/SESSION-STATE.md"] end)
      | .mcp.aimem //= {"type":"local","command":["aimem","mcp"],"enabled":true}' \
    "$ocjson" > "$tmp" && mv "$tmp" "$ocjson"
  say "OpenCode instructions + MCP wired"

  # /aimem-analyze slash command for both clients. MANAGED: overwritten on
  # every install so template improvements roll out with upgrades — tune it
  # in the aimem repo (install.sh), not in the wired project.
  mkdir -p "$dir/.claude/commands" "$dir/.opencode/command"
  for cmdfile in "$dir/.claude/commands/aimem-analyze.md" "$dir/.opencode/command/aimem-analyze.md"; do
    cat > "$cmdfile" <<'ANALYZE'
---
description: Analyze this repo into the shared aimem knowledge base, subsystem by subsystem
---
<!-- managed by aimem install.sh — edits here are overwritten on reinstall -->
Analyze this repository to build durable knowledge, subsystem by subsystem.
Focus area for this run (optional): $ARGUMENTS

Before starting: read .aimem.json to learn this project's name and declared
knowledge groups. Call get_design_doc to load what the group's knowledge
base already knows, and recall_memory (scope "both") for anything about
this repo. Do not re-derive what is already recorded — verify and extend.

Then work through the codebase one subsystem at a time: entry points, core
data flow, then supporting modules. For each, understand and summarize in
your replies: what it does, its key invariants and assumptions, gotchas,
cross-module contracts, and why non-obvious decisions were made.

Record knowledge, don't transcribe code: use the remember tool for the most
important findings (one self-contained sentence each; kind fact/decision/
convention; tags = component names). Facts that hold framework-wide across
the group's repos: scope "group:<name>". Facts specific to this repo:
default project scope. Never record what is trivially readable from the
code (signatures, field lists) — record behavior, invariants, and reasons.

End each work chunk with a short summary reply of what you established —
those summaries are what curation distills into the knowledge base.
ANALYZE
  done
  say "/aimem-analyze command wired (Claude Code + OpenCode)"

  if [ ! -f "$dir/AGENTS.md" ]; then
    cp "$REPO_DIR/templates/AGENTS.md" "$dir/AGENTS.md"
    say "copied AGENTS.md protocol (edit its project-context section)"
  fi
  if [ ! -f "$dir/CLAUDE.md" ]; then
    printf 'See @AGENTS.md for the shared session handoff protocol.\n' > "$dir/CLAUDE.md"
    say "created CLAUDE.md import stub"
  fi

  # Group membership: sharing is opt-in, empty groups = isolated project.
  # AIMEM_GROUPS="a,b" pre-declares shared knowledge groups at install time;
  # AIMEM_PROJECT=name pins a stable project id so the same repo shares one
  # journal on every machine and checkout dir.
  if [ ! -f "$dir/.aimem.json" ]; then
    jq -cn --arg g "${AIMEM_GROUPS:-}" --arg p "${AIMEM_PROJECT:-}" \
      '{groups: ($g | split(",") | map(gsub("^\\s+|\\s+$";"")) | map(select(length>0)))}
       + (if $p != "" then {project: $p} else {} end)' \
      > "$dir/.aimem.json"
    say "created .aimem.json (project: ${AIMEM_PROJECT:-auto}, groups: ${AIMEM_GROUPS:-none})"
  fi
  say "project wired. Commit the new files if the project is tracked."
}

# deploy-server: run LOCALLY to provision a hub over ssh. Cross-builds a
# static binary (no Go needed on the server), installs it with a systemd
# user unit, and enables the service. The server only journals and syncs —
# no client hooks or plugins are installed there.
deploy_server() {
  need go; need ssh; need scp
  local dest="${1:?usage: $0 deploy-server <ssh-destination>}"
  local sshopts=${AIMEM_SSH_OPTS:-}
  say "cross-building static binary"
  (cd "$REPO_DIR" && CGO_ENABLED=0 go build \
    -ldflags "-X main.version=$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)" \
    -o /tmp/aimem-deploy ./cmd/aimem)
  say "copying to $dest"
  # shellcheck disable=SC2086
  scp -q $sshopts /tmp/aimem-deploy "$dest:aimem.new"
  rm -f /tmp/aimem-deploy
  say "installing on $dest"
  # shellcheck disable=SC2086
  ssh $sshopts "$dest" 'bash -s' <<'EOF'
set -euo pipefail
mkdir -p ~/.local/bin ~/.config/systemd/user
mv ~/aimem.new ~/.local/bin/aimem
chmod 755 ~/.local/bin/aimem
grep -q '\.local/bin' ~/.profile 2>/dev/null || echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.profile
# sync arrives over non-login ssh: make sure the binary is on that PATH too
grep -q '\.local/bin' ~/.bashrc 2>/dev/null || sed -i '1i export PATH="$HOME/.local/bin:$PATH"' ~/.bashrc 2>/dev/null || true
cat > ~/.config/systemd/user/aimem.service <<UNIT
[Unit]
Description=aimem - session journal hub

[Service]
EnvironmentFile=-%h/.config/aimem/env
ExecStart=%h/.local/bin/aimem serve
Restart=on-failure
RestartSec=2
UMask=0077

[Install]
WantedBy=default.target
UNIT
mkdir -p ~/.config/aimem
systemctl --user daemon-reload
systemctl --user enable aimem
systemctl --user restart aimem
sleep 0.5
~/.local/bin/aimem health
EOF
  say "hub deployed. IMPORTANT: enable lingering on the server so the service"
  say "survives logout/reboot:  sudo loginctl enable-linger \$USER   (run on $dest)"
  say "then from each client machine:  aimem sync $dest"
}

# enable-sync: periodic journal sync from this machine to the hub via a
# systemd user timer. AIMEM_SSH_OPTS at install time is baked into the
# unit so unattended runs use the dedicated key.
enable_sync() {
  # No argument: sync every configured hub over its API (or its ssh
  # destination, for hubs that still have one) — DESIGN-hub-sync. An
  # explicit ssh destination keeps the legacy single-peer form.
  local dest="${1:-}"
  local synccmd="sync --all-hubs" desc="all configured hubs"
  if [ -n "$dest" ]; then synccmd="sync $dest"; desc="$dest"; fi
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/aimem-sync.service" <<EOF
[Unit]
Description=aimem journal sync to $desc

[Service]
Type=oneshot
Environment="AIMEM_SSH_OPTS=${AIMEM_SSH_OPTS:-}"
EnvironmentFile=-%h/.config/aimem/env
ExecStart=$BIN_DIR/aimem $synccmd
EOF
  cat > "$UNIT_DIR/aimem-sync.timer" <<EOF
[Unit]
Description=periodic aimem journal sync

[Timer]
OnBootSec=2min
OnUnitActiveSec=10min
RandomizedDelaySec=1min

[Install]
WantedBy=timers.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now aimem-sync.timer
  say "sync timer enabled: every ~10min to $desc"
}

# bootstrap: the boot.sh one-liner entrypoint. Run from inside a project
# directory: user-level install if aimem is not on PATH yet, optional hub
# push config from env (AIMEM_HUB_URL / AIMEM_HUB_TOKEN), then wire the
# current project.
bootstrap() {
  local dir="${1:-$PWD}"
  if command -v aimem >/dev/null 2>&1 && [ "${AIMEM_REINSTALL:-0}" != 1 ]; then
    say "aimem already on PATH ($(command -v aimem)); skipping user install (AIMEM_REINSTALL=1 to force)"
  else
    install_user
  fi
  if [ -n "${AIMEM_HUB_URL:-}" ] && [ -n "${AIMEM_HUB_TOKEN:-}" ]; then
    "$BIN_DIR/aimem" hub "$AIMEM_HUB_URL" "$AIMEM_HUB_TOKEN" && say "hub push configured: $AIMEM_HUB_URL"
  fi
  install_project "$dir"
  say "bootstrap done. Restart Claude Code / OpenCode sessions in $dir to activate."
}

uninstall_user() {
  need jq
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user disable --now aimem 2>/dev/null || true
  fi
  rm -f "$UNIT_DIR/aimem.service" "$BIN_DIR/aimem" "$OC_PLUGIN_DIR/aimem.ts"
  remove_claude_hooks "$CLAUDE_SETTINGS"
  say "user install removed (journal data in ~/.local/state/aimem left untouched)"
}

case "${1:-user}" in
  user) install_user ;;
  project) install_project "${2:-.}" ;;
  bootstrap) bootstrap "${2:-$PWD}" ;;
  deploy-server) deploy_server "${2:-}" ;;
  enable-sync) enable_sync "${2:-}" ;;
  uninstall-user) uninstall_user ;;
  *) echo "usage: $0 [user|project [dir]|bootstrap [dir]|deploy-server <ssh-dest>|enable-sync [ssh-dest]|uninstall-user]" >&2; exit 2 ;;
esac
