#!/usr/bin/env bash
# Shell entrypoints must remain LF-terminated; see .gitattributes.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:${PATH:-}"
if ! command -v go >/dev/null 2>&1; then
  echo "relay: go is required to build (brew install go)" >&2
  exit 1
fi
BUILD="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)"
if ! git -C "$ROOT" diff --quiet 2>/dev/null || ! git -C "$ROOT" diff --cached --quiet 2>/dev/null; then
  DIRTY_HASH="$(git -C "$ROOT" diff --binary HEAD 2>/dev/null | git hash-object --stdin | cut -c1-12)"
  BUILD="$BUILD-dirty.$DIRTY_HASH"
fi
(
  cd "$ROOT"
  # Stamp the commit so a relayd left behind by an older install is
  # distinguishable from a current one. The protocol version is invariant
  # across rebuilds, so without this nothing could detect fleet drift.
  LDFLAGS="-X github.com/dostos/relay/internal/coord.Build=$BUILD"
  go build -ldflags "$LDFLAGS" -o "$INSTALL_DIR/relay" ./cmd/relay
  go build -ldflags "$LDFLAGS" -o "$INSTALL_DIR/relayd" ./cmd/relayd
)
echo "installed $INSTALL_DIR/relay $INSTALL_DIR/relayd"
RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relay" version
"$INSTALL_DIR/relayd" version

# Running watcher processes keep the old executable image after an upgrade.
# Recycle them so inbox deduplication and reconnect behavior change atomically
# with the installed CLI instead of only after the next handoff.
STATE_ROOT="${RELAY_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/relay}"
RELAYD_SOCK_PATH="${RELAYD_SOCK:-$STATE_ROOT/relayd.sock}"

# Replacing relayd on disk does not replace a running daemon. Worse, Unix
# listeners can survive an unlinked socket while a second daemon binds the new
# path, leaving two healthy-looking process trees split across event buses.
# Restart the existing service through its owner when possible; otherwise
# preserve an already-running unmanaged daemon as one new process.
relayd_pids="$(pgrep -f -x "$INSTALL_DIR/relayd serve" 2>/dev/null || true)"
if [[ -n "$relayd_pids" ]]; then
  if systemctl --user is-active --quiet relayd.service >/dev/null 2>&1; then
    systemctl --user restart relayd.service
  else
    kill $relayd_pids 2>/dev/null || true
    for _ in {1..20}; do
      live_relayd=""
      for pid in $relayd_pids; do
        kill -0 "$pid" 2>/dev/null && live_relayd=1
      done
      [[ -z "$live_relayd" ]] && break
      sleep 0.25
    done
    nohup env RELAYD_SOCK="$RELAYD_SOCK_PATH" "$INSTALL_DIR/relayd" serve >> "$STATE_ROOT/relayd.log" 2>&1 &
    disown 2>/dev/null || true
  fi
  daemon_ok=""
  for _ in {1..20}; do
    daemon_status="$(RELAYD_SOCK="$RELAYD_SOCK_PATH" "$INSTALL_DIR/relayd" status 2>/dev/null || true)"
    if [[ "$daemon_status" == *"\"build\":\"$BUILD\""* ]]; then
      daemon_ok=1
      break
    fi
    sleep 0.25
  done
  if [[ -n "$daemon_ok" ]]; then
    echo "relayd: restarted on build $BUILD"
  else
    echo "relay: WARNING - live relayd did not report installed build $BUILD" >&2
  fi
fi

# Bridge lifecycle belongs to relay-control.service on an authoritative host
# and to no process on a visualization-only host. Never infer ownership from a
# socket pathname: doing so can kill the authoritative service and resurrect
# the retired desktop topology. Binary activation is handled by the owner’s
# service manager, independently of this bootstrap installer.

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
  # Linux/WSL deployments commonly keep the supervisor alive from systemd or
  # a boot script rather than launchd. Preserve that lifecycle across upgrades:
  # an old process keeps its deleted executable image and can look alive while
  # running pre-upgrade watcher logic indefinitely.
  supervisor_pids="$(pgrep -f -x "$INSTALL_DIR/relay supervise" 2>/dev/null || true)"
  if [[ -n "$supervisor_pids" ]]; then
    kill $supervisor_pids 2>/dev/null || true
    for _ in {1..20}; do
      live_supervisor=""
      for pid in $supervisor_pids; do
        kill -0 "$pid" 2>/dev/null && live_supervisor=1
      done
      [[ -z "$live_supervisor" ]] && break
      sleep 0.25
    done
    nohup env RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relay" supervise >> "$STATE_ROOT/supervisor.log" 2>&1 &
    supervisor_pid=$!
    disown 2>/dev/null || true
    for _ in {1..20}; do
      if kill -0 "$supervisor_pid" 2>/dev/null && RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relay" supervise --check >/dev/null 2>&1; then
        break
      fi
      sleep 0.25
    done
    if kill -0 "$supervisor_pid" 2>/dev/null && RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relay" supervise --check >/dev/null 2>&1; then
      echo "relay supervisor: restarted on the new binary"
    else
      echo "relay: WARNING - supervisor restart did not restore all watchers; run: relay supervise --check" >&2
    fi
  elif ! RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relay" supervise --check >/dev/null 2>&1; then
    echo "relay: WARNING - live handoffs have no watcher and no supervisor installation was detected." >&2
    echo "relay:   inspect: relay supervise --check" >&2
  fi
fi

# The optional Mac visualization client connects outbound to the authoritative
# control host. It is installed only when owner config declares a control
# target; home never needs inbound SSH access to the Mac.
VIZ_CONFIG="${RELAY_CONFIG_DIR:-$HOME/.config/relay}/viz.json"
VIZ_LABEL="com.dostos.relay-viz"
VIZ_PLIST="$HOME/Library/LaunchAgents/$VIZ_LABEL.plist"
if [[ "$(uname -s)" == "Darwin" && -f "$VIZ_CONFIG" ]] && grep -q '"control"' "$VIZ_CONFIG"; then
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  sed -e "s|REPLACE_INSTALL_DIR|$INSTALL_DIR|g" -e "s|REPLACE_HOME|$HOME|g" \
    "$ROOT/share/launchd/$VIZ_LABEL.plist" > "$VIZ_PLIST"
  if [[ -z "${RELAY_VIZ_SELF_UPDATE:-}" ]]; then
    launchctl bootout "gui/$(id -u)/$VIZ_LABEL" >/dev/null 2>&1 || true
    launchctl bootstrap "gui/$(id -u)" "$VIZ_PLIST"
    launchctl kickstart -k "gui/$(id -u)/$VIZ_LABEL"
  fi
  echo "relay viz: outbound client registered"
fi

# Relay's compact JSON + `next`/`argv` is the complete agent protocol. Remove
# symlinks created by older installers; workspace AGENTS.md files can point at
# `relay agent protocol` without a runtime-specific skill layer.
unlink_relay_skills() {
  local dst="$1"
  local name target
  [[ -d "$dst" ]] || return 0
  for name in relay-sessions relay-handoff relay-role-bootstrap; do
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

# Goal handoff is semantic policy above the compact wire protocol, so it is
# useful to every agent runtime. Link the same vendor-neutral skill everywhere
# without overwriting a user-owned directory or file.
link_relay_goal_skill() {
  local dst="$1"
  local src="$ROOT/skills/relay-goal-handoff"
  local link="$dst/relay-goal-handoff"
  [[ -d "$src" ]] || return 0
  mkdir -p "$dst"
  if [[ -e "$link" && ! -L "$link" ]]; then
    echo "relay: preserving user-owned skill at $link" >&2
    return 0
  fi
  ln -sfn "$src" "$link"
}
link_relay_goal_skill "$HOME/.agents/skills"
link_relay_goal_skill "$HOME/.claude/skills"
link_relay_goal_skill "$HOME/.codex/skills"
link_relay_goal_skill "$HOME/.cursor/skills"

# Register with cmux Vault so panes re-launch after cmux quit / Mac reboot.
if command -v cmux >/dev/null 2>&1 || [[ -x /Applications/cmux.app/Contents/Resources/bin/cmux ]]; then
  if "$INSTALL_DIR/relay" install-cmux-restore >/dev/null 2>&1; then
    echo "cmux session restore: registered (relay install-cmux-restore)"
  else
    echo "relay: cmux restore registration skipped (non-fatal)" >&2
  fi
fi
