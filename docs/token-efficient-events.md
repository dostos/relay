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
| serialized manager-composer bytes | 418 | 159 |
| approximate model tokens (bytes / 4) | 105 | 40 |
| retry opportunities | 7 | 2 |

The token estimate is deliberately mechanical, not a tokenizer claim. It
excludes the much larger avoided manager turns, so it understates savings.
The compact notice uses the durable parent-message ID as its sole routing key;
handoff, lineage, child session, and cursor remain recoverable from that ID and
are not repeated in the composer.

Adversarial coverage preserves real asks, permission gates, explicit results,
uncovered exits, replay deduplication, disconnected-manager retry, stale gate
failure, and durable cursor advancement. Compression happens before envelope
allocation; it cannot convert a failed delivery into success.
