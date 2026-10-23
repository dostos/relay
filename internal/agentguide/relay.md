# relay: message other agent sessions (tmux panes) safely

`relay` types a message into another agent's terminal session (Claude Code,
Codex, cursor-agent, … running in tmux on any fleet host), proves whether it
was submitted, and lets you wait for the reply. Always use `--json` and read
`ok`, `receipt.state` and the exit code. Never paste into panes with raw
`tmux send-keys`.

## Address a session

- `HOST/NAME`: tmux session NAME on HOST (e.g. `c1/reviewer`). Works for any
  tmux session, registered or not.
- `NAME`: when exactly one session has that name. Ambiguous → exit 1,
  `reason: ambiguous_session`, `candidates: [HOST/NAME…]`; pick one.
- `sess-…`: registry id. List with `relay --json session list`.

## Send, then wait for the reply

```sh
relay --json session readiness c1/reviewer            # state + activity: idle|busy|blocked
relay --json session send c1/reviewer --idempotency-key review-42 -- "Please review PR #42 and reply with LGTM or issues."
printf '%s\n' "$LONG_MESSAGE" | relay --json session send c1/reviewer --idempotency-key review-43 --stdin
relay --json session wait c1/reviewer --idle --timeout 100s --tail 60   # loop on exit 124
relay --json session capture c1/reviewer -n 80        # read more of the answer
```

## Receipt states (`receipt.state`) and exit codes

| state | exit | meaning | do |
|-------|------|---------|----|
| `submitted` | 0 | a new turn started for your message | wait for the reply |
| `queued` | 0 | target was busy; message accepted, runs after its current turn | wait |
| `not_sent` | 1 | nothing typed (see `receipt.evidence`) | fix cause, retry (same key is fine) |
| `deposited` | 67 | text is in the target's input, no turn seen | inspect; `relay session submit ID` only if you decide to |
| `uncertain` | 67 | may or may not have been delivered | inspect with `session capture`; NEVER resend blindly |

`not_sent` evidence:
- `composer_not_empty`: the target's input already holds someone's draft
  (`receipt.leftover`). Do not clear it. Wait / ask the human; append only
  with `--force-append` if the draft is yours.
- `target_at_gate`: the target waits at an approval/login prompt. Only a
  human answers it; relay never presses Enter there.
- `send_lock_busy`: another sender is mid-send; retry shortly.

Always pass `--idempotency-key KEY` (unique per logical message): a retry
with the same key never types twice and replays the first receipt
(`"replayed": true`).

## `session wait --idle` exit codes

0 idle (reply finished; `result.tail` with `--tail N`) · 65 blocked at a gate
· 66 session gone · 124 timeout (still busy; re-run the `argv` in the output).
Use `--timeout` below your own tool timeout (e.g. 100s) and loop.

## When things fail

- Every failure prints JSON with `error`, and usually `reason`, `next` and a
  ready-to-run `argv`. Follow `argv` instead of guessing.
- `desktop relay bridge unavailable`: messaging still works from this host;
  address targets as `HOST/NAME`.
- Other commands: `relay <command> --help` shows flags and examples;
  `relay help exit-codes` lists every exit code.
