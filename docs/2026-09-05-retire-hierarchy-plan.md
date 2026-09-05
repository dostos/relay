# relay — retiring ownership, keeping messaging

Date: 2026-09-05
Status: Accepted — branch A (flat inbox). Three of four passes landed
2026-09-05 (fa9b02b, 4ed068a, d25a08c): -2761 lines, 19 test packages green.
One pass remains, scoped at the bottom.

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


## Landed (2026-09-05)

| Pass | Commit | What went |
|---|---|---|
| 1 | fa9b02b | `authority_policy.go`, its bridge wiring, 15 dead no-op gate call sites, `parent list --under` |
| 2 | 4ed068a | ancestor walks in delivery: `deliveryCandidates`, `pendingAttention`, `createParentMessage`, manager verification |
| 3 | d25a08c | verbs `parent link/adopt/move/reparent/status/retire` + their methods, `promoteMessage`, `validateManagerEdge`, both `Destroy` ownership refusals |

Two behaviours were re-expressed rather than dropped, because the property was
worth more than the mechanism that carried it:

- **Stale asks still surface.** The old walk ended at the human in practice, so
  `ReportStaleEscalations` now names that endpoint directly and tells the local
  human surface. Telling the holder about its own stall was the alternative and
  is worse: it pokes a possibly-remote mailbox about something it already knows,
  and on a fleet that send is not deliverable at all.
- **A manager still addresses itself by writing nothing.** That was enforced by
  the authority policy; it is argument parsing (`ParentVerbTarget`), and moved.

## Remaining — pass 4: apex

Not started. It is separable and self-contained, but it reaches into three
files this plan has not yet audited, so it is deliberately a fresh unit of work:

| Site | What |
|---|---|
| `root.go` | `Apex`, `Adopt`, `Release`, `Enroll`, `Unenroll`, `Governed`, `Digest` |
| `cli/app.go` | `relay root adopt/release/enroll/unenroll/status/rules/digest` |
| `parent.go` (3), `supervisor.go`, `authority.go` | `ApexLabel` reads |
| `session.go` | `UnobservableGovernedChildren` |
| `ancestor.go` | `AncestorChain` — its last caller is the apex code above |

Nothing live depends on it: `relay root status` reports `no apex designated` on
this control plane, so the retirement is not a production change.
