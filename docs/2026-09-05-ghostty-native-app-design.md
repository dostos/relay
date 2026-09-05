# A Ghostty-based app with relay attached

Date: 2026-09-05
Status: Shape settled. No app code written yet.

Goal, in the user's words: throw a devcontainer-capable repo at the fleet, it
launches, and you attach or detach it from your window whenever you want. The
app is a **new app built on Ghostty**, with relay attached — not a layer of
relay abstractions piled on top of a terminal.

## The whole design

Ghostty already is the terminal: tabs, splits, window management, config, a
Metal renderer. relay already is the fleet: sessions, hosts, containers,
messages. Neither needs to learn the other's job.

One field connects them. Ghostty's `SurfaceConfiguration` carries an explicit
command (`macos/Sources/Ghostty/Surface View/SurfaceView.swift:588`), and a tab
is opened with one (`TerminalController.newTab(_:from:withBaseConfig:)`,
`macos/Sources/Features/Terminal/TerminalController.swift:419`). So opening a
fleet session in a tab is:

```swift
var config = Ghostty.SurfaceConfiguration()
config.command = "relay resume --session \(name) --host \(host)"
_ = TerminalController.newTab(ghostty, from: TerminalController.preferredParent?.window,
                              withBaseConfig: config)
```

The app's only new component is the list that feeds `name` and `host`, read
from `relay session list --json`. That is the entire integration.

## What the app deliberately does not do

It does not implement relay's `Viz` port, or any part of the presenter
contract cmux satisfies — no `ManagedPanes`, no `BindLocalParent`, no
`NotifyParent`, no surface-ref bookkeeping. Those exist so relay can drive a
presenter it owns. This app is not driven by relay; it drives relay, by running
one command per tab.

It also does not speak SSH, tmux, or docker. `relay resume` already performs the
SSH hop, the tmux attach, and the reconnect loop.

An earlier version of this document designed the opposite: the app as a second
`Viz` implementation. That framing is dropped. The seam work it motivated
(commit dee3a62, moving `ManagedPane` into `ports` and adding a presenter
selector) stands on its own as type-location hygiene, but a second presenter is
no longer the plan.

## Attach and detach

Attach is opening a tab. Detach is closing it. The remote process is untouched
either way, because it never lived locally.

Verified live against hamburg with a marker process ticking in the remote
session:

| Step | Result |
|---|---|
| Attach from a local pty | surface shows the live remote output |
| Detach (kill the local surface) | remote session and its process keep running |
| Reattach from a fresh surface | same session, full scrollback, continuity |

**The reattach trap, found and fixed.** With the remote session gone, reattach
did not fail. It silently created a new empty session of the same name and
showed a clean prompt, with nothing to say the previous work was gone — a tab
that looks alive with its agent dead. The cause was `tmux new-session -A`,
which attaches or creates and reports neither. Fixed in relay (959598a): the
create path now writes a notice as the new session's first scrollback line.

Two failed attempts at that notice are worth remembering, because anything
printing around a terminal takeover hits them. Printing before
`exec tmux new-session` is erased by the repaint in the same instant. Pausing so
it can be read is still a warning that disappears, which is the same failure one
moment later.

## Building it

Ghostty builds and runs on this Mac. Verified: `zig-out/Ghostty.app`, version
1.3.2-dev, Zig 0.16.0, ReleaseFast, universal binary.

Getting there corrected a wrong diagnosis worth recording. The current release,
1.3.1, pins Zig 0.15.2 and fails with dozens of undefined libSystem symbols —
which reads as a broken build. It is not: **Zig 0.15.2 cannot link an empty
`pub fn main() void {}` on this machine at all.** The failure is Zig 0.15.2
against the macOS 26.5 SDK, and Ghostty's source never reaches compilation. The
remedies that follow from the misreading — an older Xcode plus a sudo
`xcode-select --switch`, waiting for a Zig release, or adopting Nix — are all
unnecessary, and each leaves a far larger footprint.

| | Result |
|---|---|
| Zig 0.15.2, empty program | fails to link |
| Zig 0.16.0, empty program | fine |
| Ghostty 1.3.1 + Zig 0.16.0 | refused; it requires exactly 0.15.2 |
| Ghostty `main` (1.3.2-dev) | requires Zig 0.16.0 |

Build from the `tip` source tarball with Zig 0.16.0. No sudo, no global
installs, no Nix.

**Read `minimum_zig_version` from the tree you actually have**, never from the
docs page, which describes the current release. That is the rule this cost.

Prerequisites, all already satisfied here: full Xcode active (not just
CommandLineTools), the macOS/iOS SDKs and Metal Toolchain, `gettext` from
Homebrew, and a local Zig that needs no system install. Zig writes a compiler
cache to `~/.cache/zig` regardless of where the build runs.

## Costs, stated plainly

A fork of a fast-moving upstream has to be rebased, and that never stops. The
local build uses the ReleaseLocal configuration, which disables Library
Validation so it runs unsigned — fine for a personal tool, not for
distribution.

Upstream has an AI usage policy (`AI_POLICY.md`) requiring disclosure and full
human understanding for contributions. It governs contributions to Ghostty, not
a private fork, but it is the standard to meet if anything here is ever sent
upstream.

## Open decision

Where the app lives: a new repo registered in `workspace.yaml`, carrying the
Ghostty fork. Name not chosen.
