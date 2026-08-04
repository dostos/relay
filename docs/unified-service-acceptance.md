# Unified-service acceptance matrix

This matrix tracks the evidence required by `HANDOFF.md`. A green unit test is
accepted only where its scope matches the invariant. Live rows record the
2026-08-04 owner-authorized cutover and acceptance run on `home-relay`.

| Requirement | Authoritative evidence | Current state |
|---|---|---|
| Typed classification and one authorization boundary | `TestAuthorityOperationParserSeparatesDiscoveryStartLifecycleAndTargets`, `TestServerAuthorityBoundaryRunsAfterIdentity` | Proven |
| One audit receipt across retries, concurrency, restart, and unrelated large records | authority receipt index tests, including 24 concurrent retries and partial-ledger failure | Proven |
| Apex, ancestor, immediate parent, unrelated subtree, declared repository scope | `TestAuthorityPolicyAllowsApexHierarchyAndPreservesHumanGates` and parent delivery suites | Proven |
| Human trust/login/credential/security gates remain unanswered | authority, readiness, agent, and parent security-gate tests | Proven |
| Component start/failure/restart, reverse shutdown, honest aggregate health | `internal/homeservice/service_test.go` | Proven |
| Event and watcher cursors survive component and process restart | coordinator repair/replay tests, watcher cursor tests, and `scripts/verify-unified-service.sh` | Proven in disposable state |
| Ask/result/security-gate deduplication and typed failover | communication lifecycle and parent concurrency/failover suites | Proven |
| Exact Cursor, Codex, Claude, and direct-job launch payloads and terminal exit | `internal/core/handoff_test.go` | Proven |
| Stateless CLI authentication, forwarding, idempotent effect confirmation | CLI forwarding test and bridge completed/pending/cancelled receipt tests | Proven |
| Viz snapshot, outage, reconnect, stale/duplicate ack, convergence | cmux projection and Viz broker suites | Proven in fixtures |
| Narrow compatibility shim, primary-only install, rollback artifacts | argv mapping, artifact tests, self-update rollback tests, disposable `install.sh` run | Proven in disposable state |
| Concurrent socket ownership and dual-authority prevention | bridge, event coordinator, and home-service ownership tests plus disposable verifier | Proven |
| Malformed, oversized, cancelled, and partial requests/receipts | bridge framing, cancellation, pending and truncated receipt tests; coordinator corruption tests | Proven |
| One installed binary and one authoritative live process | `relay.service` PID 1131333 owns both canonical sockets; `relayd` is a symlink to the same 1-byte-identical executable; three legacy units inactive | Proven live |
| Disposable direct job and every supported agent reaches ready or a genuine gate | direct job `ho-6ad7a7bda3429c61` exited 0; Codex `pm-58d4518cb3472283`, Cursor `pm-6a92b3663153bce6`, and Claude `pm-8f01fec2d6413efe` surfaced genuine unanswered login/theme gates | Proven live |
| Immediate-parent ask/result once across watcher and whole-service restart | job `ho-17e9353c6636e3f9`; ask `pm-2607f03c9273168d` and result `pm-d0e59822f7a2eb18` each appeared once; service restart recovery 6.03 s | Proven live |
| Viz disconnect/reconnect does not affect live hierarchy or delivery | broker PID 1131739 terminated; hierarchy event accepted and health stayed green; broker reconnected as PID 1145614 in 2.077 s from the authoritative projection stream | Proven live |

## Commands

Pre-deployment:

```bash
gofmt -w <changed Go files>
git diff --check
go vet ./...
go build ./...
go test ./...
go test -race ./internal/core ./internal/bridge ./internal/homeservice \
  ./internal/coord/relayd ./internal/coord/sshcoord \
  ./internal/viz/cmux ./internal/vizbroker ./internal/cli
./scripts/verify-unified-service.sh
```

Post-migration evidence must record the migration receipt, `relay doctor`,
`relay service status`, installed file sizes and symlink targets, `systemctl`
unit states/MainPIDs, Unix-socket owners, delivery attempts and semantic
duplicates, emit and failure-notification latency, restart recovery, Viz
convergence, and direct-job/provider terminal outcomes. Never disable the
legacy units until this read-back succeeds; restart them immediately if the new
service fails acceptance.
