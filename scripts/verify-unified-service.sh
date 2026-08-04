#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERIFY_ROOT="$(mktemp -d)"
VERIFY_BIN="$VERIFY_ROOT/relay"
VERIFY_STATE="$VERIFY_ROOT/state"
VERIFY_CONFIG="$VERIFY_ROOT/config"
VERIFY_SOCKET="$VERIFY_ROOT/event.sock"
SERVICE_PID=""

stop_service() {
  if [[ -n "$SERVICE_PID" ]] && kill -0 "$SERVICE_PID" 2>/dev/null; then
    kill "$SERVICE_PID"
    wait "$SERVICE_PID"
  fi
  SERVICE_PID=""
}
trap stop_service EXIT

mkdir -p "$VERIFY_CONFIG"
printf 'version: 1\nhost_id: disposable-home\n' > "$VERIFY_CONFIG/host.yaml"
(
  cd "$REPO_ROOT"
  go build -ldflags "-X github.com/dostos/relay/internal/coord.Build=disposable-verifier" -o "$VERIFY_BIN" ./cmd/relay
)

export RELAY_STATE_DIR="$VERIFY_STATE"
export RELAY_CONFIG_DIR="$VERIFY_CONFIG"
export RELAYD_SOCK="$VERIFY_SOCKET"
unset RELAY_BRIDGE_SOCK RELAY_BRIDGE_LOCAL_INVOKE RELAY_SOURCE_SESSION_ID RELAY_SOURCE_HOST_ID RELAY_SOURCE_PERSIST_NAME RELAY_SOURCE_TOKEN
unset RELAY_SESSION_ID RELAY_SESSION_HOST RELAY_SESSION_NAME

start_service() {
  "$VERIFY_BIN" service run >> "$VERIFY_ROOT/service.log" 2>&1 &
  SERVICE_PID=$!
  for _ in {1..100}; do
    if "$VERIFY_BIN" service status > "$VERIFY_ROOT/health.json" 2>/dev/null; then
      return 0
    fi
    sleep 0.02
  done
  sed -n '1,160p' "$VERIFY_ROOT/service.log" >&2
  return 1
}

start_service
"$VERIFY_BIN" service status
if "$VERIFY_BIN" service run > "$VERIFY_ROOT/second-owner.log" 2>&1; then
  printf 'second service unexpectedly acquired authority\n' >&2
  exit 1
fi
"$VERIFY_BIN" version
"$VERIFY_BIN" service event emit -s verifier --kind started

latencies=()
for _ in {1..20}; do
  start_ns="$(date +%s%N)"
  "$VERIFY_BIN" service event emit -s verifier --kind progress >/dev/null
  end_ns="$(date +%s%N)"
  latencies+=("$(((end_ns-start_ns)/1000000))")
done
mapfile -t sorted_latencies < <(printf '%s\n' "${latencies[@]}" | sort -n)
printf 'emit_latency_ms p50=%s p95=%s max=%s\n' "${sorted_latencies[9]}" "${sorted_latencies[18]}" "${sorted_latencies[19]}"
"$VERIFY_BIN" service event emit -s verifier --kind result
"$VERIFY_BIN" service event subscribe -s verifier --from 20

stop_service
if "$VERIFY_BIN" service status >/dev/null 2>&1; then
  printf 'stopped service still reported healthy\n' >&2
  exit 1
fi
start_service
restart_response="$("$VERIFY_BIN" service event emit -s verifier --kind after_restart)"
if [[ "$restart_response" != *'"seq":23'* ]]; then
  printf 'event cursor did not survive whole-service restart: %s\n' "$restart_response" >&2
  exit 1
fi
printf '%s\n' "$restart_response"
"$VERIFY_BIN" service status

receipt_count="$(find "$VERIFY_STATE/command-receipts" -type f -name '*.json' | wc -l | tr -d ' ')"
if [[ "$receipt_count" -lt 1 ]]; then
  printf 'stateless CLI produced no durable command receipt\n' >&2
  exit 1
fi
printf 'command_receipts=%s\n' "$receipt_count"
printf 'verification_root=%s\n' "$VERIFY_ROOT"
