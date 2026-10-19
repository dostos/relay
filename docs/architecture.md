# How relay fits together

| Layer | Role |
|-------|------|
| **Forge** (or any terminal) | Tabs, splits, workspaces, restore; each tab runs `relay resume` |
| **relay CLI** | Session ids and `--json` output for other programs |
| **control bridge** | Unix-socket daemon on the control host; serializes authenticated remote requests |
| **tmux** (remote) | Durable process surface |
| **relay service event** (remote) | Always-on event bus over a **Unix socket only** (no TCP listen) |

Host profiles (`~/.config/relay/host.yaml` on each remote; see
[`share/host.yaml.example`](../share/host.yaml.example)) list agent CLIs and
`path_map`. Connection coords stay in your SSH config — relay only uses Host
aliases.

## Control bridge

During the control-plane migration, the desktop bridge is started on demand by
`relay resume`. Its socket is `~/.local/state/relay/desktop-bridge.sock`
(0600). Each attached pane uses SSH stream-local reverse forwarding to expose a
per-session socket under `/tmp` on the remote host. Requests carry a
per-session token, and the bridge allowlist is limited to named-session
operations. There is no TCP listener or inbound connection to the laptop; the
forward lives and reconnects with the pane's dedicated SSH connection.

`install.sh` is only for initial binary/service bootstrap. Both outbound
control and target attachment are batch-only with strict host-key checking. If
the Mac is asleep, requests wait durably while control work continues on home.

Remote-to-remote commands require a session created by this bridge-aware relay
version. The shorthand can still adopt an older tmux session for attachment,
but it warns that the legacy shell has no bridge identity; choose a new `NAME`
to enable chaining.

## Resume behaviour

`relay resume` keeps the tab alive across sleep and "Shared connection …
closed": on drop it shows a single animated status line (spinner + countdown)
and retries after a short delay (default 3s), frozen to that session. Disable
reconnect with `--no-reconnect` or `RELAY_AUTO_RECONNECT=0`.

| Resume presence | Meaning | Resume? |
|-----------------|---------|---------|
| `live` | tracked locally | yes |
| `disconnected` | SSH dropped; remote may still be up | yes |
| `cleaned` | intentional destroy/finalize | **no** |
| `unknown` | never created here | no |
