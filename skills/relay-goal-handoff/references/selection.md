# Execution and CLI selection

Choose from evidence, in this order.

## Job or agent

- Use a job for a deterministic command with a known completion condition:
  builds, tests, migrations, benchmarks, exports, and bounded log collection.
- Use an agent when the goal requires diagnosis, implementation choices,
  synthesis, iteration, or judgment.
- Do not spend an agent on waiting. Use `relay agent wait` or the event stream.

## Host

Select a host that has the required repository/data locality, compute,
credentials, and runtime capability. Prefer an already-authorized target. Do
not initiate login, trust, or permission setup as part of selection.

## Agent CLI

1. Honor an explicit user choice.
2. Otherwise run `relay agent pick HOST` and inspect remaining usage and host
   capability from Relay's profile/probes.
3. Match task shape to capability: coding and tool execution for implementation;
   long-context reasoning for synthesis; low-cost capacity for procedural work.
4. Among capable choices, prefer healthy available capacity with greater
   remaining usage. Preserve scarce/high-cost capacity when a cheaper CLI is
   sufficient.
5. Treat the pick as advisory. If every capable choice is exhausted, blocked,
   or unauthenticated, report that constraint rather than silently selecting an
   unsuitable CLI.

Host policy may define named launch profiles with model arguments. Profiles
that draw from the same provider quota share `usage_key`; `preferred_agent`
selects the normal profile and `exhausted_agent` selects the provider-managed
fallback when that quota drops below `usage_min_remaining`. For example, a
host may prefer a fast Grok profile and fall back to an Auto profile. Keep the
actual model identifiers and flags in `host.yaml`, never in the delegated goal.

Never encode vendor names, UI strings, login flows, or model-specific flags in
the goal. Relay's host profile owns those details.
