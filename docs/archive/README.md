# relay — archived documents

Design records for parts of relay that no longer exist. Kept for the history
of *why*, never as a description of the current binary. The current surface is
the top-level README; the decision that retired these is
`dostos-workspace/docs/superpowers/specs/2026-09-13-agent-infra-one-owner-per-axis.md`
and the workspace's living system map is `dostos-workspace/docs/agent-infra.md`.

| document | described | retired |
|---|---|---|
| `2026-07-24-relay-design.md` | the founding design: sessions **and** the handoff/manager control plane | handoff half, 2026-09-13 |
| `2026-08-04-unified-service-refactor-handoff.md` (was `HANDOFF.md`) | the one-binary/one-home-service refactor, with the watcher reconciler and viz roles | watcher + viz, 2026-09-13; the home service itself stays |
| `communication-patterns.md`, `token-efficient-events.md`, `continuous-operations.md` | manager↔child envelopes, escalation, the always-on conductor | 2026-09-13 |
| `unified-service.md`, `unified-service-acceptance.md`, `viz-ssh.md` | the cmux presenter, projection-only Mac, viz broker | 2026-09-13 (Forge is the presenter) |
| `2026-09-05-retire-hierarchy-plan.md`, `2026-09-05-messaging-persistence-surface.md` | the half-retirement that kept messaging | superseded 2026-09-13 |
| `superpowers-specs/2026-07-29-relay-container-handoff-design.md` | container handoffs | 2026-09-13 (devcontainers.md is current) |
| `superpowers-specs/2026-08-01-relay-autonomous-{A,B,C,D}-*.md` | autonomous mode: nearest live ancestor, agent apex, lateral comms, control-plane locality | 2026-09-13 |
| `superpowers-specs/2026-08-08-host-ensure-account-agents-design.md` | relay's own ccs/codex account discovery in `host ensure` | 2026-09-13 (agent-accounts is the source) |
