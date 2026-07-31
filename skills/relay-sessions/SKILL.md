---
name: relay-sessions
description: Use for relay remote sessions. Prefer relay agent for handoffs; use session verbs only for ad-hoc exec/capture. New machines: targets → discover → init.
---

# relay sessions

## New machine

```bash
relay targets --json                          # ssh Host aliases (local, no SSH)
relay host discover -H HOST --json            # inventory + proposed host.yaml
relay host init -H HOST --apply               # install relay+relayd + write proposal
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
relay HOST NAME                               # named tmux in current cmux pane
```

The shorthand starts or reuses `NAME`, binds the current cmux surface, and
keeps a reverse Unix-socket bridge on its reconnecting SSH attach. From inside
that remote pane, another `relay HOST NAME` or `relay agent start -H …` is
forwarded to the desktop and opens the next pane in the parent's recorded cmux
workspace. The first child splits right of the parent; later children from the
same parent split down from the newest live sibling. Placement is based on the
session binding, not current focus. Do not start cmux on a remote host.

```bash
relay session create -H HOST --json
relay session list --json
relay session capture sess-… -n 100
relay session send sess-… -- "…"
relay session exec sess-… -- "…"
relay session rename sess-… beholder          # true tmux/checkpoint rename; keeps session id + lineage
relay viz present|close sess-…
relay pane list                                # exact workspace/pane/parent bindings
relay pane rename sess-… personal-db           # persistent UI alias; tmux identity stays stable
relay history                                 # durable source → destination graph
relay parent register                         # make current local pane a lineage parent
relay parent link --parent sess-… --handoff ho-… # adopt existing goal orchestration
relay session bridge sess-…                  # repair legacy adopted pane, no restart
relay parent status SESSION_ID                # token-efficient retirement gate
```

After cmux quit/reopen: panes re-attach via Vault (`relay install-cmux-restore`) or manually `relay viz save` then `relay viz restore`.
In a relay pane, bare `relay resume` uses that surface’s pane history (`~/.local/state/relay/panes/`); pass `--session <persistName>` to pin a different session.

**Never** `session attach` from an agent. Host profile: `~/.config/relay/host.yaml` on the remote.
