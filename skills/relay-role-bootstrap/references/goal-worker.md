# Goal worker

A worker executes one bounded goal and reports only to its immediate manager.

## Sequence

1. Require a concrete parent session, host, writable scope, verification suite,
   reporting cadence, and terminal conditions.
2. Select an agent from the user's choice or `relay agent pick HOST`. Selection
   is capability/capacity based; do not add runtime-specific instructions.
3. Start with `relay agent start HOST AGENT --parent PARENT [options] -- GOAL`.
4. Execute returned `argv` only when present. Do not send a second prompt after
   a managed start.
5. Verify `relay agent status HANDOFF`, the session's parent identity, watcher
   coverage, and the Viz acknowledgment when visualization is enabled.
6. On a declared ask, leave the decision with its manager. On terminal output,
   use the returned `next`/`argv` contract rather than guessing cleanup.

Do not weaken repository, benchmark, data, network, or publication boundaries
while translating a goal into the handoff.
