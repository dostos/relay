# Relay communication patterns

Relay has one durable control plane and several projections. This catalog is
the boundary: adding a new communication path requires naming its durable
owner, wake rule, acknowledgement effect, and cleanup rule here and in tests.

| Phase | Direction | Pattern | Durable owner | Wake / acknowledgement | Effect coverage |
|---|---|---|---|---|---|
| launch | manager → child | managed agent goal | handoff + session registry | launch and goal delivery are acknowledged separately | `TestAgentSend*`, `TestLaunch*` |
| launch | manager → child | direct command job | handoff + session registry | process start is the delivery effect | `TestCommandSend*`, `TestLaunch*` |
| lifecycle | child → manager | started, heartbeat, inject, idle, note, progress, hook result | relayd event log + handoff cursor | receipt only; never wakes | `TestCommunicationLifecycleMatrix` |
| decision | child → manager | ask, needs-input | parent envelope | one wake per unresolved question | `TestCommunicationLifecycleMatrix` |
| security | child → manager | permission, trust, login, security gate | parent envelope; recognized gates also persist structured choices | exactly one wake; explicit resolve only | `TestCommunicationLifecycleMatrix`, `TestAskLabelCannotHideVisibleSecurityGateFromPolicy`, `TestExplicitGateApproveDeliversPendingGoalAndDenyCleansUp` |
| completion | child → manager | explicit result, uncovered exit | parent envelope + history | milestone wake; result coalesces following exit | `TestCommunicationLifecycleMatrix` |
| resolution | manager → child | reply, approve, deny | parent envelope + communication history | confirmed child effect before closure | `TestResolveAndDeliveryCloseInboxWithCorrelatedHistory`, `TestExplicitGateApproveDeliversPendingGoalAndDenyCleansUp` |
| receipt | manager → control plane | ack | parent envelope + communication history | closes a receipt; no child effect | `TestResolveAndDeliveryCloseInboxWithCorrelatedHistory` |
| retry | watcher ↔ control plane | reconnect, replay, pending delivery | relayd event seq + handoff cursor | cursor commits only after durable routing | `TestWatcherCursorCommitsOnlyAfterDurableRoute`, `TestDisconnectedParentRetriesOneDurableAttentionEnvelope` |
| hierarchy | child → manager → ancestor | nearest reachable manager failover | parent envelope + lineage | ownership moves only after confirmed delivery | `TestParentMessageCarriesFailoverAttribution` |
| lateral | sibling ↔ sibling | board post, query, watch, subtree roll-up | manager-scoped board log | cursor-based watch; no self echo | `TestBoard*` |
| projection | authority → client | session snapshot, tombstone, focus, delete | authoritative registry + client watermark | ordered projection; visualization owns no lifecycle | `TestProjectionTombstoneRejectsOlderReplay`, `TestProjectionFocusReplaysAgainstLiveEqualRevision` |
| presentation | authority ↔ client | surface upsert + ack | projection revision + surface binding | ack must match session and revision | `TestApplyProjectionAckRequiresMatchingSessionAndRevision` |
| cleanup | authority → child/client | finalize, reconcile, retire, delete, failed-launch cleanup | deletion reservation + registry | each operation preserves its own absence/force/async contract | `TestCleanupFailedChildRetiresMissingHandoffArtifact`, `TestProjectedDeleteSurvivesVizOutage`, `TestReplaceApexMovesDirectChildrenAndHandoffs` |
| client lifecycle | authority ↔ all clients | update event, receipt, process-build status | client event stream + update receipt | current and requested builds remain distinguishable | `TestUpdateAckReportsInstalledBuildBeforeRestart`, `TestMalformedPendingUpdateReceiptIsQuarantined` |
| inspection | caller → authority | session, pane, history, log, inbox, status reads | the owning registry/log | fail closed on non-authoritative or malformed state | `TestProjectionOnlyAuthorityFamiliesFailClosed`, `TestProjectionSessionListDoesNotCollapseInventoryFailureToEmpty` |

The routing matrix is intentionally adversarial. It covers duplicate event
sequences, repeated gate frames, hostile `source=hook` metadata, telemetry that
looks like a permission request, and mislabeled or unparsed permission events.
The adjacent watcher test covers replay after the first durable write fails.
Compression is valid only when it reduces manager wakes without changing these
effects.

Run the cross-package communication contract with:

```sh
go test ./... -run 'CommunicationLifecycleMatrix|WatcherCursorCommitsOnlyAfterDurableRoute|AgentSend|CommandSend|Launch|Parent|Policy|Board|Projection|Viz|Update|Cleanup|ReplaceApex'
```

## Optimization rule

Events already owned by relayd are receipts, not messages to retype into a
manager composer. A manager turn exists only for a decision, an explicit
milestone, or an uncovered failure. Durable IDs and cursors carry context;
screen excerpts are limited to the exact decision. Never compress away a
failed write, stale cursor, duplicate delivery, terminal exit, or security
gate.

Interactive delivery distinguishes confirmed, definitely failed before input,
and uncertain after input/submission was attempted. Only a definite pre-input
failure is an automatic retry. An uncertain effect stays pending and auditable;
watchers must not retype it, and reconciliation or an explicit operator action
owns any later attempt.
