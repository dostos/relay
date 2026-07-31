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
relay history                                          # who handed off to whom
```

Inside a pane opened with `relay HOST NAME`, the command is forwarded through
that pane's owner-only Unix-socket bridge to the desktop relay. The returned
binding includes `source_session_id`, `source_host_id`, and
`source_persist_name`; preserve them rather than synthesizing lineage. The
desktop uses that source binding to split the first child right of its parent
and stack later siblings downward in the same workspace.

Rules encoded in `next` (do not re-derive):
- Job `idle` → wait (do not send)
- Agent `idle`/`needs_input` → send or escalate
- `exit` → done
- Timeout → wait again on a **new turn** (not a tight loop)

Requires once per host: `relay host bootstrap -H HOST` (installs both the
remote `relay` client and `relayd`).
