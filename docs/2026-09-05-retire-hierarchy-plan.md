# relay — retiring ownership, keeping messaging

Date: 2026-09-05

> **Superseded in part (2026-09-13):** the "keep messaging" half is replaced by
> `dostos-workspace/docs/superpowers/specs/2026-09-13-agent-infra-one-owner-per-axis.md`.
> The constraint this plan cited for keeping it (hermes holding a headless
> root) had ended on 2026-08-26; the delegation handshake retires as one
> unit once its remaining consumer (agent-command-center) is migrated.
Status: **Done** — branch A (flat inbox). All four passes landed 2026-09-05
(fa9b02b, 4ed068a, d25a08c, 42926b9, 693cdfb): ~4100 lines removed, 19 test
packages green. No open questions: board.go was settled last, scoped to one
mailbox.

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

## Pass 4: apex — landed in 693cdfb

The instruction that settled it: *"apex 같은 관리자는 이제 필요 없음. 그냥 binary
자체가 관리자."* There is no governing session; the binary is the manager. That
retired manager replacement too, which this plan had provisionally kept as
control-plane authority. With no manager session there is nothing to replace.

| Site | What |
|---|---|
| `root.go` | `Apex`, `Adopt`, `Release`, `Enroll`, `Unenroll`, `Governed`, `Digest` |
| `cli/app.go` | `relay root adopt/release/enroll/unenroll/status/rules/digest` |
| `parent.go` (3), `supervisor.go`, `authority.go` | `ApexLabel` reads |
| `session.go` | `UnobservableGovernedChildren` |
| `ancestor.go` | `AncestorChain` — its last caller is the apex code above |

Nothing live depended on it: `relay root status` reported `no apex designated`
before removal, so this was not a production change.

Two things that sound like the retired concept were kept, and the commit says
why: `relay root control-plane` and `relay root rules` govern nothing, and
`authority_lock.go` is a state-file write lock that merely shares the name.

Two doctor checks were deleted rather than rewritten. `governed_event_channels`
keyed off a label only enrollment ever set, so it had already become a check
that could never fire. Rewriting it would have meant guessing a new predicate:
"a launched session that should have an event stream" is not the same set as
"a session with a launch edge", because interactive sessions have the latter
and no handoff, and would have been flagged wrongly.

## Two dependencies this plan missed, found by a survey of the kept surface

Both verified in code, neither addressed yet.

**`board.go` built a tree — settled: one mailbox.** Reading it closely, only
half of it was tree-shaped. `resolveBoard` already resolved to exactly one
mailbox: a session's board is the board of the mailbox that launched it, keyed
on the same launch edge flat delivery uses, so a session's peers are the
siblings sharing its mailbox. That needed no change beyond wording.

The tree was `QuerySubtree`, which assembled a parent-to-children map and rolled
up nested managers, plus the `--subtree` flag that reached it. Both are gone,
along with the depth guard that bounded the walk. `relay board query` returns
one mailbox's board, which is what post and watch already did.

The isolation property survives and is now the whole point: siblings share one
board, and there is no way to name another mailbox's.

**`applyAgentChildWorkspaceTrust` is a second permission decision**, living in
the file this plan calls the messaging engine (parent.go:1519-1551). It
auto-approves a child agent's folder-trust gate when the launch edge and the
workspace both match. It is deliberately **kept**: it grants rather than
refuses, and it keys off the launch edge (`SourceSessionID`), which is exactly
what flat delivery is built on, not off the ancestor tree. Its one real tie to
the retiring machinery is an `ApexLabel` read, which goes in pass 4.

## A note on what the retirement cost

Flattening `deliveryCandidates` made `len(candidates) == 1` permanently true,
which silently killed the second delivery attempt after a transient transport
failure -- one arm of an early return that no longer meant what it said. No
test failed, because that property had only ever been covered through the
failover tree and its test retired with the tree. Fixed in 42926b9 with a test
that fails on the exact regression.

The lesson generalizes to pass 4: when a guard is written in terms of a
structure being removed, deleting the structure can quietly change what the
guard does. Re-read every condition that mentions candidates, ancestors,
children or labels before assuming it is inert.
