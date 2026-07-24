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

# Agentic skills live IN this repo (skills/). Symlink into agent skill dirs.
# Source of truth: $ROOT/skills/relay-{sessions,handoff}
link_skills() {
  local dst="$1"
  [[ -z "$dst" ]] && return 0
  mkdir -p "$dst"
  ln -sfn "$ROOT/skills/relay-sessions" "$dst/relay-sessions"
  ln -sfn "$ROOT/skills/relay-handoff" "$dst/relay-handoff"
  rm -f "$dst/sst-sessions" "$dst/sst-handoff"
  echo "skills → $dst/relay-{sessions,handoff}"
}
if [[ -n "${RELAY_SKILL_DIR:-}" ]]; then
  link_skills "$RELAY_SKILL_DIR"
else
  link_skills "$HOME/.claude/skills"
  # Codex shares the same SKILL.md layout when present.
  if [[ -d "$HOME/.codex/skills" ]] || command -v codex >/dev/null 2>&1; then
    link_skills "$HOME/.codex/skills"
  fi
fi

# Register with cmux Vault so panes re-launch after cmux quit / Mac reboot.
if command -v cmux >/dev/null 2>&1 || [[ -x /Applications/cmux.app/Contents/Resources/bin/cmux ]]; then
  if "$INSTALL_DIR/relay" install-cmux-restore >/dev/null 2>&1; then
    echo "cmux session restore: registered (relay install-cmux-restore)"
  else
    echo "relay: cmux restore registration skipped (non-fatal)" >&2
  fi
fi
