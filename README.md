# relay

Greenfield **session + handoff control plane**. Replaces the conflated `sst` workflow with a clean core and pluggable adapters:

| Port | Default | Role |
|------|---------|------|
| Transport | SSH | Reach a host (swappable) |
| Persistence | tmux | Survive disconnect (swappable) |
| Visualization | cmux | Human window surface (optional, swappable) |
| Coord | relayd | Always-on host event bus (Unix socket only; no TCP) |

One CLI for humans and local agents (`--json`).

## Install

```bash
cd ~/dev/relay
./install.sh          # → ~/.local/bin/relay + skill symlinks
relay doctor --json
```

## Host profiles + relayd (authoritative on each remote)

Each target keeps `~/.config/relay/host.yaml` — agents available there, project path maps, defaults. Local only caches.

Also install always-on **relayd** (Unix socket at `~/.local/state/relay/relayd.sock` — **no TCP listen**; campus-IPS safe):

```bash
relay host example -H c1          # starter YAML
# copy to remote: ~/.config/relay/host.yaml
relay host bootstrap -H c1        # one quiet SSH upload + user systemd/nohup
relay host fetch -H c1
relay host probe -H c1            # PATH + light auth checks + relayd ping
```

## Sessions

Explicit IDs only — no multi-source “bare connect” guessing.

```bash
cd ~/dev/myproject
relay session create -H c1        # uses host path_map for this repo
relay session list --json
relay session capture sess-… -n 80
relay session send sess-… -- "make test"
relay session exec sess-… -- "uname -a"
relay session resize sess-…       # pty resync (tmux adapter)
relay session attach sess-…       # humans only; agents use capture/send
```

## Agent surface (token-efficient; no poll loops)

Orchestrators should use `relay agent` and follow JSON `next` / `argv` — never `events tail -f` in a loop.

```bash
relay agent start -H c1 --agent claude --goal "port the scheduler; keep tests green"
# → {"next":"wait","argv":["relay","agent","wait",…]}
relay agent wait --handoff ho-… [--from N] [--timeout 120]   # blocks once
relay agent send --handoff ho-… -- "…"                       # agent only; refused on jobs
relay agent done --handoff ho-… --outcome done
relay agent status --handoff ho-…                            # resume after compaction
```

Lower-level `relay handoff` / `relay events` remain for humans/debug.

State machine: `pending → running → needs_input ↔ running → done|failed|abandoned`.

## cmux restart restore

Remote tmux survives cmux quit; local panes do not. Relay registers as a cmux Vault agent so panes re-run `relay resume --session …`:

```bash
./install.sh                       # calls install-cmux-restore when cmux is present
relay install-cmux-restore         # idempotent re-register
relay viz save                     # snapshot already-open panes (pre-registration)
relay viz restore                  # manual re-attach after restart (no approval needed)
relay resume --session NAME        # one pane (Vault resumeCommand target)
```

Approve **relay** once under cmux Settings → Terminal → Resume Commands for hands-off auto-restore. Pane launch argv is always `relay resume --session <persistName>` (not raw `ssh`), so cmux can extract the session id.

Resume presence (see `relay resume list`):

| presence | meaning | resume? |
|----------|---------|---------|
| `live` | local session record exists | yes (already tracked) |
| `disconnected` | cmux/SSH dropped; remote may still be up | yes |
| `cleaned` | intentional destroy/finalize | **no** — close the pane |
| `unknown` | never created on this machine | no |

## Adapters

Core never imports SSH/tmux/cmux quirks into business logic. See `internal/ports` and:

- `internal/transport/ssh`
- `internal/persist/tmux`
- `internal/viz/cmux`

## Deprecating sst

`relay` is the replacement for `sst` session/handoff/pane flows. `./install.sh` installs `relay-*` skills and redirects legacy `sst-*` names to cutover shims. See `docs/2026-07-24-relay-design.md`.

## Develop

```bash
go test ./...
go build -o bin/relay ./cmd/relay
```
