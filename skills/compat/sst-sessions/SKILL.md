---
name: sst-sessions
description: DEPRECATED — sst sessions are replaced by relay. Use when an agent would previously call sst query/exec/capture/send; redirect to relay-sessions for host profiles and explicit session ids.
---

# DEPRECATED: use `relay-sessions`

`sst` session/query/exec flows are retired for daily use. Use **`relay`** (`~/dev/relay`):

| Was (sst) | Now (relay) |
|-----------|-------------|
| `sst query --json --live` | `relay session list --json` / `relay host probe -H HOST` |
| `sst exec -H HOST -s SESS -- …` | `relay session exec sess-… -- …` |
| `sst capture …` / `sst send …` | `relay session capture` / `relay session send` |
| cmux panes by guess | `relay viz present\|focus\|close <session_id>` |

Load skill **`relay-sessions`**. Host profiles live on the remote: `~/.config/relay/host.yaml`.

```bash
cd ~/dev/relay && ./install.sh
relay session create -H HOST --json
```

**Do not** interactive-attach from an agent (`relay session attach` is human-only).
