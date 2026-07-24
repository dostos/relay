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

## Handoffs (goal-based / long-running)

```bash
relay handoff -H c1 --agent claude --goal "port the scheduler; keep tests green"
# prints JSON binding {handoff_id, session_id, watch, …}

relay events tail -f --handoff ho-… --from 0
relay handoff finalize ho-… --outcome done
relay viz close sess-…            # optional; finalize does not require viz
```

State machine: `pending → running → needs_input ↔ running → done|failed|abandoned`.

## Adapters

Core never imports SSH/tmux/cmux quirks into business logic. See `internal/ports` and:

- `internal/transport/ssh`
- `internal/persist/tmux`
- `internal/viz/cmux`

## Deprecating sst

`relay` is the replacement for `sst` session/handoff/pane flows. Keep `sst` installed until your hosts have `host.yaml` and daily flows are migrated; then remove skill symlinks for `sst-sessions` / `sst-handoff` and use `relay-*` skills instead. See `docs/2026-07-24-relay-design.md`.

## Develop

```bash
go test ./...
go build -o bin/relay ./cmd/relay
```
