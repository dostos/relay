---
name: sst-handoff
description: DEPRECATED — sst handoff is replaced by relay. Use when an agent would previously call sst handoff; redirect to relay-handoff for remote agent/job goals, event tail, inject/escalate, finalize.
---

# DEPRECATED: use `relay-handoff`

`sst handoff` is retired for daily use. Use **`relay`** (`~/dev/relay`):

| Was (sst) | Now (relay) |
|-----------|-------------|
| `sst handoff -H HOST --agent … --goal …` | `relay handoff -H HOST --agent … --goal …` |
| `sst handoff -H HOST --cmd …` | `relay handoff -H HOST --cmd …` |
| `sst events …` / JSONL `tail -F` | `relay events tail -f --handoff ho-… --from N` |
| finalize / ledger | `relay handoff finalize ho-…` |

Load skill **`relay-handoff`** and follow it. Requires `relay host bootstrap -H HOST` once (always-on `relayd`, Unix socket only).

```bash
cd ~/dev/relay && ./install.sh
relay doctor -H HOST --json
```
