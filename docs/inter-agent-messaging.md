# Inter-agent messaging on relay

Three mechanisms, all riding the existing **relayd** bus (append-only, per-name
`.jsonl` with authoritative seq + `subscribe`). No new daemon, no `tail -f`
poll loop — one blocking read per turn, the same discipline as `agent wait`.

They are **complementary, not competing**:

| | A — channel bus | B — explicit signaling | C — fan-in |
|---|---|---|---|
| Enables | agent ↔ agent, orchestrator fan-in | orchestrator ↔ agent (declared state) | one waiter ↔ N agents |
| Surface | `relay msg send/read/wait` | existing `relay agent wait` | `relay msg wait -c a -c b …` |
| Direction | N ↔ N, any channel | agent → orchestrator | N → 1 |
| Replaces | (new capability) | idle-guessing + `capture` scraping | per-agent polling |
| Cost | tiny (thin wrapper over relayd) | tiny (policy in `DecideNext`) | part of A |
| Coupling | decoupled from handoffs | tied to a handoff/session | decoupled |

## A — channel bus (`relay msg`)

Any channel name (namespaced `chan.<name>` on the host's relayd). Messages are
relayd events with `from`/`text` lifted out of `meta`; `read`/`wait` return a
`next_from` cursor so the caller never re-reads or spins.

```bash
# publisher (any agent, on any host that can reach the channel's relayd)
relay msg send -H hamburg -c teamX --from A --kind result --text "built ok" --meta '{"pr":42}'

# consumer: drain backlog (one shot, returns cursor)
relay msg read -H hamburg -c teamX --from 0
#  {"messages":[{"seq":1,"kind":"result","from":"A","text":"built ok","meta":{"pr":42}}],"next_from":1}

# consumer: block for the next one (one turn), then thread next_from
relay msg wait -H hamburg -c teamX --from 1 --timeout 60
```

Workflow: [`examples/msg-bus.sh`](examples/msg-bus.sh).

## B — explicit goal-handoff signaling

This is Relay's long-lived goal orchestration channel, not agent chat. An agent
**declares** its state instead of falling silent. It emits a typed
event into its own session stream; the orchestrator's existing `agent wait`
surfaces it — no `idle` heuristic, no screen-scrape.

```bash
# inside the agent's pane / wrapper (relayd is on the host):
relayd emit -s "$RELAY_SESSION" --kind ask    --meta '{"q":"which env: prod or dev?"}'
relayd emit -s "$RELAY_SESSION" --kind note   --meta '{"text":"cloned repo, building"}'
relayd emit -s "$RELAY_SESSION" --kind result --meta '{"text":"tests pass","meta":{"cov":0.91}}'
```

The portable hook surface is shorter and uses Relay's injected session name:

```bash
relay signal ask --text "prod or dev?" --correlation env-choice
relay signal permission_required --text "approve deploy" --correlation deploy-1
relay signal result --text "tests pass"

# From a vendor hook (JSON on stdin is bounded; only useful fields are kept):
relay hook --kind permission_required
```

Codex and Claude handoffs receive native permission/result hook adapters by
default. Other CLIs still get an exit wrapper and can invoke the same commands
from their hook system; `relay_hooks: off` disables injection per agent.

For a parented handoff these events are also written to the parent's durable
inbox and wake its exact cmux pane. Use `relay parent reply`/`ack`; do not send
transcripts through the channel bus. Correlation and durable acknowledgments
keep the goal valid across nested handoffs, reconnects, and agent replacement.

`agent wait` maps them: `ask → next=send` (with `text` = the question),
`note`/`progress`/`result` → surfaced but `next=wait`. `exit` still finalizes.

Workflow: [`examples/agent-signaling.sh`](examples/agent-signaling.sh).

## C — fan-in

`msg wait` takes multiple `--channel`; the first new message on any channel
wins. **Each channel has its own seq space**, so a single `--from` is a footgun
— pass a per-channel cursor as `NAME:SEQ` (or thread each `next_from`).

```bash
# supervise 3 agents at once; skip each channel's backlog with its own cursor
relay msg wait -H hamburg -c a:12 -c b:4 -c c:0 --timeout 120
#  {"message":{"channel":"b","seq":5,"kind":"done","from":"b"}, "next_from":5}
```

Cross-host: relayd is per-host, so pick one reachable box as the rendezvous
(e.g. `hamburg`) and have every agent publish there. A local-Mac relayd
rendezvous is possible too (`relayd` runs locally) but a shared remote host
needs zero extra infra.

Workflow: [`examples/fan-in.sh`](examples/fan-in.sh).

## Verdict — what to use

- **Ship A + B.** They are orthogonal and cover the two real needs: A is the
  only path for **agent ↔ agent** and fan-in; B makes the **orchestrator ↔
  agent** loop honest (declared state beats inferring "idle" from tmux silence
  and scraping the pane). Both are ~a few hundred lines over relayd.
- **C is A's `wait` over N channels** — take it for free, but always use
  per-channel cursors.
- **If you must pick ONE primitive:** the **channel bus (A)**. It is the general
  substrate — B is expressible as "agent publishes to its own channel," and C is
  just A over N channels. B remains worth keeping because it upgrades the
  *existing* handoff loop with no new caller surface.

### Cleanup

Channels are relayd `.jsonl` files, so they persist until removed. Drop one
explicitly when a task is done — `relay msg rm -H HOST -c CHANNEL` — or let the
unified sweep handle stale/empty ones:

```bash
relay gc [-H HOST] [--dry-run] [--channel-ttl DAYS]   # reap dead sessions +
                                                      # prune tombstones +
                                                      # drop stale panes +
                                                      # GC empty/old channels
```

`relay gc` is the single "clean up when done" entry point: one probe SSH per
host (tmux sessions + channels in one exec), unreachable hosts skipped,
`--dry-run` to preview. `relay resume reap` is the same sweep scoped to
sessions only.

### Storm safety

Every read is a single blocking `subscribe` (ControlMaster-reused), bounded by
`--timeout`; fan-in opens one stream per channel (small N). No client-side poll
loop — on timeout you get `next:"wait"` and call again on a new turn. This keeps
the same IPS-safe posture as the rest of relay.
