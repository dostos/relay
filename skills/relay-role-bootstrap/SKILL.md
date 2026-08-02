---
name: relay-role-bootstrap
description: Boot, adopt, resume, or verify long-lived goal-driven Relay sessions according to their management role while remaining agent-runtime agnostic. Use for apex/conductor startup, governed project roots, intermediate managers, goal workers, and low-cost procedural operators; for repairing an inert role session; or when a user asks Relay to choose an agent and establish verified lineage, watchers, and optional visualization.
---

# Relay Role Bootstrap

Create the smallest valid Relay topology for the requested role. Use Relay's
JSON protocol and host profiles; never inspect or automate a vendor-specific
agent UI.

## Select the role

- Apex or conductor: read [references/apex.md](references/apex.md).
- Project root or intermediate manager: read
  [references/manager.md](references/manager.md).
- Goal worker: read [references/goal-worker.md](references/goal-worker.md).
- Procedural monitor, evaluator, or log collector: read
  [references/operator.md](references/operator.md).

If the requested role is unclear and the distinction changes authority or
lineage, ask one short question before creating anything.

## Bootstrap workflow

1. Run `relay agent protocol` once. Treat its JSON as the runtime contract.
2. Inspect before creating: `relay session list`, `relay handoff list`, and the
   role-specific status command. Reuse a healthy matching session; do not create
   duplicates based only on a name.
3. Resolve the host from user intent and host profiles. If no agent type was
   requested, run `relay agent pick HOST` and use its advisory selection only
   when it reports usable capacity. Never encode runtime names in this skill.
4. Build a concrete goal containing scope, authority, verification, reporting,
   and terminal conditions. Keep role behavior in the role prompt; keep the
   goal specific to this handoff.
5. Execute the role card's Relay command. A managed start has no conversational
   follow-up: execute `argv` only when Relay returns it.
6. Stop immediately if Relay classifies a trust, login, security, or permission
   gate. Surface the exact gate to the human; never send text or Enter to it.
7. Verify effects, not launch calls:
   - session and handoff records exist;
   - the observed agent state is ready, not blocked or absent;
   - parent lineage and role labels match the requested topology;
   - the supervisor reports every live handoff watched;
   - optional Viz returns an acknowledgment with the expected parent anchor;
   - doctor reports no bridge build drift.
8. Return one compact receipt: `role`, `session_id`, `handoff_id` when present,
   `parent_session_id`, `host`, `agent`, `readiness`, `watcher`, `viz`, and
   `next`. Do not repeat the full goal.

## Authority rules

- Address only the immediate parent or children established by the topology.
- Never adopt an apex, enroll a root, re-parent, restart, retire, or broaden
  scope unless the user's request explicitly authorizes that exact change.
- Never turn missing policy into permission. Hold ambiguous governance changes.
- Prefer `relay agent restart HANDOFF` for a failed goal and a verified reuse
  for a healthy role; do not silently replace identities.
- Use a blocking Relay wait for event-driven work. Never poll in an agent loop.
