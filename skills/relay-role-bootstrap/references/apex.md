# Apex

Use the portable role at `share/roles/relay-conductor.md`. Do not copy or alter
it. The apex performs judgment; Relay remains model-free.

## Required inputs

- Always-on control host.
- Agent selection, explicit or from `relay agent pick HOST`.
- Durable session name.
- Human-authored rules directory used by the conductor role.

## Sequence

1. Run `relay root status`. If a ready apex already exists, return it.
2. Confirm the selected host is the authoritative always-on control host.
3. Start the role as a normal goal-driven agent session with no parent. Supply
   the complete conductor role plus the concrete `rules_dir` using
   `relay agent start HOST AGENT --name NAME -- GOAL`. The role must read its
   own authenticated Relay session identity after startup; do not predict an
   ID before Relay creates it.
4. Read back the created session and classify its pane. Do not adopt a blocked
   or absent agent.
5. When the user explicitly requested creating/replacing the apex, run
   `relay root adopt SESSION` and verify `relay root status` names the same
   session and reports the agent ready.
6. Enroll project roots only when separately and explicitly requested. Confirm
   the control-plane description is always-on before claiming autonomy.

Never bypass prompts for an apex. With no manager above it, any gate belongs to
the human.
