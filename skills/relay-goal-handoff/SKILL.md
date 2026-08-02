---
name: relay-goal-handoff
description: Delegate, boot, resume, or repair a goal-driven Relay session using only the goal and its immediate hierarchy edge. Use when handing off implementation, research, evaluation, monitoring, or commands; when choosing between a job and an agent; when selecting an agent CLI from task fit and remaining usage; or when verifying a delegated goal without micromanaging it.
---

# Relay Goal Handoff

Delegate an outcome, not a role or a transcript. Let Relay authenticate the
parent edge and let the child choose its working method inside the goal's
boundaries.

Read these references as needed:

- Choose job versus agent and select a CLI: [references/selection.md](references/selection.md).
- Write a bounded, non-micromanaging goal: [references/goal-contract.md](references/goal-contract.md).
- Preserve the management tree: [references/hierarchy.md](references/hierarchy.md).

## Workflow

1. Run `relay agent protocol` once and follow its JSON `next`/`argv` contract.
2. Infer the immediate parent from the authenticated current session. Ask for a
   parent only when no authenticated edge exists and the choice changes where
   decisions go. Never ask the user to name a role.
3. Run `relay resume list --probe` and `relay handoff list`. Treat only
   `remote_alive: true` as confirmed active. `host_reachable: false` means
   unknown, never dead. Do not inspect `sessions.json`, resume registry files,
   or unprobed `relay session list` to decide liveness. Reuse a confirmed-live
   goal session or restart its handoff when appropriate; do not duplicate work
   based only on a display name.
4. Classify the task and choose the execution form using `selection.md`.
5. Convert the request into the goal contract from `goal-contract.md`. Preserve
   user constraints verbatim, but avoid prescribing ordinary implementation
   steps the child can decide.
6. Start exactly one handoff:
   - deterministic work: `relay agent start HOST --cmd CMD [options]`;
   - goal work: `relay agent start HOST AGENT [options] -- GOAL`.
   Include `--parent PARENT` whenever this is not an intentional root.
7. Execute returned `argv` only when present. A managed start needs no second
   prompt or follow-up message.
8. Verify effects: session and handoff exist, parent lineage is exact, the agent
   is ready rather than blocked/absent, its watcher is active, optional Viz is
   acknowledged, and doctor reports no bridge drift.
9. Yield ownership. Wait for declared progress, ask, or result events. Do not
   poll, repeatedly capture, rewrite the plan, or send unsolicited tactical
   instructions. Intervene only for a child request, a violated boundary, a
   verified stall, or new user intent.

The immediate manager owns the conclusion: accept, redirect, restart, or ask
upward. A child supplies evidence and recommendations but does not make its
manager's acceptance decision or report past that manager.

Stop on every trust, login, security, or permission gate. Surface it to the
human without sending text or Enter.

Return a compact receipt: `goal`, `execution` (`job` or `agent`), `host`,
`agent` when used, `session_id`, `handoff_id`, `parent_session_id`, readiness,
watcher, Viz acknowledgment, and the next event to wait for. Do not repeat the
full goal.
