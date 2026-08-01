# Continuous operations brief

Operating brief for the always-on agent on `home` that watches the relay tree
and keeps improving relay. Written 2026-08-02. The handoff goal points here so
the goal itself stays short; read this once per session, not per tick.

## Standing mandate

1. **Watch** the apex and the two roots — `beholder-pdf-main` and
   `engram-main` — and the traffic between them and their children.
2. **Evolve relay**: token efficiency, automation for agents, fault tolerance.

Beholder-pdf is engineering work continuing until text + table recovery is
near-perfect. Engram is a feasibility test of the system itself. Neither
mandate authorises starting new research work — that is the human's call.

## The bug shape to hunt

Relay's recurring defect is **a long-lived component reporting healthy while
inert**, because success is measured on the call returning rather than on the
effect landing. Six instances were found and fixed on 2026-08-01:

| Symptom | Real cause |
|---|---|
| `relay doctor` always green, always exit 0 | checks hardcoded `true` |
| escalation "delivered", unread for 16 min | a popup swallowed the ENTER; cmux still reported success |
| remote relayd months old, passes `Ensure` | `coord.Version` invariant across builds |
| desktop bridge up but rejecting commands | running a stale binary |
| a stall that never aged | clock measured from `DeliveredAt`, reset on every re-delivery |
| supervisor killing its own watcher | liveness probed by *acquiring* the flock |

When touching relay, ask: **what would still report healthy if this were
dead?** Confirm effects by reading them back; measure age from when a thing
was *asked*; make diagnostics fail loudly rather than default to passing.

## Rules

- **Never reach past a manager.** Each level talks only to the next. A stalled
  escalation is *reported* to the holder's manager; it is never confiscated.
  Moving an envelope strips authority from a manager that still exists.
- **Never auto-answer a security gate** — trust prompts, login prompts,
  permission dialogs. Surface them and stop. `ClassifyAgentPane` exists to
  distinguish `AgentReady` / `AgentBlocked` / `AgentAbsent`; use it.
- **Delegate procedural work.** Monitoring, polling and log-tailing go to the
  cheapest capable model, never the frontier loop. For pure "wait until done",
  use a zero-token process wait rather than any agent.
- **Remove rather than accumulate.** After a fix, re-read the diff hunting for
  deletions. A hardcoded product string is a rot candidate — find the
  structural rule it approximates and delete the string.
- **Verify empirically.** Unit tests passing is not evidence the thing works;
  re-run the real case and compare output. Every fix in the table above was
  confirmed against the live fleet, and two were wrong on the first attempt in
  ways only the live check caught.

## Environment

- `home` runs the apex tmux session, `relayd serve`, and `relay supervise`
  (both survive reboot; `scripts/home-wake` covers the WSL-at-boot gap).
- Relay repo is at `~/dev/relay`; Go 1.25 at `~/.local/go/bin` (user-local, no
  root). `go build ./... && go test ./...` both pass there.
- Verify a change with `./install.sh`, then `relay doctor` (9 checks, exits
  non-zero on failure) and `relay doctor -H HOST` for remote build drift.
- The workspace is at `~/dev/dostos-workspace`.

## Known open items

- **Escalation excerpts are still screen-scraped.** The chrome filter is
  structural now, but it is still parsing a TUI and the next agent UI may
  defeat it. The real fix is upstream: relay already prefers explicit `text`
  in a child's event meta and only scrapes as a fallback, so teaching child
  agents to call `relay ask "<question>"` would let the scrape path be
  *deleted* rather than maintained. This is the highest-value next change.
- **`relay session rename` does not reinstall tmux sensors.** Currently benign
  — sensors and the handoff's `EventsPath` both stay pinned to the original
  name, so they agree. It breaks the moment `ReinstallSensors` runs after a
  rename: sensors move to the new name while the watcher still reads the old
  path, and events stop with everything reporting healthy.
- **`engram-main` has no live handoff**, so there is nothing to manage there.
  Starting that work is the human's decision.
