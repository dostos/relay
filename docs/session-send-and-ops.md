# Session send receipts and durable operations

Two `relay session` surfaces are built for callers that must not guess:
`session send` (typing into an agent pane) and `session exec --detach`
(long-running commands that have to survive a dropped SSH connection).

## `session send`: success means a fresh turn

```console
relay --json session send ID [--idempotency-key KEY] -- TEXT
relay --json session submit ID [--idempotency-key KEY]   # Enter only, same check
```

Delivery is one shared path (`internal/persist/tmux` `Persist.Send`); every
caller that types into an agent pane goes through it.

0. Take the **per-session send lock** on the target host
   (`~/.local/state/relay/sends/<session>.lock`, atomic `mkdir`). Concurrent
   senders, from any machine, wait their turn (up to 60 s), so their texts and
   Enters never merge into one turn (#66). A lock older than 2 minutes is
   abandoned and is taken over. Timeout: `not_sent`, evidence
   `send_lock_busy`, exit 1.
   Then **refuse when the target sits at a gate**: if `session readiness`
   would say `blocked` (Codex `Would you like to run the following command?`,
   Claude `Do you want to proceed?`, login/trust prompts), nothing is typed:
   `not_sent`, evidence `target_at_gate`, exit 1. Enter on an approval menu
   would select its highlighted "Yes" (#67).
1. Resolve the pane id and **capture the pane before typing** (the
   baseline). If this fails nothing has been typed: `not_sent`, exit 1.
2. Deliver the text as **one bracketed paste** (`tmux load-buffer` +
   `paste-buffer -p -r`), wait 300 ms, then press **Enter exactly once**.
   Typed keystrokes (`send-keys -l`) look like a paste burst to Codex, which
   turns an Enter arriving within 120 ms into a newline, so the message never
   submitted (#65). Newlines, quotes, `$`, backticks and UTF-8 arrive verbatim.
3. Poll the pane (8 reads, 600 ms apart) for evidence of a turn that began
   *after the baseline*:
   - `fresh_working_indicator`: a working/`esc to interrupt` line that was not
     on the baseline screen;
   - `working_timer_reset`: the baseline already showed a timed working line
     and a new one restarted below it. A timer that keeps ticking is the
     *old* turn; the text was only queued;
   - `composer_released`: a recognizable composer (`›`, `>`, `❯`) no longer
     holds the text, and the text appears more often than on the baseline.
     The composer is the last prompt-glyph line *and its indented
     continuation lines*, so text appended to a multiline draft still counts
     as held (#64). The text is matched by its first non-blank line (32 runes),
     ignoring whitespace, so TUI wrapping does not hide it.

   A `Working` line that was already in scrollback can never satisfy the
   check by itself (#59).
4. Without that evidence the send ends as `deposited` (the text is visibly
   still in the input) or `uncertain` (nothing provable), exits
   **67** (`ExitDeliveryUncertain`), and returns `*DeliveryUncertainError`.
   Enter is **not** repeated: once output may be delayed, a second Enter can
   submit a different composer state or approve a gate.

Receipt (`--json`):

```json
{"ok": true, "receipt": {"state": "submitted", "evidence": "fresh_working_indicator",
  "submits": 1, "polls": 2, "session_id": "sess-…", "at": "…"}, "error": ""}
```

| state | meaning | caller action |
|-------|---------|---------------|
| `submitted` | a turn started after this send | done |
| `deposited` | text is in the input, no turn seen | inspect (`session capture`); `session submit` if you decide to |
| `uncertain` | input attempted, outcome unknown | inspect; never resend blindly |
| `not_sent` | failed before typing (incl. `send_lock_busy`, `target_at_gate`) | safe to retry; with a key, the same key is reusable |

### Idempotency keys

With `--idempotency-key KEY` the attempt is recorded **on the target host**
(`~/.local/state/relay/sends/<session>/<KEY>/`) before anything is typed:

- the claim is an atomic `mkdir`; a `typing` marker is written just before the
  text is typed; the final receipt is stored as `receipt.json`;
- a retry with the same key never types again. It replays the stored receipt
  (`"replayed": true`): `submitted` replays as success; `deposited` or
  `uncertain` replays as uncertain (exit 67);
- an attempt that died after `typing` but before its receipt replays as
  `uncertain` with evidence `interrupted_attempt`;
- a claim with no `typing` marker belongs to a sender that has not typed yet.
  For the first minute it is reported as *in progress* (exit 1, nothing
  typed); after that a retry may take it over (atomic rename) and send.

Keys are 1–128 chars of `[A-Za-z0-9._:-]`, starting alphanumeric.

## `session exec --detach`: durable operations

```console
relay session exec ID --detach [--op-id OP] -- CMD
relay session op status ID OP [--cursor N]
relay session op wait   ID OP [--cursor N] [--timeout 60s]
relay session op output ID OP [--stream stdout|stderr] [--offset N] [--limit 16384]
```

`--detach` starts CMD under a runner detached from the SSH connection
(`setsid nohup`, falling back to `nohup`) and keeps all state on the host
under `~/.local/state/relay/ops/<session>/<op>/` (`cmd`, `created`, `pid`,
`started`, `stdout`, `stderr`, `ended`, `exit`). Every status is computed from
those files, so any later connection can query or wait on the op.

- **Start is idempotent per op id.** The launch is claimed with `mkdir`. If
  the start reply is lost, the error output still carries `op_id`; retrying
  with `--op-id` returns the existing op (`"existing": true`) and never runs
  it twice.
- **Receipt** (always JSON, never includes output):
  `{op_id, session_id, host_id, state, cursor, terminal, changed, exit_code,
  created_at, started_at, ended_at, stdout_bytes, stderr_bytes, error}`.
- **States / cursor** (monotonic): `starting` 0 → `running` 1 →
  `exited` | `lost` 2 (terminal). `lost` means the runner vanished without an
  exit code (killed, host rebooted), or a claimed launch never started within
  30 s.
- **`op status --cursor N`** sets `changed` to whether the cursor moved past N.
  Unchanged status is a few fields, with no pane history.
- **`op wait --cursor N`** blocks on the host (20 s chunks) until the cursor
  moves past N or the op is terminal, or `--timeout` elapses (then
  `changed: false`). If the connection drops, it fails with the cursor to wait
  from again (`argv` in the error output); the op keeps running.
- **`op output`** is an explicit, bounded byte-range read (default 16 KiB);
  `next_offset` is the cursor for the next read.

Container sessions are wrapped with `docker exec` exactly like synchronous
`session exec`; host sessions run in the session's remote cwd.

## Argument and shell parsing

`session send` and `session exec` join every argument after `--` with single
spaces into one string. For `exec`, the remote shell (the remote user's shell via
SSH for synchronous exec, `$SHELL -c` for detached ops) then parses that string, so
quoting applies **twice**: once by your local shell, once remotely. Quote the
whole command once locally when it contains remote metacharacters:

```console
relay session exec ID -- 'cd sub && make test 2>&1 | tail -n 20'
```

Flags (`--idempotency-key`, `--detach`, `--op-id`) are only recognized
*before* `--`. Without `--`, every word is payload (the legacy form). A local
quoting mistake is an invocation error, not a Relay execution defect.
