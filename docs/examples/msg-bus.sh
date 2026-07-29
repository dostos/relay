#!/usr/bin/env bash
# A — channel bus: two agents coordinate over a named channel.
# Usage: HOST=hamburg ./msg-bus.sh
set -euo pipefail
HOST="${HOST:-hamburg}"
CH="demo-teamX-$$"

echo "[agentA] publish a note, a result, then a question"
relay msg send -H "$HOST" -c "$CH" --from agentA --kind note   --text "starting build"
relay msg send -H "$HOST" -c "$CH" --from agentA --kind result --text "built ok" --meta '{"pr":42}'
relay msg send -H "$HOST" -c "$CH" --from agentA --kind ask    --text "deploy to prod?"

echo "[agentB] drain the channel (one shot) and note the cursor"
NEXT=$(relay msg read -H "$HOST" -c "$CH" --from 0 --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["next_from"])')
echo "[agentB] next_from=$NEXT — reply to the question"
relay msg send -H "$HOST" -c "$CH" --from agentB --kind reply --text "yes, deploy"

echo "[agentA] block for the reply (from the cursor it already had)"
relay msg wait -H "$HOST" -c "$CH" --from "$NEXT" --timeout 30 --json \
  | python3 -c 'import sys,json;m=json.load(sys.stdin)["message"];print("  <- %s: %s" % (m["from"], m["text"]))'

# cleanup
ssh -o BatchMode=yes "$HOST" "rm -f ~/.local/state/relay/events/chan.$CH.jsonl" 2>/dev/null || true
