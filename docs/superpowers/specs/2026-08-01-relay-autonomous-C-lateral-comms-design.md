# relay — autonomous mode, Part C: lateral child-to-child communication

Date: 2026-08-01
Status: Proposed
Series: Part **C** of Autonomous Mode (A → B → C). **Orthogonal** to A/B — it rides the same tree + relayd substrate and is independent of the escalation changes, but is sequenced last. See A for the shared model.

## Problem

Relay is strictly **vertical**: a child talks only to its parent. The sole path from child A to child B is up-through-the-common-ancestor-and-back-down — high latency, and it burdens the manager (or the apex) with relaying *coordination that isn't a decision*. Autonomous subtrees need lateral coordination — status awareness, resource/GPU coordination, artifact handoff, lateral requests — without round-tripping the apex.

## Substrate already exists

`MsgService` (`internal/core/msg.go`) over relayd provides the transport: `Send(host, channel, kind, from, text, meta)`, `Read(..., follow)`, `WaitOne(host, channels, ..., timeout)` (zero-token blocking wait). It is exposed as the raw `relay msg` CLI (`internal/cli/app.go:380`). But it is **ungoverned**: no lineage scoping, no authorization, per-host only, and absent from the `relay agent` protocol. **C governs and exposes this substrate; it builds no new transport.**

## Scope (confirmed with user)

A **layered** model — a low-governance escape hatch plus a governed, structured, queryable layer:

- **Layer 1 — free forest bus.** Flat channel namespace within a forest; any member publishes/reads by name; light naming. Closest to today's raw `relay msg`; for ad-hoc/opportunistic messaging. Intentionally low-governance.
- **Layer 2 — multi-level, categorized, queryable blackboard.** The important new capability: not just streaming, but **queryable current state**, scoped per subtree, organized by category, with hierarchical rollup.

## Decisions

1. **Layer 2 addressing = lineage-path + category.** Channels keyed like `tree.<root>…<node>/<category>` with categories such as `status`, `artifacts`, `needs`, `resource`. A child publishes to **its own** node/category.

2. **Query is latest-per-key over a subtree prefix.** The new verb reads a **materialized snapshot** — latest value per `(node, category, key)` — across a subtree **prefix**, filtered by category (e.g. query `tree.beholder.*/status` for every child's status under beholder). This is a read-time fold over the append log for v0 (bounded by retention); a maintained relayd index is a later optimization if query volume warrants.

3. **Live updates stay zero-token.** `watch` uses the existing `WaitOne` over the relevant channels — no poll loops, consistent with relay's agent protocol discipline.

4. **Visibility is scoped by tree position — this is the governance Layer 1 lacks.** A node may query/publish within its own subtree (and, per an explicit policy knob, its ancestor chain). Default: same-subtree only; a node cannot read a sibling subtree's board. Reconciles lateral comms with the strict-tree security model.

5. **Expose through the `relay agent` JSON protocol — no raw host/channel strings.** New agent verbs (JSON `next`/`argv`): `peers` (discover addressable siblings/subtree nodes by role/name/path), `post <category> <text|kv>`, `query <path-prefix> <category> [--key]`, `watch <path-prefix|category>`. Cross-host routing via the desktop bridge, exactly like handoffs.

6. **Comms never confer authority.** No child gains a management/decision edge via C. Escalation and decisions still flow only through A/B. Layer 1's free bus is forest-boundary trust only — keep secrets off it; document the trade-off.

## Architecture / seam

- Reuse `MsgService` (`msg.go`) verbatim for transport.
- Add a thin **naming + scope + snapshot** layer above it (lineage-path channel naming, prefix query fold, scope check against lineage).
- Add the four `relay agent` verbs in `internal/cli/app.go` alongside the existing `msg` case, routing through the bridge for cross-host like handoffs.

## Error handling / edges

- **Scope violation:** a query/post outside the permitted lineage scope is refused (not silently empty) — explicit error.
- **Retention/GC:** blackboard channels are GC'd on subtree retire (tie to the existing retirement gate).
- **Cross-host unreachable:** surface a clear error; the free bus and blackboard degrade to reachable hosts.

## Testing

- Publish/query/scope: subtree rollup returns correct latest-per-key; a node cannot read outside its scope.
- Cross-host routing via the bridge.
- Zero-token `watch` wakes on update and times out cleanly.
- Free-bus basic pub/sub.
- GC on subtree retire.

## Open questions (resolve in planning)

- Exact ancestor-visibility policy (default same-subtree; is any ancestor read-through wanted?).
- Whether Layer 1 (free bus) is truly needed once Layer 2 exists — kept per explicit user ask, marked as the escape hatch.
- Read-time fold vs maintained relayd index — start with the fold; measure.
