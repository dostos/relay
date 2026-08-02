# Hierarchy

Relay is a strict management tree, not a role system.

- A handoff has zero or one immediate parent. Zero means an intentional root.
- A manager communicates with immediate children only. Never reach into a
  child's descendants or confiscate its work.
- A child reports progress, asks, and results to its immediate parent. It does
  not contact higher ancestors directly.
- An unresolved decision moves through Relay's durable routing; do not manually
  forward transcripts or recreate the ask at another level.
- A manager decides scope and acceptance, not the child's tactics. Delegate
  procedural observation to another handoff instead of hovering over execution.
- Re-parenting, root adoption/enrollment, restart, and retirement are explicit
  topology changes. Never infer authorization for them from an ordinary goal.

When a child is quiet, distinguish working, blocked, absent, and stalled using
Relay's readiness and event evidence. Silence alone is not permission to send
instructions.
