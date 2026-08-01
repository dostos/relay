# relay — autonomous mode, Part A: nearest-live-ancestor escalation

Date: 2026-08-01
Status: Proposed
Series: Autonomous Mode is three separable specs, sequenced **A → B → C**. **A (this doc)** is the foundational spine — it generalizes escalation delivery. **B** parks an always-on agent at the apex (depends on A). **C** adds lateral child-to-child comms (orthogonal). A carries the shared conceptual model; B and C reference it.

## Problem

Relay's management tree delivers a child's blocking escalation **strictly one edge up** to its immediate parent (`ho.SourceSessionID`). If that parent surface is **disconnected** — laptop asleep, cmux quit, SSH dropped — the escalation is stored durably and **waits** until the *exact* parent reconnects. In a partly-remote, multi-project setup, a closed laptop strands every subtree whose manager lived on it. The child's work stalls not because no one *could* decide, but because the one node it may address is asleep.

Recent commits already circle this pain: `[orchestration] queue one disconnected-parent ask`, `[install] recycle detached parent watchers` — today's answer is "queue and wait." We want "route to whoever is awake."

## The core idea (shared model for the series)

Generalize delivery from "one edge up" to **"the nearest *live* ancestor."** Autonomous mode is not a new subsystem; it is this generalization of the tree you already have, plus (B) an always-on agent at the apex and (C) a lateral comms layer. "Mode" is **structural**, never a global flag.

## Scope (confirmed with user)

- **Generalize delivery to N levels:** walk the lineage to the nearest live ancestor and deliver there; skip disconnected/missing managers.
- **Security invariant (explicit sign-off):** a **live** manager is **never** skipped — only disconnected/missing ones are. You cannot jump over a manager who is present and could handle it; you only route past a dead link.
- **A changes *where* an escalation is delivered, not *who decides what*.** No agent decision logic here (that is B). A live human or agent ancestor still decides via existing mechanisms.
- **No lateral/sibling comms here** (that is C).

## Decisions

1. **Delivery target = nearest live ancestor.** Introduce `resolveDeliveryTarget(ho)` that starts at `ho.SourceSessionID` and walks the lineage chain (`SourceSessionID` recursively — "SourceSessionID and CreatedByHandoffID form the durable relay lineage") returning the first ancestor whose surface is **live**. `deliverMessage` targets that ancestor instead of the immediate parent unconditionally.

2. **Liveness is the delivery outcome, not a separate presence oracle.** `deliverMessage` (`parent.go:636`) *already* fails when a manager is unreachable: `Notifier.NotifyParent` for a local parent, `Sessions.Send` for a remote one, and an explicit `no delivery path for parent %s` otherwise. So the authoritative test of "can this manager receive it" is **attempting delivery**. This is strictly better than a presence oracle: no second source of truth, and no TOCTOU race between "check live" and "deliver".

   - **Primary:** try to deliver; on failure, continue the walk to the next ancestor.
   - **Secondary (optimization only):** a cheap `sessionReachable()` pre-check picks the *likely* target so an escalation is not created in a dead parent's inbox and so we skip an obviously-dead hop. It is advisory — never authoritative.
   - `cleaned`/terminal/retired ancestors are excluded up front.
   - **Bounded attempts (confirmed necessary).** `SessionService.Send` (`session.go:378`) passes the caller's context straight through to the transport with **no internal timeout**, so a delivery attempt to a dead SSH host would hang the walk for as long as the transport takes to give up. Each hop's delivery attempt must therefore be wrapped in its own `context.WithTimeout`; `session.go:574` already establishes a `12*time.Second` precedent for a bounded remote op. The per-hop bound should be well under that so a multi-hop failover stays responsive.

3. **The invariant is enforced in the walk, not by convention.** The walk stops at the first `sessionLive` ancestor. A live intermediate manager is therefore structurally impossible to skip. Document that no new trust edge is created: the *system* routes the envelope; the child never gains the ability to address the grandparent directly. The strict-tree auth model is unchanged.

4. **Reconnect reconciliation — notify, do not re-ask.** When a skipped parent reconnects, it must not be re-asked something already ruled upstream. Dedup by the existing correlation IDs / handoff+seq. It instead receives an **informational** record ("handled upstream by `<ancestor>` while you were disconnected: `<ruling>`"), shown resolved in its inbox — not as a pending ask.

5. **The reply path generalizes too.** Today only the *immediate* parent may reply to a child (`relay parent reply`). Once an escalation is delivered to a farther live ancestor, **that resolving ancestor is authorized to reply to the child**, even though it is not the immediate parent. The reply reaches the child's own handoff/event stream directly, so it is independent of any skipped intermediate's liveness (no need to route the reply back down through a dead link). Reply authorization checks the resolver against the lineage (an ancestor of the child), not against immediate-parent identity.

6. **Grace window before failover.** Treat `disconnected` as skippable only *after* the existing reconnect/backoff window elapses (ties to `relay resume` retry, default 3s), so a transient SSH blip does not prematurely fail an escalation past a parent that is about to return.

7. **Escalation identity and inbox placement must be made target-independent.** This is the subtlest part of A, discovered in the storage layout:
   - The inbox is a **directory per parent session**: `parent-inbox/<parentID>/<messageID>.json` (`parentMessageDir`, `parent.go:464`).
   - `parentMessageID(parentID, handoffID, kind, seq)` (`:459`) **hashes the parent ID into the message identity**.
   - `pendingAttention(parentID, handoffID)` (`:623`) and `applyPolicy`'s seen/pending scan (`:810`) both dedup **within a single parent's directory**.

   Naively retargeting delivery would therefore mint a *different* ID in a *different* directory for the same logical escalation — producing duplicate asks (exactly the "at most one unresolved ask per child handoff" property `DeliverPending` documents at `:672`). Resolution:
   - **Resolve the delivery target *before* allocating the message**, so `ParentSessionID` is the resolved ancestor from the start and storage/ID/dedup stay mutually consistent — no file moves.
   - Make **`pendingAttention` chain-scoped**: before creating an attention envelope, look for an existing pending one for this handoff **anywhere along the ancestor chain**, not just in one directory. Without this, a laptop that sleeps (retarget to apex) and wakes (retarget back) yields two live asks for one question.
   - Make `applyPolicy`'s seen/pending scan chain-scoped for the same reason, so the built-in coalescing rules keep working across a retarget.
   - `FindMessage` (`:548`) already scans **all** parent directories, so reply/ack-by-ID keep working regardless of which inbox holds the message. No change needed there.

## Architecture / seam

`ParentService.RouteChildEvent` (`internal/core/parent.go:739`) already: builds the durable `ParentMessage`, runs the deterministic policy gate `applyPolicy` (line 786), then `deliverMessage(parent, ho, msg)` to the single immediate parent. The change is localized:

- `RouteChildEvent`: replace the direct `GetSession(ho.SourceSessionID)` target with `resolveDeliveryTarget(ho)`.
- New `resolveDeliveryTarget` + `sessionLive` in `parent.go`.
- `deliverMessage`: unchanged signature; receives the resolved ancestor.

The deterministic policy gate (`applyPolicy`) still runs first, unchanged — A is below/around it, only affecting the delivery target when the gate does not resolve.

## Data changes — `ParentMessage`

`ParentSessionID` (`parent.go:39`) keeps its existing meaning — **the session whose inbox holds this message** — which after A is the *resolved* ancestor, so all existing storage, listing, and delivery code keeps working unchanged. Add:

- `IntendedParentSessionID` — the immediate parent that was skipped (empty when delivered directly, so existing records stay valid).
- `SkippedSessionIDs` — the disconnected ancestors passed over, for audit.
- `ResolvedBySessionID` — who actually ruled.

These let the audit and `relay parent inbox` answer "who *should* have handled this, and who actually did." All are additive with zero-value defaults, so no migration is required.

For the reconnect path, a skipped parent must see the question as **already resolved**, never as pending. Prefer reusing the existing states (`ParentMessageReplied`/`Acked`) plus `ResolvedBySessionID` for attribution, rather than adding a state constant — fewer states, and `ListMessages(pendingOnly)` then naturally excludes it.

## Error handling / edges

- **Whole chain down (no live ancestor):** fall back to today's behavior — enqueue durably at the topmost ancestor, retry on reconnect. (With B's always-on apex, this case disappears for governed subtrees.)
- **Parent sleeps mid-delivery:** liveness is checked at delivery; a stale "live" that fails delivery falls through to the next ancestor. Delivery is idempotent per correlation ID.
- **Terminal/retired ancestor:** not live → skipped.
- **Broken/cyclic lineage:** bound the walk by max depth + visited-set; on anomaly deliver to last-known-good and log.

## Testing

- Lineage fixtures with mixed live/dead ancestors → delivery lands on nearest live; a live intermediate is never skipped (the invariant, as a test).
- Reconnect: skipped parent returns → sees resolved-upstream notification, not a pending ask; no double-handle.
- No-live-ancestor → durable enqueue + retry.
- Idempotency: duplicate correlation IDs collapse.
- Depth bound / cycle guard.
- Grace window: a parent that reconnects within the backoff window is *not* skipped.

## Open questions / deferred

- **Resolved during planning:** the reply path needs no CLI change. `authorizeParentCaller` (`internal/cli/app.go:2169`) compares the caller against `msg.ParentSessionID`, and because the resolved ancestor *is* `ParentSessionID`, it authorizes naturally. Decision 5's generalization is therefore a no-op — and `authorizeParentCaller` must **not** be widened, since that would weaken the strict-tree guarantee.
- **Deferred — grace window (decision 6).** The bounded per-hop delivery attempt already prevents a hang, and `DeliverPending` already retries on rebind. A separate grace timer adds a second retry path for a narrow case (a blip landing exactly during an escalation). Ship without it; add if real use shows premature failover.
- **Deferred — chain-scoping `applyPolicy`'s seen/pending scan (decision 7).** It degrades *safely* across a retarget: the built-in coalescing rules may fail to collapse a duplicate idle sample, but they can never auto-approve something they otherwise wouldn't. The duplicate-*ask* case that actually matters is covered by chain-scoped `pendingAttention`.
