# relay

**Durable remote agent panes for [cmux](https://cmux.com)** — with one authoritative home service and a small stateless CLI that agents can drive without poll loops.

`relay` attaches long-lived remote work (SSH + tmux) into cmux workspaces, marks those tabs with a teal **◆ RELAY** badge, and restores them after cmux quit / Mac reboot via cmux Vault. The same installed binary runs the home authority (`relay service run`), optional presentation client (`relay viz serve`), and host-local event edge (`relay service event run`).

<p align="center">
  <img src="docs/images/cmux-relay-hero.jpg" alt="cmux with relay-managed split panes and teal ◆ RELAY workspace badge" width="920" />
</p>

<p align="center"><em>Illustrative UI (anonymized hosts). Real tabs look like <code>◆ RELAY · train</code> with a matching sidebar pill.</em></p>

## Why

cmux is great at local workspace UX. Remote agent work still needs:

1. **Persistence** — the process must survive laptop sleep, wifi blips, and quitting cmux  
2. **Reattach** — panes should come back as the same session, not a fresh shell  
3. **Agent-friendly control** — orchestrators need `start → wait → send → done` without `tail -f` loops  
4. **Visual ownership** — you should see at a glance which tabs are relay-managed  

relay is the thin control plane for that. cmux stays the windowing surface; relay never owns lifecycle through the GUI alone.

## Use cases

### 1) Remote coding agent that survives cmux quit

Start Claude / Codex / cursor-agent in a durable session on a lab box. Close cmux. Reopen later — Vault runs `relay resume --session …` and the pane is back in the same tmux session.

```bash
relay session create -H host-a --name eval -- cursor-agent "fix the flaky eval; keep tests green"
relay resume --session eval --host host-a      # from any pane, later
```

### 2) Project workspace with several durable remotes

One cmux workspace with a parent pane on the left and its relay children stacked
on the right (train + eval, app + benchmark). Sidebar pill:

`◆ RELAY · train, eval`

```bash
relay session adopt -H host-a --name train
relay session adopt -H host-b --name eval
relay viz present sess-… --workspace workspace:N   # split by default
relay viz brand                                    # refresh ◆ RELAY titles + pills
```

For the common interactive path, use the host/name shorthand. It creates (or
reuses) the named remote tmux session and binds it to the **current** cmux pane:

```bash
relay c3 research
```

That pane carries a persistent reverse Unix-socket bridge to the desktop. A
`relay` command run inside it is executed by the desktop control plane, so it
can open the next host in cmux without giving a remote machine direct access to
cmux:

```bash
# inside c3/research
relay c1 followup
```

Child placement follows the recorded session binding, not whichever pane is
currently focused. The first child splits `right` from its parent; later
children of that parent split `down` from the newest live sibling. Explicit
`--workspace` / `--pane` placement overrides the default. Inspect the
session-keyed records with `relay pane list`.

### 3) Bring a new machine online

Discover SSH aliases, probe agent CLIs, propose `host.yaml`, and bootstrap the
host-local event role from the primary binary:

```bash
relay targets --json
relay host discover -H host-a --json
relay host init -H host-a --apply   # installs relay + compatibility symlink
```

## Install

```bash
git clone https://github.com/dostos/relay.git
cd relay
./install.sh          # ~/.local/bin/relay{,d}; CLI is the agent protocol
relay doctor --json
relay install-cmux-restore   # register Vault resume agent (also run by install.sh)
```

In cmux: **Settings → Terminal → Resume Commands** → approve **relay** once.

No runtime-specific agent skill is required or installed. Workspace-level
agent instructions only need to point at `relay agent protocol`.

## Quick start with cmux

```bash
# one-time per remote
relay host init -H HOST --apply

# run work
relay HOST NAME                     # current pane → named remote tmux
relay session create -H HOST --name work -- claude   # or any CLI as the pane's command
# or attach an existing tmux session
relay session adopt -H HOST --name my-tmux
relay viz present sess-…          # opens a cmux split; ◆ RELAY tab title

# after cmux restart (or laptop sleep / wifi drop)
relay resume list                 # live | disconnected | cleaned
relay resume                      # bare: this cmux pane's history
relay resume --session NAME       # pin + waits/retries on SSH drop
relay viz restore                 # optional manual path
```

`relay resume` keeps the pane alive across sleep and “Shared connection … closed”: on drop it shows a single animated status line (spinner + countdown) and retries after a short delay (default 3s), frozen to that pane’s session. Bare `relay resume` (no `--session`) reads `~/.local/state/relay/panes/<surface>.json` (or the cmux surface resume binding). Disable reconnect with `--no-reconnect` or `RELAY_AUTO_RECONNECT=0`.


| Resume presence | Meaning | Resume? |
|-----------------|---------|---------|
| `live` | tracked locally | yes |
| `disconnected` | cmux/SSH dropped; remote may still be up | yes |
| `cleaned` | intentional destroy/finalize | **no** |
| `unknown` | never created here | no |

## How it fits cmux

| Layer | Role |
|-------|------|
| **cmux** | Workspaces, splits, tabs, Vault resume UI |
| **relay CLI** | Session ids, `viz present`, branding, `--json` output for other programs |
| **control bridge** | Unix-socket daemon on the control host; serializes authenticated remote requests |
| **cmux client** | Optional visualization endpoint; executes cmux operations but owns no agent lifecycle |
| **tmux** (remote) | Durable process surface |
| **relay service event** (remote) | Always-on event bus over a **Unix socket only** (no TCP listen) |

Host profiles (`~/.config/relay/host.yaml` on each remote) list agent CLIs and `path_map`. Connection coords stay in your SSH config — relay only uses Host aliases.

During the control-plane migration, the desktop bridge is started on demand by `relay resume`. Its socket is
`~/.local/state/relay/desktop-bridge.sock` (0600). Each attached pane uses SSH
stream-local reverse forwarding to expose a per-session socket under `/tmp` on
the remote host. Requests carry a per-session token, and the bridge allowlist
is limited to named-session operations. There is no TCP listener or
inbound connection to the laptop; the forward lives and reconnects with the
pane's dedicated SSH connection.

An always-on control host can request visualization without moving its registry
or watchers to the Mac. Home sends only session ID, SSH target, and tmux name to
the optional Mac `relay viz serve` service through a durable local queue. The Mac
owns SSH attachment and placement policy, and consumes that queue using its own
outbound SSH connection. Configure home's `~/.config/relay/viz.json`:

The command server also follows the Viz acknowledgement stream and replaces a
queued reference only after the Mac reports the actual surface. This protocol
contains no agent/provider field: interactive sessions, jobs, and any agent CLI
use the same request, receipt, and cursor path.

```json
{
  "service_id": "mac"
}
```

The Mac config names the outbound control connection and an owner-fixed update
policy. Home resolves session host aliases through its SSH config and sends
only host/user/port; credentials remain on the Mac. Optional `targets` entries
can still pin a client-local identity for a host key.

```json
{
  "service_id": "mac",
  "control": {
    "host": "100.108.118.32",
    "user": "dostos",
    "port": 2222
  },
  "command": {
    "host": "home-relay"
  },
  "update": {
    "repo": "~/dev/relay",
    "remote": "origin",
    "branch": "master"
  }
}
```

`command.host` is an OpenSSH alias for ordinary stateless CLI requests from a
projection-only desktop. The alias keeps credentials and per-vantage routes in
the desktop's SSH config; it must reach the authoritative account without an
interactive prompt. The separate `control` identity remains restricted to the
Viz event protocol.

`relay viz update` appends a durable compatibility `update_relayd` signal. The Mac refuses it
when its checkout is dirty or not on the configured branch. Otherwise Relay
fetches the configured ref, builds the primary binary in a detached staging
worktree, verifies its stamped build, fast-forwards the checkout, and atomically
swaps it plus the compatibility symlink with rollback copies. It then
acknowledges with the installed commit in `result`, advances its cursor, and
lets launchd restart the follower.
After the home bridge has been verified for local and worker sessions,
`relay viz retire-control` durably asks the Mac to boot out and unregister the
legacy supervisor and stop only a verified legacy bridge socket owner. The
Viz follower and cmux restoration remain installed, and the Mac acknowledges
the exact retirement result before advancing its cursor.
`install.sh` is only for initial binary/service bootstrap. Both outbound control and target attachment are
batch-only with strict host-key checking. If the Mac is asleep, requests wait
durably while control work continues on home.

Remote-to-remote commands require a session created by this bridge-aware relay
version. The shorthand can still adopt an older tmux session for attachment,
but it warns that the legacy shell has no bridge identity; choose a new `NAME`
to enable chaining.

## CLI map

```text
relay targets / host discover / host init   # new machine
relay HOST NAME                             # named tmux in current cmux pane
relay session … / session adopt             # durable tmux (create takes -- ARGV for the pane's command)
relay container up|down|status|stop|start   # devcontainer or image-backed instance
relay auth status|login|copy                # agent CLI logins on a host
relay service run|status                    # unified home service and component health
relay pane list / pane rename               # owned surface/workspace/pane + liveness
relay viz present|brand|save|restore        # cmux surface
relay resume --session NAME                 # Vault target
```

Retired 2026-09-13 (workspace decision *one owner per axis*): the delegation
handshake — `agent`, `handoff`, `parent`, `resolve`, `ask`, `signal`, `board`,
`policy`, `msg`, `events`, `log`, `root`, `gc`, `history`, `mcp`. A unit of
delegated work is blacksmith's; relay is the session substrate. `resume
reap|prune` cover what `gc` did for sessions.

Details: [`docs/2026-07-24-relay-design.md`](docs/2026-07-24-relay-design.md)
(historical; the delegation half is retired).

## Develop

```bash
go test ./...
go build -o bin/relay ./cmd/relay
./install.sh
```

## License

See repository for license terms.
