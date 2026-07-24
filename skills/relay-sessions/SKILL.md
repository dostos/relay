---
name: relay-sessions
description: Use for relay (or legacy sst) remote sessions. Prefer relay agent for handoffs; use session verbs only for ad-hoc exec/capture. Replaces sst-sessions.
---

# relay sessions

Handoffs → skill **relay-handoff** (`relay agent …`). Ad-hoc only:

```bash
relay session create -H HOST --json
relay session list --json
relay session capture sess-… -n 100
relay session send sess-… -- "…"
relay session exec sess-… -- "…"
relay viz present|close sess-…
```

After cmux quit/reopen: panes re-attach via Vault (`relay install-cmux-restore`) or manually `relay viz save` then `relay viz restore`. Manual one-shot: `relay resume --session <persistName>`.

**Never** `session attach` from an agent. Host profile: `~/.config/relay/host.yaml` on the remote.
