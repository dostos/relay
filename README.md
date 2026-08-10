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

Hand a goal to Claude / Codex / cursor-agent on a lab box. Close cmux. Reopen later — Vault runs `relay resume --session …` and the pane is back in the same tmux session.

```bash
relay agent start host-a cursor-agent -- "fix the flaky eval; keep tests green"
# → {"next":"wait","argv":["relay","agent","wait",…]}
relay agent wait ho-… [--timeout 120]

# Resume a saved terminal goal without copying its prompt or lineage:
relay agent restart ho-…
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

# anywhere in the relay control plane
relay history
# c3/research (human)
# └─[relay]→ c1/followup (human)

# an agent handoff records the agent and ho-… edge as well
relay agent start c1 codex --name analysis -- "continue the analysis"
```

Child placement follows the recorded session binding, not whichever pane is
currently focused. The first child splits `right` from its parent; later
children of that parent split `down` from the newest live sibling. Explicit
`--workspace` / `--pane` placement overrides the default. Inspect the
session-keyed records with `relay pane list`.

Local agent panes are also first-class sessions. Starting a handoff from cmux
auto-registers the caller as its parent; an explicit migration is:

```bash
relay parent register --name personal-db-main --repo ./projects/infrastructure/workspace-search
relay parent bind sess-… --surface surface:…  # cmux restarted the root pane
relay parent link sess-… ho-…        # adopt existing work
relay parent move sess-… ho-…        # explicitly repair a wrong parent edge
relay resolve pm-… -- approve         # the only decision handshake
relay log 0                            # optional compact delta; save returned cursor
```

### Headless roots

A manager does not have to be a pane. A coordinator that runs as a long-lived
service — a container, a daemon — registers as a **headless root**:

```bash
relay parent register --headless --name Apex --ttl 15m   # idempotent; re-run on every start
relay parent heartbeat                                   # renew declared liveness
relay parent inbox                                       # also renews it
```

`inbox`, `log`, `status`, `sweep` and `heartbeat` name no manager because there
is only one they could mean: the authenticated caller. A manager gets its
identity from the command boundary, not from an id it was told to memorise, so
these read and prove *itself*. Naming another manager still works when it is
inside your own subtree, and is still refused — explicitly — when it is not.

Two things differ from a pane parent, and only two:

* **Delivery is the durable inbox.** There is no composer to type into, so an
  envelope is delivered the moment it is durably written, recorded as
  `headless_inbox_confirmed`. `--wake inject|notify` need a surface, so they
  degrade to the inbox and the registration says which mode was asked for.
* **Liveness is declared, not observed.** Relay classifies a pane by looking at
  it; it cannot look at a service. So a headless root is live only while its
  heartbeat is inside the declared TTL. Past that it is treated exactly like an
  absent pane: delivery reports the target unavailable, attention envelopes fail
  over to an ancestor, and an envelope with nowhere to go stays **pending and
  visible** rather than being marked delivered into a service nobody is running.
  `relay parent list` reports every headless root's heartbeat state alongside it.

### Managers under managers

A headless manager does not have to be a root. `--under` registers it as the
child of another manager, which is the "channel agent" shape: it supervises its
own children — status, capture, direct, restart — while governance (holds,
approvals, the last escalation stop) stays with the manager above it.

```bash
relay parent register --headless --name chan-gazer --under sess-apex…   # a parent, not a root
relay parent adopt sess-chan-gazer… sess-project…                       # give a session a manager
relay parent list --under sess-chan-gazer…                              # its own subtree
```

`relay parent link|move` name a **handoff**, because a handoff is normally what
creates a child. A session adopted from an already-running tmux
(`relay session adopt`) has no handoff, so those verbs had nothing to name and
such a session could never leave the top of the tree. `relay parent adopt`
addresses the **session** instead, and refuses rather than guesses:

* moving a session that already has a manager requires `--from CURRENT`, and
  the name must be right;
* `--from` on a session with no manager is refused, because the caller believes
  something about the tree that is not true;
* a session whose lineage is owned by a **live handoff** is not adoptable here
  at all — moving the session record alone would leave the handoff's event
  stream escalating to the old manager. The refusal names `relay parent move`.

Authority is unchanged in shape: a caller may create a manager only inside its
own subtree (`--under` is mandatory for an authenticated non-human caller —
omitting it asks for a new *root*, which is the one thing lineage confinement
exists to prevent), may adopt only into its own subtree, and may take a child
only from its own subtree.

Three details are load-bearing rather than incidental:

* Registration is authorized on `--under` but acts on `--name`, and a name is a
  guessable durable identity. So registration **never** changes lineage on
  convergence — not even from "no manager" to "some manager". The only session
  it can converge onto is one already reporting to the named manager, and so
  already inside the caller's subtree. Re-homing is `parent adopt`, always.
* `--under`, `--name` and `--from` must appear exactly once with a non-blank
  value, refused identically by the policy and by the command. The policy reads
  the first occurrence and an ordinary parse loop keeps the last, so a repeated
  flag is otherwise authorized against one value and executed with another.
* An **unmanaged** session is not claimable by an ordinary manager. `parent
  link` allows that for an unowned handoff, but most sessions in a registry have
  no manager, so the same rule for sessions is a reach across the whole fleet —
  and it does not stop at reading, because a session in your lineage can be
  `exec`'d. Which session belongs to which manager is a declared fact applied by
  the writer that owns the registry; over the bridge, only the governing apex
  may claim one.

The holder process usually lives somewhere other than the authority host. It
operates the root through the ordinary authenticated command boundary, using an
identity issued at registration (`--print-identity`) and confined by the same
lineage policy as any other manager. Where that boundary listens is
`RELAY_BRIDGE_SOCK`, honoured by both the server and the client so an authority
can publish it somewhere a co-located holder can actually reach; a host that
owns the authority but has no cmux and no reason to open ssh tunnels serves it
with `relay service boundary run --no-control`.

This is a durable control plane for **long-lived, goal-based handoff and
orchestration**, not a transcript or chat bus. Correlated control envelopes
survive agent exits, SSH reconnects, nested relays, and cmux restarts until the
goal is resolved and terminal.

The lineage is a strict management tree. A child can address only its
authenticated immediate parent. Remote parents are agent managers: they
resolve or escalate one level. Only a local cmux root receives human-facing
notifications, so descendants cannot bypass their manager and interrupt the
human directly.

Escalation is delivered to the nearest **live** ancestor. A manager that
cannot receive the envelope — laptop asleep, cmux quit, SSH dropped — is
passed over so the child never stalls on a sleeping manager. A live manager
is never skipped, so this adds resilience without weakening the tree: the
envelope still travels the lineage, one unresolved ask per handoff is
preserved across the failover, and the skipped manager is recorded on the
message (`intended_parent_session_id`) alongside who actually ruled
(`resolved_by_session_id`).

### Autonomous mode

Park an always-on agent at the top of the tree and enrolled roots keep working
while you are away. Because escalation goes to the nearest *live* ancestor, a
sleeping laptop is simply skipped and the question lands on the apex instead of
stalling:

```bash
relay agent start home claude -- "$(cat share/roles/relay-conductor.md)"
relay root adopt sess-…            # designate that session as the apex
relay root enroll sess-beholder    # this root's subtree is now governed
relay root rules beholder          # where its human-authored rules live
relay root digest                  # what needed you vs. what it ruled
```

The apex rules against **human-authored per-project rules** and holds anything
they do not clearly permit — silence in the rules is a "no". Mode is
structural, not a flag: a subtree is governed exactly when it has an agent-root
ancestor, so `relay root unenroll` is the entire off switch. The apex must
itself be a root, so you remain the last escalation stop.

Relay stays model-free throughout. It owns enrollment, rule resolution, and the
audit; the judgment lives in the portable role at
[`share/roles/relay-conductor.md`](share/roles/relay-conductor.md).

For goal-driven delegation, `relay agent protocol` is the authoritative native
contract. The repository's vendor-neutral
[`relay-goal-handoff`](skills/relay-goal-handoff/SKILL.md) is an optional human
and manager reference for goal shaping and CLI selection; installation does
not inject it into any agent runtime, and Relay correctness never depends on
the skill being loaded.

**Governance runs where the control plane runs.** The registry, the parent
inboxes, and the watcher processes all live on the machine that started the
work — so if that machine sleeps, the router sleeps with it and an escalation
raised meanwhile is not routed until it wakes. `relay root enroll` and `relay
root status` report this rather than let an enroll imply autonomy the
deployment cannot deliver. For genuinely unattended operation, start governed
work from a session on an always-on host and declare that durable machine
policy once with `relay root control-plane --always-on`. Use `--sleepable` if
the machine's availability changes. Relay deliberately does not infer this
policy from a running process or socket. See
[`docs/superpowers/specs/2026-08-01-relay-autonomous-D-control-plane-locality.md`](docs/superpowers/specs/2026-08-01-relay-autonomous-D-control-plane-locality.md).

### Home service and watcher health

The always-on home authority independently supervises the event coordinator,
authenticated command boundary/forwarders, and watcher reconciler in one
process. A component failure restarts only that component, but aggregate health
stays red until all components are live, ready, on the same build, and able to
make durable effects.

```bash
relay service run          # service-manager entrypoint
relay service status       # per-component health receipt
relay doctor               # also flags old split authority processes
```

Use `share/systemd/relay.service` on the authoritative host. The old
`relayd.service`, `relay-control.service`, and `relay-supervisor.service` are
migration inputs, not peers: only one authority may own the canonical sockets
and state. See [`docs/unified-service.md`](docs/unified-service.md).

Escalation is vertical, but peers often need to coordinate without asking
anyone to decide anything. The children of one manager share a **board** — a
categorized surface for status, resources, and artifacts:

```bash
relay board post -c status -k phase -- "scoring, 40% done"
relay board query -c status          # peers' current state, compact JSON
relay board query -c status --subtree # a manager's whole subtree, one call
relay board watch -c status          # zero-token wait for the next update
```

A board holds *state*, not conversation: re-posting a key supersedes it, and a
query folds to the latest value per node and key, so an agent pays for current
state rather than history. Scope needs no permission check — a board id is
derived from the caller's own lineage, so a node cannot name another subtree's
board, and identity comes from the authenticated bridge envelope rather than an
argument.

`relay session adopt` also provisions an owner-only bridge identity so an
already-running agent can discover Relay from its tmux pane without receiving
a secret in its prompt. Repair a session adopted by an older Relay release in
place with `relay session bridge sess-…`; no agent or tmux restart is needed.

Each supported delegated CLI starts in its autonomous permission mode
(`cursor-agent --force`, Codex approval bypass, or Claude permission bypass),
so routine tool calls do not consume parent turns. This does not bypass login,
folder-trust, onboarding, or confirmation gates: launch readiness classifies
those panes, emits `permission_required`, and sends neither the goal nor Enter.
Each child also has one detached blocking event watcher. Agent hooks publish
`permission_required`,
`result`, and `exit`; the tmux-idle sensor supplies the `ask` fallback for
agents without an input hook. Relay deduplicates by handoff/sequence, collapses
repeated idle samples into one unresolved attention envelope per child, stores
it durably while the parent is disconnected, and wakes the exact parent pane
once after delivery succeeds. Rebinding a parent retries its undelivered inbox;
it never replays the sensor samples.
No transcript is forwarded. Relay adds one compact instruction to the child
goal: when blocked on manager input, declare the question with `relay ask
"<question>"`. This emits explicit event text; tmux scraping remains only the
fallback for agents that ignore it. Set `relay_hooks: off` only when an agent
runtime cannot execute hooks.

Managers that need durable context use `relay log N` and persist the returned
`next` cursor. The authenticated session supplies the parent identity. The log
records only meaningful request, resolution, result, and policy transitions
with a bounded summary; it never stores or replays a conversation transcript or
idle sensor samples.

Informational `note`, `progress`, `result`, and `exit` events acknowledge
themselves after verified delivery to a ready manager. Only unresolved input
reaches a manager, and it takes one
`relay resolve` call to continue the child; there is no receipt acknowledgement
round trip.

A handoff launched inside the hierarchy returns `managed: true` and no
`next`/`argv`: Relay's detached watcher already owns the wait. This prevents a
parent agent from starting a second blocking wait against the same child.

The desktop policy gate removes redundant hook/fallback pings automatically
and can answer stable CLI prompts with explicit literal-guarded rules. It
normalizes bounded hook fields (`agent`, `host`, `text`, and `command`) so a
provider can change its raw hook payload without changing the policy file.
Unknown provider prompts and genuine goal decisions still continue one level
up to the immediate manager; optional literal-guarded policy rules can handle
stable fallback prompts.

```bash
relay policy list
relay policy check --kind ask --agent cursor-agent \
  --text "Run this command?" --command "git status"
relay policy add cursor-read --kind ask --agent cursor-agent \
  --contains "Run this command?" --contains "git status" --reply y
relay policy remove cursor-read
```

All `--contains` literals must match, case-insensitively. Policies are
desktop-local in `~/.config/relay/policy.yaml`; automatic decisions remain
auditable in the communication log. Built-ins coalesce repeated
idle samples while one ask/permission decision is pending, a tmux-idle
fallback after an outstanding permission event, and an `exit` shortly after a
`result`; they never grant permission themselves.

Closing a local parent is deliberately gated:

```bash
relay parent complete sess-…
relay parent status sess-…                # dry-run reasons
relay parent retire sess-…                # closes only when eligible
```

Retirement requires every child terminal, no unresolved input, all
scoped Git roots clean with no commits ahead of upstream, and an explicit
`idle` or `complete` parent state. `session destroy` cannot bypass this gate.

### 3) Bring a new machine online

Discover SSH aliases, probe agent CLIs, propose `host.yaml`, and bootstrap the
host-local event role from the primary binary:

```bash
relay targets --json
relay host discover -H host-a --json
relay host init -H host-a --apply   # installs relay + compatibility symlink
```

### 4) Orchestrator loop (no poll loops)

`relay agent` is the self-describing agent protocol. Run `relay agent protocol`
for its compact rules, then follow JSON `next` / `argv` only — never
`events tail -f` in a tight loop and never attach an agent to a session.

Managed Codex and Claude launches receive Relay as an invocation-local MCP
stdio tool and receive their initial goal through the provider's native prompt
argument. Cursor reads its user MCP inventory; register Relay without replacing
other servers, then approve that one server explicitly:

```bash
relay mcp install cursor
cursor-agent mcp enable relay
```

The MCP adapter exposes one `relay` tool whose `argv` is sent through the same
authenticated home boundary as the CLI. It contains no parallel policy or
runtime-specific orchestration instructions.

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
relay agent start HOST claude -- "…"
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
| **relay CLI** | Session/handoff IDs, `viz present`, branding, agent JSON API |
| **control bridge** | Unix-socket daemon on the control host; serializes authenticated remote requests |
| **cmux client** | Optional visualization endpoint; executes cmux operations but owns no agent lifecycle |
| **tmux** (remote) | Durable process surface |
| **relay service event** (remote) | Always-on event bus over a **Unix socket only** (no TCP listen) |

Host profiles (`~/.config/relay/host.yaml` on each remote) list agent CLIs and `path_map`. Connection coords stay in your SSH config — relay only uses Host aliases.

During the control-plane migration, the desktop bridge is started on demand by `relay resume`. Its socket is
`~/.local/state/relay/desktop-bridge.sock` (0600). Each attached pane uses SSH
stream-local reverse forwarding to expose a per-session socket under `/tmp` on
the remote host. Requests carry a per-session token, and the bridge allowlist
is limited to named-session and handoff operations. There is no TCP listener or
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
relay session … / session adopt             # durable tmux
relay agent start|wait|send|capture|done    # orchestrator API
relay parent register|inbox|reply|ack       # durable parent communication
relay parent register --headless --name N   # a root that is a service, not a pane
relay parent register --headless --under P  # a managing parent under P, not a second root
relay parent adopt PARENT SESSION [--from]  # give a handoff-less session a manager
relay parent list --under PARENT            # one manager's own subtree
relay parent heartbeat PARENT               # renew a headless root's liveness
relay board post|query|watch                # manager-scoped peer coordination
relay root adopt|enroll|status|digest       # always-on apex (autonomous mode)
relay service run|status                    # unified authority and component health
relay parent status|retire                  # guarded local-pane cleanup
relay history                               # source → destination lineage
relay pane list                             # owned surface/workspace/pane + parent + liveness
relay pane rename SESSION_ID NAME           # durable display alias; leaves tmux identity intact
relay session rename SESSION_ID NAME        # true tmux/checkpoint rename; keeps session id + lineage
relay viz present|brand|save|restore        # cmux surface
relay resume --session NAME                 # Vault target
```

Details: [`docs/2026-07-24-relay-design.md`](docs/2026-07-24-relay-design.md).

## Develop

```bash
go test ./...
go build -o bin/relay ./cmd/relay
./install.sh
```

## License

See repository for license terms.
