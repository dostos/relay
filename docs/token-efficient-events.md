# Token-efficient event flow

Relay separates durable telemetry from manager interruptions. The handoff and
relayd event stream already own session identity, goal, lineage, timestamps,
and cursors; repeating those facts in a manager composer creates another model
turn without creating state.

## Minimal lifecycle

| Event | Durable effect | Manager wake |
|---|---|---|
| launch / started | handoff state + cursor | no |
| idle | cursor; security-prompt classification only | no, unless a security prompt is visible |
| note / progress | event log + cursor | no |
| hook result | event log + cursor | no |
| explicit result | compact acknowledged envelope | yes |
| ask / permission | one pending deduplicated envelope | yes |
| resolve | correlated decision + audit history | no extra wake |
| exit | acknowledged envelope | yes unless an earlier result already covers it |
| cleanup | terminal handoff/session transaction | no |

Questions must use `relay ask`; Relay no longer invents a question from a
settled composer. Provider hooks mark their lifecycle receipts with
`source=hook`. This is mechanism metadata, not provider policy. An explicit
`relay signal result --text ...` remains a manager-visible milestone.

Security prompts remain exceptional: an idle sample may inspect the pane only
to classify a visible security/permission gate. It never selects a choice.
The exact gate is persisted and only `relay resolve` can act on it.

## Measurement

`TestLifecycleCommunicationMeasurement` models eight events: progress, note,
three Stop-hook receipts, a real ask, an explicit result, and exit. Compared
with the prior route-every-event behavior:

| Measure | Before | After |
|---|---:|---:|
| relayd events | 8 | 8 |
| parent envelopes | 8 | 3 |
| manager wakeups / delivery attempts | 7 | 2 |
| serialized manager-composer bytes | 418 | 139 |
| approximate model tokens (bytes / 4) | 105 | 35 |
| retry opportunities | 7 | 2 |

The token estimate is deliberately mechanical, not a tokenizer claim. It
excludes the much larger avoided manager turns, so it understates savings.
The compact notice uses the durable parent-message ID as its sole routing key;
handoff, lineage, child session, and cursor remain recoverable from that ID and
are not repeated in the composer.

The same rule applies to the default inbox projection. A representative
permission item shrinks from 226 to 146 serialized bytes (about 57 to 37 tokens)
by leaving handoff, child-session, and correlation IDs in the authoritative
envelope and history. `--all` still exposes audit state, while the default item
retains the decision content (text or structured gate), message ID, and
executable next action.
For a recognized gate, the default projection carries the structured reason,
directory, and choices without repeating their formatted text; the measured
item shrinks from 327 to 256 bytes (about 82 to 64 tokens). Unparsed permission
events retain text, and the authoritative envelope always retains both forms.

Managed agent parents use `wake_mode=inject`. Their confirmed composer delivery
is now the sole wake effect instead of also generating a desktop notification
and surface flash. Explicit `notify` parents and legacy registrations retain
desktop presentation. This reduces presentation paths per agent-manager event
from two to one without changing envelope durability or delivery acknowledgement.

Lateral message reads apply the same projection rule. The response already
owns the requested channel, so individual messages omit it; event timestamps
remain in relayd and are used internally for board ordering but are not copied
into model-facing envelopes. A representative two-message read shrinks from
187 to 95 bytes (about 47 to 24 tokens). Fan-in waits still include `channel`
because identifying the winning stream is decision content.

Board queries likewise receive category as an input and use relayd timestamps
only while folding state, so entries now project just node, key, text, and
cursor. A representative two-entry board shrinks from 208 to 112 bytes (about
52 to 28 tokens). Subtree queries preserve node identity, and watches preserve
the sequence needed for the next cursor.

Manager communication pages now use their page-level `next_after` cursor and
the durable message ID instead of repeating per-entry sequence, child-session,
handoff, and correlation IDs. Filters still run against the full ledger before
projection. A representative delta entry shrinks from 394 to 308 bytes (about
99 to 77 tokens); its kind, action, bounded summary, and policy outcome remain.

Pending parent envelopes are retried by the supervisor even when no new child
event arrives. Duplicate child frames now perform deduplication only; they are
not an implicit delivery scheduler. In the failure/recovery regression, five
repeated frames fall from five pane-delivery attempts to two: the initial
attempt and one supervisor-owned recovery attempt.

An explicit `correlation_id` is also an idempotency key within one handoff and
event kind. Producer retries with a new relayd sequence reuse the original
envelope and wake; hostile changed text cannot overwrite the first durable
effect. The two-event regression reduces envelopes and wakeups from two to one.

Adversarial coverage preserves real asks, permission gates, explicit results,
uncovered exits, replay deduplication, disconnected-manager retry, stale gate
failure, and durable cursor advancement. Compression happens before envelope
allocation; it cannot convert a failed delivery into success.
