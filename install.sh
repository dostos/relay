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
watchers_failed=0
failed_handoffs=""
if [[ -d "$WATCH_DIR" ]]; then
  shopt -s nullglob
  for lock in "$WATCH_DIR"/*.lock; do
    handoff_id="$(basename "$lock" .lock)"

    # Only live handoffs need a watcher. A terminal handoff's watcher exits
    # immediately by design, so killing and restarting it is pointless churn
    # that also produces spurious "failed to restart" noise.
    handoff_file="$STATE_ROOT/handoffs/$handoff_id.json"
    [[ -f "$handoff_file" ]] || continue
    handoff_status="$(sed -n 's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$handoff_file" | head -1)"
    case "$handoff_status" in
      done|failed|abandoned|"") continue ;;
    esac

    watcher_pid="$(tr -dc '0-9' < "$lock")"
    if [[ -n "$watcher_pid" ]] && kill -0 "$watcher_pid" 2>/dev/null; then
      watcher_cmd="$(ps -p "$watcher_pid" -o command= 2>/dev/null || true)"
      if [[ "$watcher_cmd" == *"parent watch $handoff_id"* ]]; then
        # A watcher blocked in an SSH subscribe stream takes ~11s to honour
        # SIGTERM (measured). The old 1s wait meant the restart raced the
        # dying process for the flock, lost, and gave up — leaving the handoff
        # with no watcher and its escalations silently unrouted. Wait properly,
        # then SIGKILL so this is bounded either way.
        kill "$watcher_pid" 2>/dev/null || true
        for _ in {1..60}; do
          kill -0 "$watcher_pid" 2>/dev/null || break
          sleep 0.5
        done
        if kill -0 "$watcher_pid" 2>/dev/null; then
          kill -9 "$watcher_pid" 2>/dev/null || true
          sleep 1
        fi
      fi
    fi

    # VERIFY BY PROCESS, not by exit code: `parent watch --detach` reports
    # success as soon as the child is spawned, so a child that then fails to
    # take the lock exits silently while the command still returns 0.
    watcher_started=0
    for _ in {1..5}; do
      "$INSTALL_DIR/relay" --json parent watch "$handoff_id" --detach >/dev/null 2>&1 || true
      sleep 1
      if pgrep -f "parent watch $handoff_id" >/dev/null 2>&1; then
        watcher_started=1
        break
      fi
    done
    if (( watcher_started )); then
      watchers_refreshed=$((watchers_refreshed + 1))
    else
      watchers_failed=$((watchers_failed + 1))
      failed_handoffs="$failed_handoffs $handoff_id"
    fi
  done
  shopt -u nullglob
fi
if (( watchers_refreshed > 0 )); then
  echo "relay parent watchers: refreshed $watchers_refreshed"
fi
# Never report success for a watcher that did not come back: a handoff with no
# watcher routes nothing, and the failure is otherwise invisible.
if (( watchers_failed > 0 )); then
  echo "relay: WARNING - $watchers_failed parent watcher(s) failed to restart:$failed_handoffs" >&2
  echo "relay: those handoffs are NOT routing escalations. Restart each with:" >&2
  echo "relay:   relay parent watch <handoff-id> --detach" >&2
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
