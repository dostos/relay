# relay

**Durable remote sessions** — SSH + tmux on a fleet host, one authoritative home
service, and a small stateless CLI other programs can drive with `--json`.

relay creates, names, and re-attaches long-lived remote work. It has no window
of its own: **[Forge](../forge)** (a Ghostty fork, `projects/infrastructure/forge`)
is the presenter, and a Forge tab is nothing more than a terminal running

```bash
relay resume --session NAME --host HOST
```

so any terminal can be the presenter, and relay never depends on one.

## Why

Remote agent work needs:

1. **Persistence** — the process must survive laptop sleep, wifi blips, and closing the window
2. **Reattach** — a tab should come back as the same session, not a fresh shell
3. **Program-friendly control** — Forge, the command centre and scripts need `session list|create|send|capture|exec|destroy` with `--json`

relay is the thin substrate for that. What a human looks at is the terminal's
job (Forge); what runs as delegated, machine-judged work is blacksmith's.

## Use cases

### 1) Remote coding agent that survives closing the window

Start Claude / Codex / cursor-agent in a durable session on a lab box. Close the
tab, quit Forge, sleep the laptop. Reopen later — Forge restores the tab by
running `relay resume --session …` and you are back in the same tmux session.

```bash
relay session create -H host-a --name eval -- cursor-agent "fix the flaky eval; keep tests green"
relay resume --session eval --host host-a      # from any pane, later
```

### 2) A workspace with several durable remotes

Adopt or create the sessions; Forge shows them as one workspace (train + eval,
app + benchmark), each tab attached with `relay resume`:

```bash
relay session adopt -H host-a --name train
relay session adopt -H host-b --name eval
relay --json session list                          # what Forge reads
```

For the common interactive path, use the host/name shorthand. It creates (or
reuses) the named remote tmux session and attaches to it in **this** terminal:

```bash
relay c3 research
```

That session carries a persistent reverse Unix-socket bridge to the control
host. A `relay` command run inside it is executed by the control plane, so it
can create the next session without giving a remote machine the control host's
credentials; the reply names the `relay resume …` that attaches it:

```bash
# inside c3/research
relay c1 followup
```

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
./install.sh          # ~/.local/bin/relay{,d}
relay doctor --json
```

Forge finds `relay` at `~/.local/bin/relay` (or `$FORGE_RELAY`).

## Quick start

```bash
# one-time per remote
relay host init -H HOST --apply

# run work
relay HOST NAME                     # current pane → named remote tmux
relay session create -H HOST --name work -- claude   # or any CLI as the pane's command
# or attach an existing tmux session
relay session adopt -H HOST --name my-tmux

# in a Forge tab, or any terminal (also after laptop sleep / wifi drop)
relay resume list                 # live | disconnected | cleaned
relay resume --session NAME --host HOST   # attach; waits/retries on SSH drop
```

`relay resume` keeps the tab alive across sleep and “Shared connection … closed”: on drop it shows a single animated status line (spinner + countdown) and retries after a short delay (default 3s), frozen to that session. Disable reconnect with `--no-reconnect` or `RELAY_AUTO_RECONNECT=0`.


| Resume presence | Meaning | Resume? |
|-----------------|---------|---------|
| `live` | tracked locally | yes |
| `disconnected` | SSH dropped; remote may still be up | yes |
| `cleaned` | intentional destroy/finalize | **no** |
| `unknown` | never created here | no |

## How it fits together

| Layer | Role |
|-------|------|
| **Forge** (or any terminal) | Tabs, splits, workspaces, restore; each tab runs `relay resume` |
| **relay CLI** | Session ids and `--json` output for other programs |
| **control bridge** | Unix-socket daemon on the control host; serializes authenticated remote requests |
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

cmux is no longer the presenter, and relay has no presenter port: the viz
client, `relay viz serve`, `viz-broker`, `install-cmux-restore` and the
projection-only Mac split were retired on 2026-09-13 with Forge in their place.

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
relay HOST NAME                             # named tmux, attached in this terminal
relay session … / session adopt             # durable tmux (create takes -- ARGV for the pane's command)
relay container up|down|status|stop|start   # devcontainer or image-backed instance
relay auth status|login|copy                # agent CLI logins on a host
relay service run|status                    # unified home service and component health
relay resume --session NAME --host HOST     # what a Forge tab runs
```

Retired 2026-09-13 (workspace decision *one owner per axis*): the delegation
handshake — `agent`, `handoff`, `parent`, `resolve`, `ask`, `signal`, `board`,
`policy`, `msg`, `events`, `log`, `root`, `gc`, `history`, `mcp` — a unit of
delegated work is blacksmith's; and the presenter — `viz`/`pane`, `viz serve`,
`viz-broker`, `install-cmux-restore`, `container open` — Forge is the one.
relay is the session substrate. `resume reap|prune` cover what `gc` did.

Details: [`docs/archive/2026-07-24-relay-design.md`](docs/2026-07-24-relay-design.md)
(historical; the delegation half is retired).

## Develop

```bash
go test ./...
go build -o bin/relay ./cmd/relay
./install.sh
```

## License

See repository for license terms.
