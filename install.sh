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

# Skill symlinks for Claude / Cursor — relay primary, sst names → redirect shims
SKILL_DST="${RELAY_SKILL_DIR:-$HOME/.claude/skills}"
mkdir -p "$SKILL_DST"
ln -sfn "$ROOT/skills/relay-sessions" "$SKILL_DST/relay-sessions"
ln -sfn "$ROOT/skills/relay-handoff" "$SKILL_DST/relay-handoff"
# Retire sst skill names in-place so old descriptions still resolve to the cutover note.
ln -sfn "$ROOT/skills/compat/sst-sessions" "$SKILL_DST/sst-sessions"
ln -sfn "$ROOT/skills/compat/sst-handoff" "$SKILL_DST/sst-handoff"
echo "skills → $SKILL_DST/{relay,sst}-{sessions,handoff} (sst → relay redirect)"
