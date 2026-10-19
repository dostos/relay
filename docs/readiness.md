# Readiness for presenters

A presenter wants one answer per session: is the agent **ready** (working),
**blocked** at a gate only a human may answer (login, folder trust, a
permission prompt — with its literal choices), **absent** (a bare shell), or
**unknown** (relay could not look). The classifier is one piece of code in
relay; two verbs reach it so nobody copies it:

- For a pane the presenter is **attached** to, it already holds the screen
  text: pipe the last ~40 lines to `relay pane classify` and get the JSON
  back. No ssh, no registry — a local function behind a verb, so Forge can
  classify every attached tab as often as it likes at no fleet cost.
- For a **put-away** session, `relay session readiness ID` captures the pane
  tail over the wire (bounded to ~8s) and classifies it. `session list
  --readiness` does that for every session, one host at a time per host and
  at most two hosts at once, never failing the list: an unreachable host
  reports `unknown` with the ssh error as its reason.

Both print the same shape: `state`, `reason`, `gate` (with `choices` when the
prompt is numbered), `lines`, `sampled_at`. A gate is reported, never
answered: `readiness.go`'s rule is that a security prompt is a terminal state
for automation.

```bash
relay pane classify < TEXT                  # readiness of pane text you already hold (no ssh)
relay session readiness ID [--lines N]      # capture + classify one put-away session
relay session list|get --json --readiness [--readiness-lines N]
```
