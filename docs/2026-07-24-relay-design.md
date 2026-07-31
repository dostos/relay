# relay — session + handoff control plane

Date: 2026-07-24  
Status: Implemented (v0.1.0)

## Problem

SSH transport, tmux persistence, cmux visualization, agent launch, and event-driven handoff need a single control plane with clear ports — not a conflated bash CLI where failures cross layers and recovery lives in skills.

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
| local `~/.local/state/relay/desktop-bridge.sock` | remote relay → desktop control/ cmux bridge (0600) |
| local `~/.local/state/relay/viz/*.json` | authoritative session → surface/workspace/pane/parent binding |
| local handoff ledger | session nodes and source → target lineage edges |

Bootstrap: `relay host bootstrap -H HOST` installs `~/.local/bin/relay`,
`~/.local/bin/relayd`, and the relayd user systemd unit (or nohup).

## Named pane bridge

`relay HOST NAME` creates or adopts `NAME`, binds the current cmux surface,
and attaches through a dedicated reconnecting SSH connection. That connection
reverse-forwards the remote session socket to the local desktop bridge. Remote
relay invocations carry the current session identity and are serialized by the
desktop bridge before they touch the local registry or cmux. No component
listens on TCP and remotes never call cmux directly.

Every created session writes a `session_start` ledger record. Cross-host
requests add `source_session_id`; handoffs also add `created_by_handoff_id`.
`relay history` reconstructs the durable source → target graph from that
append-only data.

## Pane ownership and placement

The desktop bridge owns one versioned cmux binding per relay session. A binding
records the exact surface, pane, workspace, attach command, placement mode, and
source session. Session IDs are authoritative; titles/checkpoints are only a
legacy recovery aid.

For an automatic child handoff, the source session's recorded pane is the
anchor even if cmux focus moved elsewhere. Its first child uses `new-split
right`, creating a right-hand column. Later live children with the same source
use `new-split down` from the newest sibling. Explicit `--workspace` or
`--pane` values opt out of this default placement.

`relay resume` rebinds a restored surface before attaching, replacing obsolete
surface references. `relay viz save` reconciles bindings across all cmux
workspaces, and `relay pane list` reports live/disconnected ownership without
requiring cmux to be running.

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

## Local parents and decision inbox

This parent/child path is the durable orchestration contract for long-lived,
goal-based handoffs. It deliberately carries correlated decisions and terminal
receipts rather than conversation history or transcripts, and persists across
agent processes, SSH reconnects, nested relays, and cmux restarts.

A cmux main-agent surface is registered as a normal session with
`host_id=local`, `persist.kind=cmux`, a stable session id, scoped Git roots,
and an authoritative viz binding. Local registration never changes the pane's
command or resume checkpoint. Handoffs launched there carry the parent session
id exactly like remote-to-remote bridge launches.

Existing goals can be migrated with `relay parent link sess-… ho-…`; the link
is one-time and starts the same blocking watcher.
Already-running adopted tmux agents discover an owner-only bridge identity by
their current tmux session name. `relay session bridge sess-…` provisions this
identity for legacy registry entries without restarting or exposing its token
to the agent transcript.

Each parented handoff starts one detached watcher using a single blocking
relayd subscription. The watcher routes only actionable events into
`parent-inbox/<parent>/<message>.json`: `ask`, `permission_required`, `result`,
and `exit`; ambiguous agent `idle` becomes a short decision request with at
most four captured lines. Envelopes are bounded and deduplicated by
parent/handoff/kind/sequence, so replays do not notify twice.

The cmux adapter sends a desktop notification and flash to the exact bound
surface. Agent parents additionally receive one compact prompt containing the
message id and the exact `parent reply` or `parent ack` verb. Replies inject
only the decision text into the child and emit a correlated `inject` event.
Request, reply, and acknowledgment records are appended to `relay history`.

`relay signal` is the provider-neutral hook protocol. Relay injects native
PermissionRequest/Stop adapters for Codex and Claude, wraps every agent for an
exit signal, and keeps tmux silence as the fallback for CLIs without hooks.
`relay hook` accepts bounded JSON stdin from any vendor hook and extracts only
small useful fields.

Local-parent retirement is fail-closed. It requires: explicit idle/complete
state; no nonterminal child handoffs; no pending inbox messages; and every
scoped Git root clean, reachable upstream, and zero commits ahead after a
fresh fetch. Only then is the exact parent surface closed and its registry
record removed. Generic session destruction and GC cannot reap local parents.
