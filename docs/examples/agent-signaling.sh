#!/usr/bin/env bash
# B — explicit agent signaling: the agent DECLARES state; `agent wait` surfaces
# it (no idle-guessing, no pane scraping).
# Usage: HOST=hamburg ./agent-signaling.sh
set -euo pipefail
HOST="${HOST:-hamburg}"

echo "[orchestrator] start a handoff"
J=$(relay agent start "$HOST" cursor --cwd '~' --name sig-demo -- \
      "Wait quietly; take no action." --json)
HID=$(echo "$J" | python3 -c 'import sys,json;print(json.load(sys.stdin)["handoff_id"])')
SID=$(echo "$J" | python3 -c 'import sys,json;print(json.load(sys.stdin)["session_id"])')
PERSIST=$(relay session list --json | python3 -c "import sys,json;print(next(s['persist']['name'] for s in json.load(sys.stdin) if s['id']=='$SID'))")

echo "[agent] declares a question instead of going silent"
ssh -o BatchMode=yes "$HOST" "\$HOME/.local/bin/relayd emit -s $PERSIST --kind ask --meta '{\"q\":\"which env: prod or dev?\"}'"

echo "[orchestrator] agent wait surfaces it as next=send with the question text"
relay agent wait "$HID" --from 0 --timeout 15 --json \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print("  event=%s next=%s text=%r" % (d["event"]["kind"], d["next"], d["event"].get("text")))'

echo "[orchestrator] answer, then finalize (pane closes on done)"
relay agent send "$HID" -- "dev" >/dev/null
relay agent done "$HID" --outcome done >/dev/null
