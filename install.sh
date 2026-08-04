#!/usr/bin/env bash
# Shell entrypoints must remain LF-terminated; see .gitattributes.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
STATE_ROOT="${RELAY_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/relay}"
RELAYD_SOCK_PATH="${RELAYD_SOCK:-$STATE_ROOT/relayd.sock}"
mkdir -p "$STATE_ROOT"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:${PATH:-}"
if ! command -v go >/dev/null 2>&1; then
  echo "relay: go is required to build (brew install go)" >&2
  exit 1
fi
BUILD="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)"
UNTRACKED="$(git -C "$ROOT" ls-files --others --exclude-standard 2>/dev/null || true)"
if ! git -C "$ROOT" diff --quiet 2>/dev/null || ! git -C "$ROOT" diff --cached --quiet 2>/dev/null || [[ -n "$UNTRACKED" ]]; then
  # git diff omits untracked files, including new Go packages that are part of
  # the compiled binary. Fold their paths and blob IDs into the build identity
  # so equal build labels really do describe equal source inputs.
  DIRTY_HASH="$({
    git -C "$ROOT" diff --binary HEAD 2>/dev/null
    while IFS= read -r -d '' file; do
      printf 'untracked %s %s\n' "$file" "$(git -C "$ROOT" hash-object -- "$file")"
    done < <(git -C "$ROOT" ls-files --others --exclude-standard -z)
  } | git hash-object --stdin | cut -c1-12)"
  BUILD="$BUILD-dirty.$DIRTY_HASH"
fi
OLD_INSTALLED_BUILD="absent"
if [[ -x "$INSTALL_DIR/relay" ]]; then
  OLD_INSTALLED_BUILD="$(RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relay" build 2>/dev/null || echo unknown)"
fi
OLD_BUILD_KEY="$(printf '%s' "$OLD_INSTALLED_BUILD" | tr -c 'A-Za-z0-9._-' '_')"
ROLLBACK_BACKUP="$STATE_ROOT/migration-backups/$OLD_BUILD_KEY"
mkdir -p "$ROLLBACK_BACKUP"
if [[ ( -e "$INSTALL_DIR/relay" || -L "$INSTALL_DIR/relay" ) && ! -e "$ROLLBACK_BACKUP/relay" ]]; then
  cp -p -- "$INSTALL_DIR/relay" "$ROLLBACK_BACKUP/relay"
fi
if [[ ( -e "$INSTALL_DIR/relayd" || -L "$INSTALL_DIR/relayd" ) && ! -e "$ROLLBACK_BACKUP/relayd" && ! -L "$ROLLBACK_BACKUP/relayd" ]]; then
  cp -a -- "$INSTALL_DIR/relayd" "$ROLLBACK_BACKUP/relayd"
fi
STAGED_RELAY="$INSTALL_DIR/.relay.new.$$"
STAGED_COMPAT="$INSTALL_DIR/.relayd.new.$$"
cleanup_staged() {
  rm -f -- "$STAGED_RELAY" "$STAGED_COMPAT" "$INSTALL_DIR/.relay.previous.new.$$" "$INSTALL_DIR/.relayd.previous.new.$$"
}
trap cleanup_staged EXIT
(
  cd "$ROOT"
  # Stamp the commit so a relayd left behind by an older install is
  # distinguishable from a current one. The protocol version is invariant
  # across rebuilds, so without this nothing could detect fleet drift.
  LDFLAGS="-X github.com/dostos/relay/internal/coord.Build=$BUILD"
  go build -ldflags "$LDFLAGS" -o "$STAGED_RELAY" ./cmd/relay
)
STAGED_BUILD="$(RELAY_BRIDGE_LOCAL_INVOKE=1 "$STAGED_RELAY" build)"
if [[ "$STAGED_BUILD" != "$BUILD" ]]; then
  echo "relay: staged build mismatch: got $STAGED_BUILD want $BUILD" >&2
  exit 1
fi
if [[ -e "$INSTALL_DIR/relay" || -L "$INSTALL_DIR/relay" ]]; then
  cp -p -- "$INSTALL_DIR/relay" "$INSTALL_DIR/.relay.previous.new.$$"
  mv -f -- "$INSTALL_DIR/.relay.previous.new.$$" "$INSTALL_DIR/relay.previous"
fi
if [[ -e "$INSTALL_DIR/relayd" || -L "$INSTALL_DIR/relayd" ]]; then
  cp -a -- "$INSTALL_DIR/relayd" "$INSTALL_DIR/.relayd.previous.new.$$"
  mv -f -- "$INSTALL_DIR/.relayd.previous.new.$$" "$INSTALL_DIR/relayd.previous"
fi
mv -f -- "$STAGED_RELAY" "$INSTALL_DIR/relay"
ln -s relay "$STAGED_COMPAT"
mv -f -- "$STAGED_COMPAT" "$INSTALL_DIR/relayd"
echo "installed $INSTALL_DIR/relay (relayd is a compatibility symlink)"
RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relay" version
RELAY_BRIDGE_LOCAL_INVOKE=1 "$INSTALL_DIR/relayd" version

# Installation never kills or replaces a healthy system-owned process. Service
# migration is a separate explicit policy decision. User-owned units may be
# migrated atomically only when the caller sets RELAY_MIGRATE_SERVICE=1.
OLD_PIDS="$(pgrep -f 'relayd serve|relayd control serve|relay supervise|relay service event run|relay service boundary run|relay service watcher run' 2>/dev/null | tr '\n' ' ' || true)"
PROCESS_OWNERS="none"
if [[ -n "$OLD_PIDS" ]]; then
  OLD_PID_CSV="$(echo "$OLD_PIDS" | xargs | tr ' ' ',')"
  PROCESS_OWNERS="$(ps -o pid=,user= -p "$OLD_PID_CSV" 2>/dev/null | tr '\n' ';' || echo unknown)"
fi
SYSTEM_OWNED=""
SYSTEM_UNIT_PROPOSAL="none"
UNIFIED_SYSTEM_ACTIVE=0
LEGACY_SYSTEM_ACTIVE=0
for unit in relay.service relayd.service relay-control.service relay-supervisor.service; do
  if systemctl cat "$unit" >/dev/null 2>&1; then
    UNIT_STATE="$(systemctl is-active "$unit" 2>/dev/null || true)"
    SYSTEM_OWNED="$SYSTEM_OWNED $unit:$UNIT_STATE"
    if [[ "$unit" == "relay.service" && "$UNIT_STATE" == "active" ]]; then
      UNIFIED_SYSTEM_ACTIVE=1
    elif [[ "$unit" != "relay.service" && "$UNIT_STATE" == "active" ]]; then
      LEGACY_SYSTEM_ACTIVE=1
    fi
  fi
done
if [[ -n "$SYSTEM_OWNED" ]]; then
  SYSTEM_UNIT_PROPOSAL="$STATE_ROOT/relay-system.service.proposed"
  sed -e "s|REPLACE_USER|$(id -un)|g" -e "s|REPLACE_GROUP|$(id -gn)|g" -e "s|REPLACE_HOME|$HOME|g" \
    "$ROOT/share/systemd/relay-system.service" > "$SYSTEM_UNIT_PROPOSAL"
  chmod 600 "$SYSTEM_UNIT_PROPOSAL"
  echo "relay: system-owned services preserved:$SYSTEM_OWNED" >&2
  echo "relay: proposed system unit: $SYSTEM_UNIT_PROPOSAL" >&2
  if (( UNIFIED_SYSTEM_ACTIVE )) && (( ! LEGACY_SYSTEM_ACTIVE )); then
    echo "relay: unified system service is active; administrator restart required to load this build: sudo systemctl restart relay.service" >&2
  else
    echo "relay: explicit administrator migration required: sudo install -m 0644 $SYSTEM_UNIT_PROPOSAL /etc/systemd/system/relay.service && sudo systemctl daemon-reload && sudo systemctl stop relayd relay-control relay-supervisor && sudo systemctl enable --now relay" >&2
    echo "relay: if relay fails its read-back, restart the preserved legacy units; disable them only after acceptance" >&2
  fi
elif [[ "${RELAY_MIGRATE_SERVICE:-}" == "1" ]]; then
  mkdir -p "$HOME/.config/systemd/user"
  sed "s|%h|$HOME|g" "$ROOT/share/systemd/relay.service" > "$HOME/.config/systemd/user/relay.service"
  systemctl --user daemon-reload
  systemctl --user disable --now relayd.service relay-control.service relay-supervisor.service >/dev/null 2>&1 || true
  systemctl --user enable --now relay.service
  echo "relay: migrated user services to relay.service"
elif systemctl --user is-active --quiet relay.service >/dev/null 2>&1; then
  systemctl --user restart relay.service
  echo "relay: restarted user-owned relay.service"
elif [[ -n "$OLD_PIDS" ]]; then
  echo "relay: legacy processes preserved ($OLD_PIDS); rerun with RELAY_MIGRATE_SERVICE=1 after review" >&2
fi

RECEIPT="$STATE_ROOT/migration-receipt-$BUILD.json"
printf '{"v":1,"old_installed_build":"%s","installed_build":"%s","event_socket":"%s","command_socket":"%s","legacy_pids":"%s","process_owners":"%s","system_owned":"%s","proposed_system_unit":"%s","rollback_backup":"%s","rollback":"restore the build-keyed backup, remove relay.service, then re-enable the units recorded here"}\n' \
  "$OLD_INSTALLED_BUILD" "$BUILD" "$RELAYD_SOCK_PATH" "$STATE_ROOT/desktop-bridge.sock" "$OLD_PIDS" "$PROCESS_OWNERS" "$SYSTEM_OWNED" "$SYSTEM_UNIT_PROPOSAL" "$ROLLBACK_BACKUP" > "$RECEIPT"
chmod 600 "$RECEIPT"
echo "relay migration receipt: $RECEIPT"

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
# symlinks created by older installers, including the formerly auto-linked goal
# helper; workspace agent instructions can point at `relay agent protocol`
# without making a runtime-specific skill part of correctness.
unlink_relay_skills() {
  local dst="$1"
  local name target
  [[ -d "$dst" ]] || return 0
  for name in relay-sessions relay-handoff relay-role-bootstrap relay-goal-handoff; do
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
