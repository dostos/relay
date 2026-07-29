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

## Agent auth

Works for any agent type (`claude`, `cursor-agent`, `codex`, `ccs:personal`, `ccs:hcs`, …):

```bash
relay auth status -H HOST --json
relay auth login -H HOST --agent claude   # pane + reassemble wrapped OAuth URL + open locally
relay auth url --session sess-…           # if the pane still looks cropped
relay auth copy --from c3 --to c1 --agent ccs:personal   # when copy_supported
```

Narrow cmux panes wrap long OAuth URLs mid-token (`https://claude.com/ca` / `i/oauth/…`). `auth login` / `auth url` stitch them and `open` on the Mac. `RELAY_NO_OPEN=1` skips the browser.

Probes/launches use a login shell so nvm/`~/.local/bin` agents are visible.

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
