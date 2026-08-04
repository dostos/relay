#!/usr/bin/env bash
# Perform the explicitly authorized system-unit cutover. Run via sudo; this
# script never accepts or handles the owner's credential itself.
set -euo pipefail

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  echo "relay: run this cutover with sudo" >&2
  exit 2
fi

RELAY_USER="${SUDO_USER:-}"
if [[ -z "$RELAY_USER" || "$RELAY_USER" == root ]]; then
  echo "relay: SUDO_USER must name the non-root Relay owner" >&2
  exit 2
fi

RELAY_HOME="$(getent passwd "$RELAY_USER" | cut -d: -f6)"
RELAY_GROUP="$(id -gn "$RELAY_USER")"
case "$RELAY_HOME" in
  /home/*) ;;
  *) echo "relay: refusing unexpected owner home: $RELAY_HOME" >&2; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
STATE_ROOT="$RELAY_HOME/.local/state/relay"
PROPOSAL="$STATE_ROOT/relay-system.service.proposed"
TARGET=/etc/systemd/system/relay.service
RELAY_BIN="$RELAY_HOME/.local/bin/relay"
LEGACY_UNITS=(relayd.service relay-control.service relay-supervisor.service)

owner_relay() {
  runuser -u "$RELAY_USER" -- env \
    HOME="$RELAY_HOME" \
    USER="$RELAY_USER" \
    LOGNAME="$RELAY_USER" \
    XDG_STATE_HOME="$RELAY_HOME/.local/state" \
    XDG_CONFIG_HOME="$RELAY_HOME/.config" \
    "$RELAY_BIN" "$@"
}

[[ -x "$RELAY_BIN" ]] || { echo "relay: missing executable $RELAY_BIN" >&2; exit 2; }
[[ -f "$PROPOSAL" && ! -L "$PROPOSAL" ]] || {
  echo "relay: missing regular proposal $PROPOSAL" >&2
  exit 2
}
[[ ! -e "$TARGET" && ! -L "$TARGET" ]] || {
  echo "relay: refusing to overwrite existing $TARGET" >&2
  exit 2
}

if ! cmp -s "$PROPOSAL" <(
  sed \
    -e "s|REPLACE_USER|$RELAY_USER|g" \
    -e "s|REPLACE_GROUP|$RELAY_GROUP|g" \
    -e "s|REPLACE_HOME|$RELAY_HOME|g" \
    "$REPO_ROOT/share/systemd/relay-system.service"
); then
  echo "relay: proposed unit does not match the repository template and owner" >&2
  exit 2
fi

for unit in "${LEGACY_UNITS[@]}"; do
  systemctl is-active --quiet "$unit" || {
    echo "relay: expected active rollback unit $unit" >&2
    exit 2
  }
done

rollback_needed=1
rollback() {
  rc=$?
  trap - EXIT
  if (( rollback_needed )); then
    echo "relay: cutover failed; restoring compatibility services" >&2
    systemctl disable --now relay.service >/dev/null 2>&1 || true
    if [[ -e "$TARGET" || -L "$TARGET" ]]; then
      unlink -- "$TARGET"
    fi
    systemctl daemon-reload || true
    systemctl start "${LEGACY_UNITS[@]}" || true
  fi
  exit "$rc"
}
trap rollback EXIT

install -m 0644 "$PROPOSAL" "$TARGET"
systemd-analyze verify "$TARGET"
systemctl daemon-reload
systemctl stop "${LEGACY_UNITS[@]}"
systemctl enable --now relay.service

healthy=0
status_output=""
for _ in {1..30}; do
  if status_output="$(owner_relay service status 2>&1)"; then
    healthy=1
    break
  fi
  sleep 1
done
if (( healthy != 1 )); then
  echo "relay: unified service did not become healthy" >&2
  printf '%s\n' "$status_output" >&2
  exit 1
fi

for unit in "${LEGACY_UNITS[@]}"; do
  if systemctl is-active --quiet "$unit"; then
    echo "relay: legacy unit remained active: $unit" >&2
    exit 1
  fi
done

MAIN_PID="$(systemctl show relay.service -p MainPID --value)"
[[ "$MAIN_PID" =~ ^[1-9][0-9]*$ ]] || { echo "relay: invalid unified MainPID" >&2; exit 1; }
[[ "$(pgrep -fc "^$RELAY_BIN service run$")" -eq 1 ]] || {
  echo "relay: expected exactly one unified service process" >&2
  exit 1
}
for socket in "$STATE_ROOT/relayd.sock" "$STATE_ROOT/desktop-bridge.sock"; do
  ss -xlpn | grep -F "$socket" | grep -F "pid=$MAIN_PID" >/dev/null || {
    echo "relay: unified MainPID does not own $socket" >&2
    exit 1
  }
done

rollback_needed=0
trap - EXIT
echo "relay: unified cutover healthy (pid=$MAIN_PID build=$(RELAY_BRIDGE_LOCAL_INVOKE=1 owner_relay build))"
owner_relay service status
