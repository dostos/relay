# Procedural operator

Use an operator for monitoring, waiting, log collection, benchmark execution,
or other procedural work that does not require independent judgment.

## Sequence

1. Prefer a job handoff for a deterministic command:
   `relay agent start HOST --cmd CMD --parent PARENT [options]`.
2. Use an agent handoff only when the procedure requires interpretation. Pick
   the cheapest capable agent reported by the host rather than naming a vendor.
3. State the exact observation, timeout, output schema, and stop condition.
4. Use one blocking `relay agent wait` or an event stream. Never spend agent
   turns polling unchanged state.
5. Return evidence to the immediate manager without making the decision the
   evidence informs.

An operator must escalate a security gate and stop. It never converts a
procedural assignment into authority over the project.
