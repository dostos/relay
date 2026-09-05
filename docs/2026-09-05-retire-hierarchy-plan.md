# relay — retiring ownership, keeping messaging

Date: 2026-09-05
Status: Accepted — branch A (flat inbox), 2026-09-05. In progress.

Goal, as stated: (1) retire the permission/ownership machinery, (2) refocus on
messaging + persistence (tmux) + viz, (3) aim at a Ghostty-based native app that
launches a devcontainer-capable repo on the fleet and attaches/detaches at will.

## What the code actually contains

The three things are not three files. Measured, not guessed:

| Cluster | Where | Lines | Verdict |
|---|---|---|---|
| Messaging + delivery | most of `parent.go`, `msg.go`, `board.go`, `eventstream.go`, `policy.go` | ~3.0k | **keep** — this is (2) |
| Ownership + hierarchy | `authority_policy.go`, `ancestor.go`, the apex half of `root.go`, and the link/adopt/reparent/retire half of `parent.go` | ~1.6k | **retire** — this is (1) |
| Bridge identity + history | `lineage.go` | 0.6k | **keep** — see correction below |
| Control-plane authority | `authority.go`, `vizbroker/authorize.go`, registry projection | ~0.6k | **keep** — infrastructure |

Three findings that change the shape of the work:

**1. `parent.go` is mostly the messaging engine, not a permission system.**
`RouteChildEvent`, `deliverMessage`, `DeliverPending`, `ListMessages`, `Reply`,
`Watch`, escalation and redelivery all live there. Retiring the file would
delete (2) in the name of (1).

**2. "Authority" is two unrelated concepts sharing a word.** Control-plane
authority is *which client is the source of truth* — the reason this Mac is
authoritative and `host_id: local` matters. `vizbroker/authorize.go` authorizes
a launchd **viz service binary** by fingerprint. Neither is a user permission.
Only `authority_policy.go` (757 lines, "whether a valid bridge identity may
exercise ordinary hierarchy authority") is the permission boundary, and it
exists only to gate the hierarchy verbs — so it retires *with* them.

**Correction (2026-09-05, after reading the code):** `lineage.go` was initially
filed under ownership by its name. It is not. It holds bridge *identity*
(`AuthorizeBridgeSource` compares a token — authentication, not authorization)
plus the session history and communication log. It stays. The permission
boundary is `authority_policy.go` alone: `AuthorizeBridgeRequest` decides
"whether a valid bridge identity may exercise ordinary hierarchy authority",
so it retires with the hierarchy it guards.

**3. Messaging currently addresses through the tree.** Delivery escalates to the
nearest live ancestor. Remove the hierarchy and messages have no addressee.
This is the actual blocker, and it is why (1) cannot simply precede (2).

## Live constraint

hermes holds relay's root **headlessly** — `headless.go` exists so a
container-hosted coordinator can be a root at all, with the durable inbox as its
channel. Retiring roots wholesale removes hermes's inbox and breaks the live
Slack agent. Any retirement must leave a delivery target standing.

## The fork — resolved: A

Both branches retire the same core. They differ in what replaces tree-based
addressing. **A was chosen.**

**A. Flat inbox. (chosen)** Keep `parent register --headless` and the durable inbox as a
plain named delivery target; drop link/adopt/reparent/retire/lineage and the
bridge authority gate. Smallest change, hermes untouched, no migration. Keeps a
vestigial "parent" noun for what is really just a mailbox.

**B. Channels.** Migrate delivery onto the existing `relay msg` channel bus
(named channels, per-channel cursors) and delete the parent noun entirely.
Cleaner end state and a better fit for (3) — a Ghostty app wants a session list
and a message bus, not an org chart. Costs a real migration and touches hermes
in production.

## Sequencing (either branch)

1. Retire the hierarchy verbs and their gate; keep every messaging verb.
2. Cut `Destroy`'s child refusal and the retirement gate down to the tmux/viz
   teardown that actually has to happen.
3. Consolidate what remains around three nouns: **session** (tmux, attach,
   detach), **message**, **surface** (viz).
4. Only then (3): the Ghostty app speaks to that reduced surface. `relay
   container open` is already the launch primitive; what the app adds is
   attach/detach against a session list, replacing cmux as the presenter.

## Not in scope here

Removing tmux (it *is* the detach substrate), the devcontainer work, or
control-plane authority.
