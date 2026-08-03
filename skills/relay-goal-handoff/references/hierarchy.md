# Hierarchy

Relay is a strict management tree, not a role system.

- A handoff has zero or one immediate parent. Zero means an intentional root.
- A manager communicates with immediate children only. Never reach into a
  child's descendants or confiscate its work.
- When an immediate manager is conclusively absent, Relay may connect the
  nearest live ancestor to that manager's child for operational verbs and
  escalation resolution. This is audited failover, not re-parenting: the
  durable lineage edge stays unchanged. Unknown/unreachable is not absent.
- A child records progress and reports asks/results to its immediate parent. A
  progress receipt advances the durable cursor without interrupting the
  manager. It never contacts higher ancestors directly.
- An unresolved decision moves through Relay's durable routing; do not manually
  forward transcripts or recreate the ask at another level.
- A manager decides scope and acceptance, not the child's tactics. Delegate
  procedural observation to another handoff instead of hovering over execution.
- Re-parenting, root adoption/enrollment, restart, and retirement are explicit
  topology changes. Never infer authorization for them from an ordinary goal.

When a child is quiet, distinguish working, blocked, absent, and stalled using
Relay's readiness and event evidence. Silence alone is not permission to send
instructions.
