#!/usr/bin/env bash
# C — fan-in: one orchestrator blocks on N agents at once; first message wins.
# Each channel has its own seq space → per-channel cursors (NAME:SEQ).
# Usage: HOST=hamburg ./fan-in.sh
set -euo pipefail
HOST="${HOST:-hamburg}"
A="demo-a-$$"; B="demo-b-$$"; C="demo-c-$$"

echo "[setup] give channel $B some backlog we want to skip"
relay msg send -H "$HOST" -c "$B" --from b --kind note --text "old chatter" >/dev/null

echo "[agent-c] will report late (2s)"
( sleep 2; relay msg send -H "$HOST" -c "$C" --from c --kind done --text "C finished" >/dev/null ) &

echo "[orchestrator] wait on all three, skipping B's backlog with B:1"
relay msg wait -H "$HOST" -c "$A:0" -c "$B:1" -c "$C:0" --timeout 30 --json \
  | python3 -c 'import sys,json;m=json.load(sys.stdin)["message"];print("  woke on %s: %s -> %s" % (m["channel"], m["from"], m["text"]))'
wait

# cleanup
ssh -o BatchMode=yes "$HOST" "rm -f ~/.local/state/relay/events/chan.demo-*.jsonl" 2>/dev/null || true
