# relay — greenfield session + handoff control plane

Date: 2026-07-24  
Status: Implemented (v0.1.0)  
Supersedes: sst session/handoff/pane orchestration for new work

## Problem

`sst` conflated SSH transport, tmux persistence, cmux visualization, agent launch, and event-driven handoff into one bash CLI. Failures were cross-layer; recovery lived in skills.

## Decisions

1. Greenfield binary `relay` (Go).
2. One API for humans and agents.
3. Host profiles authoritative on each remote (`~/.config/relay/host.yaml`).
4. Four ports — Transport / Persistence / Viz / Coord — with SSH / tmux / cmux / relayd as defaults only.
5. Viz never owns lifecycle; `present(session_id)` is optional.
6. Explicit session/handoff IDs only.
7. Coord is always-on per-host `relayd` (Unix socket only; no TCP). Campus IPS-safe: one SSH subscribe stream, backoff-limited reconnect, bootstrap via single `ssh cat` upload.

## Host profile schema

```yaml
version: 1
host_id: c1
agents:
  - name: claude
    command: claude
path_map:
  - match: relay
    remote_cwd: ~/gh/relay
defaults:
  preferred_agent: claude
  silence_sec: 45
```

Local cache: `~/.local/state/relay/hosts/<host>.json` (read-through; remote wins).

## State

| Location | Contents |
|----------|----------|
| `~/.local/state/relay/sessions.json` | session registry |
| `~/.local/state/relay/handoffs/*.json` | handoff records |
| `~/.local/state/relay/handoffs/ledger.jsonl` | start/end ledger |
| remote `~/.local/state/relay/events/<tmux-name>.jsonl` | event log (owned by relayd) |
| remote `~/.local/state/relay/relayd.sock` | relayd Unix socket (0600; no TCP) |
| remote `~/.config/relay/host.yaml` | host profile |

Bootstrap: `relay host bootstrap -H HOST` installs `~/.local/bin/relayd` + user systemd unit (or nohup).

## New-machine onboarding

```text
relay targets [--json]                 # parse ~/.ssh/config (+ Include) → Host aliases
relay host discover -H HOST [--json]   # reachability, tmux, relayd, agent catalog, path_map proposal
relay host init -H HOST [--apply] [--force]
                                       # bootstrap relayd; write proposed host.yaml (--force to overwrite)
```

SSH config remains connection source of truth. Discover never writes; init dry-runs without `--apply`.

## Handoff SM

`pending → running → needs_input ↔ running → done|failed|abandoned`

Events: `started`, `idle`, `needs_input`, `note`, `inject`, `exit` with monotonic `seq`. Resume with `--from SEQ`.

## sst migration

| sst | relay |
|-----|-------|
| `sst query --json --live` | `relay session list --json` + host probe |
| `sst exec/capture/send` | `relay session exec/capture/send` |
| `sst pane pair/remote` | `relay viz present` |
| `sst handoff` | `relay handoff` |
| `sst events tail -f` | `relay events tail -f --handoff` |
| `sst handoff finalize` | `relay handoff finalize` |
| convention `~/dev→gh` | host `path_map` |
| skill auth ladder | `relay host probe` + profile agents |

Deprecate sst skills once hosts are profiled and daily flows use relay.
