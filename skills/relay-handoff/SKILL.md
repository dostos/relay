---
name: relay-handoff
description: Use when handing a goal to a remote relay session (agent CLI or job), watching events with a seq cursor, coordinating inject/escalate, and finalizing — cmux viz optional.
---

# relay Handoff

Mechanism: `relay` CLI. Policy: this skill (when to inject / escalate).

Requires remote `~/.config/relay/host.yaml` with the agent listed, and always-on **relayd** on the host:

```bash
relay host bootstrap -H HOST   # once per host; unix socket only, one quiet SSH upload
relay host probe -H HOST
```

Events go through `relayd` (not `tail -F`). Do not hammer reconnects — the CLI backs off (max 6/10min/host) for campus IPS safety.

## 1. Launch + arm monitor (same turn)

```bash
cd <git-repo>
relay handoff -H HOST --agent claude --goal "…"
# JSON binding: handoff_id, session_id, watch, pane
```

Job mode:

```bash
relay handoff -H HOST --cmd "make train" --no-pane
```

Immediately arm:

```bash
relay events tail -f --handoff ho-… --from 0
```

Set `persistent: true` on the monitor for long work.

If `pane:false` and the human should see it: `relay viz present <session_id>`.

## 2. Coordinate

| Event | agent mode | job mode |
|-------|------------|----------|
| idle / needs_input | actionable | informational — do not inject |
| exit | agent quit | job finished — actionable |

On actionable event:

1. `relay session capture <session_id> -n 120`
2. In-scope → `relay session send <session_id> -- "…"` then re-arm monitor
3. Ambiguous / high-stakes → escalate to human; do not guess

`idle` re-fires while silent — act at most once per silence streak.

## 3. Resume after compaction / SSH drop

```bash
relay events tail --handoff ho-… --from 0    # note last seq N (omit -f)
relay events tail -f --handoff ho-… --from N
```

Retire the old monitor before arming a new one.

## 4. Finalize

```bash
relay session capture <session_id> -n 200
relay handoff finalize ho-… [--outcome done|failed|abandoned] [--keep-session]
```

Still-live agent sessions need `--outcome` (chat agents often never emit natural `exit`).

`finalize` does **not** close the cmux surface — run `relay viz close <session_id>` if desired.

Orphans: `relay handoff reconcile`.

## Never

- `session attach` / interactive SSH from an agent
- Inject into a healthy **job** on idle
- Autonomously answer outward-facing / high-stakes prompts
- Close viz surfaces without an explicit session id
