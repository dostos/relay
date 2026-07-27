---
name: relay-handoff
description: Use when handing a goal to a remote relay session. Prefer relay agent JSON verbs; no event poll loops.
---

# relay agent (follow `next`)

Always use the JSON agent surface. **Never** `events tail -f` in a loop. **Never** `session attach`.

`NAME` must match the host profile. Discover valid names with `relay host discover -H HOST`
(typically `claude` / `cursor-agent` / `codex` / `ccs:<profile>`). It is **`cursor-agent`**,
not `cursor` — though an unambiguous prefix now resolves (`cursor`→`cursor-agent`,
`ccs`→a lone `ccs:personal`), and a bad name errors with the list of available agents.

```bash
relay agent start -H HOST --agent NAME --goal "…"     # or --cmd "…"
# → {"next":"wait","argv":["relay","agent","wait",…]}
relay agent wait --handoff ho-… [--from N] [--timeout 120]
# → next ∈ send|done|escalate|wait  — run argv once, stop
relay agent send --handoff ho-… -- "…"                # agent only; refused on jobs
relay agent capture --handoff ho-…                    # if unsure before send
relay agent done --handoff ho-… --outcome done
relay agent status --handoff ho-…                     # resume after compaction
```

Rules encoded in `next` (do not re-derive):
- Job `idle` → wait (do not send)
- Agent `idle`/`needs_input` → send or escalate
- `exit` → done
- Timeout → wait again on a **new turn** (not a tight loop)

Requires once per host: `relay host bootstrap -H HOST`.
