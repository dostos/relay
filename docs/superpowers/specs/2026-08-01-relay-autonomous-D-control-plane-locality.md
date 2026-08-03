# relay — autonomous mode, Part D: control-plane locality

Date: 2026-08-01
Status: **Direction chosen 2026-08-02: home owns control; Mac is viz-only.** Migration is incremental so there is never more than one authoritative writer.

## The gap in one sentence

Autonomous mode cannot run autonomously while the laptop is away, because the thing that *routes* escalations — and the durable state it routes with — lives on the laptop.

## Evidence

Simulated a sleep by freezing the watcher process (`SIGSTOP`), with a real tree on a real host (`home-relay`): child → manager → always-on apex, manager killed mid-flight, a real `permission_required` emitted through `relayd`.

| Phase | Envelopes created | Apex notified |
|---|---|---|
| Laptop asleep (watcher `SIGSTOP`) | **0** | **no** |
| Laptop awake (`SIGCONT`) | 1, failed over to apex correctly | yes |

The failover logic is right. The *trigger* is on the wrong machine.

## Why

Two facts compose badly:

1. **The router is local.** `startParentWatcher` (`internal/cli/app.go:2403`) spawns `os.Executable()` — the local binary — as a detached **local** process, once per handoff, on whatever machine ran `relay agent start`.
2. **The state is local.** `ParentService.Watch` routes through `p.Reg`, which reads `StateRoot()` (`internal/core/paths.go:15`) — `~/.local/state/relay` on that same machine. The registry, the parent inboxes, and the communication ledger all live there.

So when the laptop sleeps, nothing subscribes to the child's event stream, no envelope is created, and the apex — always-on and perfectly able to decide — is never told there is anything to decide.

## Why this matters more than it looks

The value proposition inverts. The promise was "work continues while I am away." The actual behaviour is **work stalls while I am away, and is then auto-decided the moment I return** — precisely when I am present and could have decided myself.

Parts A/B/C are not wrong, and they are not wasted: the routing, the invariant, the apex lifecycle, and the boards are all correct and proven on real hardware. But they deliver autonomy only for a tree whose control plane already lives somewhere always-on.

## What currently *does* work

Autonomy holds today when the control plane is not on the laptop — e.g. the operator runs `relay agent start` **from a session on `home`**, so both the watcher and `~/.local/state/relay` are on the always-on host, and the laptop is purely a viewport. This is the topology the README's "laptop is a detachable viewport" language already implies; it was simply never the *default*, and nothing enforces or even reports it.

## Directions (not yet chosen)

1. **Move the watcher to the work host.** Events originate at the child's `relayd`; a watcher there needs no SSH to observe them. But delivery and the durable inbox still need the control-plane state, so this alone is insufficient.
2. **Move the control plane to the apex host.** Registry, inboxes, and watchers live on `home`; the laptop becomes a client of it. Most faithful to "the apex is the root of the forest," and makes `relay root` genuinely meaningful — but it is a substantial change to a deliberately desktop-centric design (`docs/2026-07-24-relay-design.md`: the desktop bridge owns cmux, remotes never call cmux directly).
3. **Apex-side supervisor.** The apex host runs a process that adopts watchers for governed handoffs when the originating desktop is unreachable. Smaller change, but introduces two writers to one durable store — the exact hazard that produced the replay/overwrite bug already fixed in Part A.
4. **Report the limitation honestly and require the topology.** Make `relay root enroll` refuse, or loudly warn, when a governed subtree's watcher would live on a non-always-on host. Cheapest, and prevents a false sense of autonomy.

Option 4 is worth doing regardless of which of 1–3 is chosen, because today nothing tells the operator their "autonomous" subtree is only autonomous while the laptop is open.

## Status: option 4 implemented

`DescribeControlPlane` (`internal/core/root.go`) reports the host governance actually runs on, and `relay root enroll` / `relay root status` surface it. Relay never claims always-on on its own — it cannot detect whether a machine sleeps. A human declares the durable host policy with `relay root control-plane --always-on` (or reverses it with `--sleepable`); the host-identity-bound declaration lives in `~/.config/relay/control-plane.json`, so interactive shells, relayd, the supervisor, and restarts agree without rewriting `host.yaml`. The environment variable remains a compatibility override.

This does not deliver away-from-desk autonomy. It removes the false impression of it, which was the more dangerous of the two problems.

Options 1–3 remain open. The control plane currently stays on the Mac by choice, while `home` is being redesigned; revisit once that settles.

## Decision

`home-relay` owns the durable registry, authorization, inboxes, watchers,
bridge, and visualization request queue. The Mac is a replaceable visualization
client. It connects outbound to home, then executes target SSH and cmux locally
when awake. Home requires no SSH access to the Mac. Coordination must continue
when visualization is unavailable.

The first migration increment separates presentation from control locality.
Home sends a typed `{session_id, target, tmux_name}` request to the optional Mac
`relayd viz` queue. It sends no shell command and no geometry. The Mac launchd
follower consumes requests through outbound strict SSH, resolves target SSH
coordinates, and applies the user's local placement policy (default `rome`). A
typed `update_relayd` signal uses owner-fixed repo/remote/branch policy and
refuses dirty, wrong-branch, or non-fast-forward updates. The relayd binary
stages and build-verifies both executables, swaps them with rollback copies,
and exits for launchd restart; runtime updates do not execute `install.sh`.
State/socket cutover remains a
separate verified step; until it completes, the existing desktop bridge stays
authoritative.
