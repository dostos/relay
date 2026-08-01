# relay — autonomous mode, Part B: agent apex ("root mode"), rules & audit

Date: 2026-08-01
Status: Implemented (apex lifecycle + conductor role). Phase 3 ergonomics landed early as `relay root adopt|enroll|unenroll|status|rules|digest`; the role ships at `share/roles/relay-conductor.md`. Not yet exercised against a real always-on host.
Series: Part **B** of Autonomous Mode (A → B → C). **Depends on Part A** (nearest-live-ancestor escalation): B assumes an escalation reliably reaches the nearest live ancestor. See A for the shared model.

## Problem

Give a subtree an always-on autonomous decision-maker: a top-tier model (the user named **Fable**) parked at the apex on the always-on **home** host, ruling project-level escalations against **human-authored per-subtree rules**, so work proceeds while the human is away. The human becomes the **single point of communication** — they converse with the apex, which filters the whole forest for them.

Because Part A already routes escalation to the nearest *live* ancestor, an always-on apex is automatically the terminal decision-maker for every governed subtree: when the human's laptop is asleep, escalation walks past it and lands on the apex. "Laptop disconnected → the root agent decides" falls out of A + an always-live root — no special failover path.

## Scope (confirmed with user)

- **Authority ceiling: govern existing roots.** Answer project-level decisions surfaced by roots. Do **not** spawn/retire roots, reprioritize across projects, or allocate the fleet.
- **Boundary: human-authored per-subtree rules; fail-closed.** Unmatched → escalate to the human. No fixed hard-stop list (rules are the sole boundary); safety comes from fail-closed + rules that name irreversible actions explicitly. Break-glass override considered and **declined** (YAGNI).
- **Engagement: structural always-apex.** A subtree is governed **iff** it has an agent-root ancestor. When present, the root filters regardless of connection; disconnect just means the unmatched residue waits longer. Not a global flag — topology decides.
- **Runtime: a persistent, conversational Fable pane, always-on on home.** The laptop is a detachable viewport.
- **Topology: mixed.** The apex governs whichever roots are alive and parented under it (laptop-local or durable-remote).

## Decisions

1. **The apex is a normal always-on agent session — no new entity type.** It is the `claude` CLI at the Fable model, in relay's existing autonomous-permission mode, running on `home` in durable tmux, resume-registered so it survives cmux quit / reboot / sleep. Being always-live makes it Part A's terminal ancestor for every governed subtree.

2. **Relay stays model-free; the apex agent is the decision engine.** Relay does not call Fable. The apex's governance is the **semantic sibling** of relay's deterministic `applyPolicy` policy gate — one tier up in the *same* escalation pipeline:
   1. **Policy gate** (relay core, deterministic, zero-token) — redundancy / stable-literal auto-answers. *Exists.*
   2. **Apex agent** (Fable, semantic, per-subtree rules) — project-level rulings. *This spec.*
   3. **Human** (attached to the apex) — the unmatched residue only.

3. **The apex is a portable role, not relay code.** A role prompt under `.agents/roles/` (per the workspace's portable-roles rule; vendor dirs are thin adapters) drives the loop:
   1. Zero-token wait on its escalation inbox (`relay parent inbox` / event wait) — **never poll**.
   2. Per escalation: load the originating subtree's rules; judge in-envelope vs out.
   3. In-envelope → `relay parent reply` (resolve down), logging ruling + rationale.
   4. Out-of-envelope / low-confidence → **hold** for the human (leave in the human-queue).
   5. Delegate any procedural sub-work (watch an eval, tail a job) to cheap operators per the workspace's mandatory eval-ops delegation rule. The apex does judgment; it must not become a polling loop on a frontier model.

4. **Rules are the delegation envelope: human-authored, semantic, per subtree, versioned in the workspace.** The workspace is the source of truth for project structure, so rules live there — proposed `.agents/rules/<project>.md` (prose, Fable-judged), resolved by the apex from the escalating subtree's project identity. Examples: "auto-approve eval-flywheel steps", "never push to main without me", "borrowed GPUs: yield on owner return without asking". Fail-closed: not-clearly-permitted → escalate.

5. **Audit is non-negotiable.** Every autonomous ruling is a durable, attributable record (escalation, matched rule, decision, rationale, timestamp, subtree), built on existing `AppendCommunication`/ledger + `relay parent inbox --all` (auto-decisions are already auditable). On human reattach, a **"while you were away" digest**: counts by subtree, notable rulings, pending residue.

6. **The human interface is conversation with the apex.** "Single point of communication" is literal: the human attaches to the apex pane (`relay home root` / `relay resume`) and talks to Fable in natural language; Fable executes the `relay parent reply`s. Detach is free; the apex keeps ruling; residue waits.

## Lifecycle / staging

- **Prereq:** `home` is a relay host (`relay host init -H home`); Fable reachable via the `claude` CLI there.
- **Phase 0 (v0, zero relay code):** start the apex with existing `relay agent start home claude --name root -- "<role>"`; parent governed roots under it via `relay parent link/move`; rules as files; audit via raw `relay parent inbox --all`. Prove end-to-end on one project, on top of Part A.
- **Phase 3 (ergonomics, thin verbs):** promote proven conventions into `relay root` verbs — `root up -H home` (ensure apex), `root enroll <root>` (parent + mark governed in one step), `root rules <project>`, `root log --since` (the digest). Relay stays model-free throughout.

## Testing

- Role-level eval harness: in-envelope vs out-of-envelope escalations → correct reply/hold; fail-closed on ambiguity.
- Audit completeness: every ruling produces a durable, attributable record; digest counts are correct on reattach.
- Model-free invariant guard: no model call is introduced into relay core.
- Structural engagement: a subtree with no agent-root ancestor is unaffected (human decides directly).

## Open questions (resolve in planning)

- Naming: `relay root` vs `relay conductor` vs `relay apex`. ("root mode" language favors `root`.)
- Rules format: markdown prose (Fable-judged, v0) vs structured yaml (readable by both the apex and a future deterministic pre-filter). Start prose; revisit.
- Per-project hands-on opt-out is already expressible structurally (do not enroll that project) — document as the mechanism rather than a flag.
