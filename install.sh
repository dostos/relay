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
# The desktop bridge is a long-lived relayd process that keeps running the OLD
# executable image after an upgrade, so its command allowlist silently lags the
# installed CLI. That is how a remote agent gets "relay command X is not allowed
# through the desktop bridge" for a command the installed binary permits — the
# same stale-process-after-upgrade failure the watchers used to have.
BRIDGE_SOCK="$STATE_ROOT/desktop-bridge.sock"
if [[ -S "$BRIDGE_SOCK" ]]; then
  bridge_pid="$(lsof -t "$BRIDGE_SOCK" 2>/dev/null | head -1 || true)"
  if [[ -n "$bridge_pid" ]]; then
    kill "$bridge_pid" 2>/dev/null || true
    for _ in {1..20}; do
      kill -0 "$bridge_pid" 2>/dev/null || break
      sleep 0.25
    done
  fi
  # Restart it on the new image. It is normally started on demand by
  # `relay resume`, but a pane that is already attached will not re-trigger
  # that, so leaving it down would strand every remote agent's control path.
  nohup "$INSTALL_DIR/relayd" bridge --relay-bin "$INSTALL_DIR/relay" \
    >> "$STATE_ROOT/desktop-bridge.log" 2>&1 &
  disown 2>/dev/null || true
  for _ in {1..20}; do
    [[ -S "$BRIDGE_SOCK" ]] && lsof -t "$BRIDGE_SOCK" >/dev/null 2>&1 && break
    sleep 0.25
  done
  if lsof -t "$BRIDGE_SOCK" >/dev/null 2>&1; then
    echo "relay desktop bridge: restarted on the new binary"
  else
    echo "relay: WARNING - desktop bridge did not come back; remote agents cannot reach this control plane." >&2
    echo "relay:   start it with: relayd bridge --relay-bin $INSTALL_DIR/relay &" >&2
  fi
fi

# Watcher lifecycle belongs to `relay supervise`, not to this script. Restarting
# N per-handoff watchers here used to race the flock of each dying watcher and
# silently leave live handoffs unwatched. Restart the ONE supervisor instead.
SUPERVISOR_LABEL="com.dostos.relay-supervisor"
SUPERVISOR_PLIST="$HOME/Library/LaunchAgents/$SUPERVISOR_LABEL.plist"
if [[ -f "$SUPERVISOR_PLIST" ]]; then
  launchctl kickstart -k "gui/$(id -u)/$SUPERVISOR_LABEL" >/dev/null 2>&1 \
    && echo "relay supervisor: restarted on the new binary" \
    || echo "relay: WARNING - could not restart $SUPERVISOR_LABEL; run: relay supervise --check" >&2
else
  if ! "$INSTALL_DIR/relay" supervise --check >/dev/null 2>&1; then
    echo "relay: WARNING - live handoffs have no watcher and no supervisor is installed." >&2
    echo "relay:   install it:  cp share/launchd/$SUPERVISOR_LABEL.plist ..." >&2
    echo "relay:   or inspect:  relay supervise --check" >&2
  fi
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
