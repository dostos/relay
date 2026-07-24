---
name: relay-sessions
description: Use for relay remote sessions. Prefer relay agent for handoffs; use session verbs only for ad-hoc exec/capture. New machines: targets → discover → init.
---

# relay sessions

## New machine

```bash
relay targets --json                          # ssh Host aliases (local, no SSH)
relay host discover -H HOST --json            # inventory + proposed host.yaml
relay host init -H HOST --apply               # bootstrap relayd + write proposal
# existing host.yaml: add --force to overwrite
```

Follow `next` / `argv` on the discover/init cards. Do not hand-edit connection coords — ssh config is source of truth.

## Ad-hoc sessions

Handoffs → skill **relay-handoff** (`relay agent …`). Ad-hoc only:

```bash
relay session create -H HOST --json
relay session list --json
relay session capture sess-… -n 100
relay session send sess-… -- "…"
relay session exec sess-… -- "…"
relay viz present|close sess-…
```

After cmux quit/reopen: panes re-attach via Vault (`relay install-cmux-restore`) or manually `relay viz save` then `relay viz restore`.
In a relay pane, bare `relay resume` uses that surface’s pane history (`~/.local/state/relay/panes/`); pass `--session <persistName>` to pin a different session.

**Never** `session attach` from an agent. Host profile: `~/.config/relay/host.yaml` on the remote.
