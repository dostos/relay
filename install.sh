#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:${PATH:-}"
if ! command -v go >/dev/null 2>&1; then
  echo "relay: go is required to build (brew install go)" >&2
  exit 1
fi
(
  cd "$ROOT"
  go build -o "$INSTALL_DIR/relay" ./cmd/relay
  go build -o "$INSTALL_DIR/relayd" ./cmd/relayd
)
echo "installed $INSTALL_DIR/relay $INSTALL_DIR/relayd"
"$INSTALL_DIR/relay" version
"$INSTALL_DIR/relayd" version

# Running watcher processes keep the old executable image after an upgrade.
# Recycle them so inbox deduplication and reconnect behavior change atomically
# with the installed CLI instead of only after the next handoff.
STATE_ROOT="${RELAY_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/relay}"
WATCH_DIR="$STATE_ROOT/parent-watch"
watchers_refreshed=0
if [[ -d "$WATCH_DIR" ]]; then
  shopt -s nullglob
  for lock in "$WATCH_DIR"/*.lock; do
    handoff_id="$(basename "$lock" .lock)"
    watcher_pid="$(tr -dc '0-9' < "$lock")"
    if [[ -n "$watcher_pid" ]] && kill -0 "$watcher_pid" 2>/dev/null; then
      watcher_cmd="$(ps -p "$watcher_pid" -o command= 2>/dev/null || true)"
      if [[ "$watcher_cmd" == *"parent watch $handoff_id"* ]]; then
        kill "$watcher_pid" 2>/dev/null || true
        for _ in {1..20}; do
          kill -0 "$watcher_pid" 2>/dev/null || break
          sleep 0.05
        done
      fi
    fi
    "$INSTALL_DIR/relay" --json parent watch "$handoff_id" --detach >/dev/null 2>&1 || true
    watchers_refreshed=$((watchers_refreshed + 1))
  done
  shopt -u nullglob
fi
if (( watchers_refreshed > 0 )); then
  echo "relay parent watchers: refreshed $watchers_refreshed"
fi

# Relay's compact JSON + `next`/`argv` is the complete agent protocol. Remove
# symlinks created by older installers; workspace AGENTS.md files can point at
# `relay agent protocol` without a runtime-specific skill layer.
unlink_relay_skills() {
  local dst="$1"
  local name target
  [[ -d "$dst" ]] || return 0
  for name in relay-sessions relay-handoff; do
    [[ -L "$dst/$name" ]] || continue
    target="$(readlink "$dst/$name")"
    if [[ "$target" == "$ROOT/skills/$name" ]] || [[ ! -e "$dst/$name" ]]; then
      rm "$dst/$name"
      echo "removed legacy skill link $dst/$name"
    fi
  done
}
unlink_relay_skills "$HOME/.agents/skills"
unlink_relay_skills "$HOME/.claude/skills"
unlink_relay_skills "$HOME/.codex/skills"
unlink_relay_skills "$HOME/.cursor/skills"

# Register with cmux Vault so panes re-launch after cmux quit / Mac reboot.
if command -v cmux >/dev/null 2>&1 || [[ -x /Applications/cmux.app/Contents/Resources/bin/cmux ]]; then
  if "$INSTALL_DIR/relay" install-cmux-restore >/dev/null 2>&1; then
    echo "cmux session restore: registered (relay install-cmux-restore)"
  else
    echo "relay: cmux restore registration skipped (non-fatal)" >&2
  fi
fi
