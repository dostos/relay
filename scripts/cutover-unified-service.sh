#!/usr/bin/env bash
# Perform the explicitly authorized first cutover or unified-service upgrade.
# Run via sudo; this script never accepts or handles the owner's credential.
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
PREVIOUS_RELAY_BIN="$RELAY_BIN.previous"
UNIT_BACKUP="$STATE_ROOT/relay-system.service.previous"
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

MODE=cutover
if [[ -e "$TARGET" || -L "$TARGET" ]]; then
  [[ -f "$TARGET" && ! -L "$TARGET" ]] || {
    echo "relay: refusing non-regular existing unit $TARGET" >&2
    exit 2
  }
  # An existing unit is upgradeable only when it is demonstrably Relay's unit
  # for this exact owner. This permits idempotent upgrades without turning the
  # script into a generic systemd-unit overwrite primitive.
  grep -Fxq 'Description=Relay authoritative home service' "$TARGET" &&
    grep -Fxq "User=$RELAY_USER" "$TARGET" &&
    grep -Fxq "Group=$RELAY_GROUP" "$TARGET" &&
    grep -Fxq "ExecStart=$RELAY_BIN service run" "$TARGET" || {
      echo "relay: refusing to overwrite unrecognized existing $TARGET" >&2
      exit 2
    }
  MODE=upgrade
fi

REPO_COMMIT="$(runuser -u "$RELAY_USER" -- git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || true)"
INSTALLED_BUILD="$(RELAY_BRIDGE_LOCAL_INVOKE=1 owner_relay build 2>/dev/null || true)"
if [[ -z "$REPO_COMMIT" || ( "$INSTALLED_BUILD" != "$REPO_COMMIT" && "$INSTALLED_BUILD" != "$REPO_COMMIT"-dirty.* ) ]]; then
  echo "relay: installed build $INSTALLED_BUILD does not match repository $REPO_COMMIT" >&2
  echo "relay: run $REPO_ROOT/install.sh as $RELAY_USER, then retry this command" >&2
  exit 2
fi

if [[ "$MODE" == cutover ]]; then
  for unit in "${LEGACY_UNITS[@]}"; do
    systemctl is-active --quiet "$unit" || {
      echo "relay: expected active rollback unit $unit" >&2
      exit 2
    }
  done
else
  systemctl is-active --quiet relay.service || {
    echo "relay: existing relay.service is not active" >&2
    exit 2
  }
  for unit in "${LEGACY_UNITS[@]}"; do
    if systemctl is-active --quiet "$unit"; then
      echo "relay: refusing upgrade while legacy unit remains active: $unit" >&2
      exit 2
    fi
  done
  [[ -f "$PREVIOUS_RELAY_BIN" && ! -L "$PREVIOUS_RELAY_BIN" ]] || {
    echo "relay: missing rollback binary $PREVIOUS_RELAY_BIN (run install.sh first)" >&2
    exit 2
  }
  install -o "$RELAY_USER" -g "$RELAY_GROUP" -m 0600 "$TARGET" "$UNIT_BACKUP"
fi

rollback_needed=1
rollback() {
  rc=$?
  trap - EXIT
  if (( rollback_needed )); then
    if [[ "$MODE" == upgrade ]]; then
      echo "relay: upgrade failed; restoring previous unified binary and unit" >&2
      systemctl stop relay.service >/dev/null 2>&1 || true
      install -o "$RELAY_USER" -g "$RELAY_GROUP" -m 0755 "$PREVIOUS_RELAY_BIN" "$RELAY_BIN" || true
      install -m 0644 "$UNIT_BACKUP" "$TARGET" || true
      systemctl daemon-reload || true
      systemctl start relay.service || true
    else
      echo "relay: cutover failed; restoring compatibility services" >&2
      systemctl disable --now relay.service >/dev/null 2>&1 || true
      if [[ -e "$TARGET" || -L "$TARGET" ]]; then
        unlink -- "$TARGET"
      fi
      systemctl daemon-reload || true
      systemctl start "${LEGACY_UNITS[@]}" || true
    fi
  fi
  exit "$rc"
}
trap rollback EXIT

install -m 0644 "$PROPOSAL" "$TARGET"
systemd-analyze verify "$TARGET"
systemctl daemon-reload
if [[ "$MODE" == upgrade ]]; then
  systemctl restart relay.service
else
  systemctl stop "${LEGACY_UNITS[@]}"
  systemctl enable --now relay.service
fi

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
