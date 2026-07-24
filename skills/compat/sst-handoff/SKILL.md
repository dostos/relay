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
| `sst events …` / JSONL `tail -F` | `relay agent wait --handoff ho-…` (blocking; no loops) |
| finalize / ledger | `relay agent done --handoff ho-…` |

Load skill **`relay-handoff`**. Follow JSON `next`/`argv`. Requires `relay host bootstrap -H HOST` once.

```bash
cd ~/dev/relay && ./install.sh
relay doctor -H HOST --json
```
