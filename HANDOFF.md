# Relay unified-service refactor handoff

## Objective

Refactor Relay into one installed `relay` binary and one authoritative,
always-on home service:

```text
home
└── relay.service
    └── relay service run
        ├── event coordinator
        ├── authenticated command/policy boundary
        ├── watcher reconciler
        └── health and delivery telemetry

desktop (optional)
└── relay viz serve
    └── consumes presentation projections and returns acknowledgements

clients and worker panes
└── relay <command>
    └── stateless authenticated request to the home service
```

Home remains the sole durable authority for hierarchy, ownership, handoffs,
messages, routing, policy decisions, and canonical presentation intent. Viz is
only a consumer. Worker processes emit events and execute work; they do not
hold competing authority.

## Current repository and deployment

- Worktree: `/home/dostos/dev/relay`
- Branch: `agent/harden-relay-delivery`
- Worktree was clean when this document was created.
- Branch is 83 commits ahead of its configured remote.
- Relevant latest commits:
  - `86e9e70 fix(delivery): make parent signaling durable and singular`
  - `702c93f fix(authority): centralize authenticated policy`
  - `ea387e2 fix(launch): preserve terminal failure diagnostics`
- Installed/running build after the last restart: `86e9e70`.
- Existing system services are still separate:
  - `relayd.service` -> `relayd serve`
  - `relay-control.service` -> `relayd control serve`
  - `relay-supervisor.service` -> watcher supervisor
- All three were restarted on 2026-08-04. The desktop bridge and coordinator
  then reported build `86e9e70`, and watcher health recovered.

Do not overwrite or discard existing commits. Preserve unrelated live sessions,
children, state, and user changes.

## Non-negotiable policy

Authorization is expressed once at the authenticated home boundary. Ordinary
Relay hierarchy operations are automatically authorized from authenticated
identity, durable lineage, and explicitly declared sandbox/worktree scope.
This includes apex/root enrollment and repair, immediate-child management,
reparenting/ownership repair, and authenticated desktop control.

Every decision produces one idempotent durable audit receipt. Clients,
forwarders, CLI handlers, and internal mechanisms must not repeat semantic
allowlists or reinterpret authority.

Preserve genuine human gates. Relay must surface and stop at external trust,
login, credentials, host authentication, OS permission prompts, or genuine
security decisions. It must never choose an answer, inject Enter, or silently
bypass them. The control-plane availability declaration also remains an
explicit human policy decision.

Service-layer structural invariants remain valid mechanism checks: cycle and
self-parent prevention, state-transition validity, authority-store role/fencing,
crash recovery, and safe filesystem boundaries. They are not caller policy.

## Delivery invariants already implemented

The recent signaling work must survive the refactor:

- A child signals its immediate parent once through a durable envelope.
- Concurrent different-sequence copies of the same ask deduplicate under the
  authority lock.
- Watcher/supervisor restarts do not duplicate delivery or lose the cursor.
- Each attempt records target, count, time, and outcome.
- A live, blocked, unknown, or transiently failing direct parent is not skipped.
- Escalation occurs only for conclusively unavailable parents.
- An apex/root ask or result reaches the human authority surface without
  inventing a synthetic hierarchy parent.
- Security-gate decisions remain durable and cannot be acknowledged away.

The seq-708/710 failure was structural: apex handoff
`ho-d22cf70ac46320f9` had no `SourceSessionID`, so the old supervisor excluded
it. Tests now cover the same root ask/result shape.

Existing communication measurement:

| Metric | Legacy | Current |
|---|---:|---:|
| Parent envelopes | 8 | 3 |
| Manager wakeups/delivery attempts | 7 | 2 |
| Manager notice bytes | 418 | 159 |
| Approximate tokens | 105 | 40 |

`TestConcurrentAskFramesCreateOneSemanticEnvelope` has also exercised 24
concurrent equivalent asks producing one durable envelope/wake.

## Confirmed defects to fix first

These were discovered while trying to create a refactor handoff. Reproduce and
add regressions before broader restructuring.

1. The central authority policy overclassifies read-only/control verbs.
   Authenticated calls to `relay agent protocol`, `relay agent pick`, and
   `relay handoff list` are refused as though they target a handoff. Internal
   lifecycle verbs are also denied. The typed operation parser must distinguish
   discovery, start, lifecycle, and target-bearing operations rather than use
   positional guesses.

2. `appendAuthorityDecision` uses the default `bufio.Scanner` token limit.
   A large existing ledger record causes every later authorization attempt to
   fail with `bufio.Scanner: token too long`. Do not merely raise an arbitrary
   limit: make receipt lookup bounded and structurally independent of unrelated
   record size (for example, a compact receipt index or streaming decoder with
   an explicit supported record bound and honest corruption reporting).

3. Launch terminal instrumentation can corrupt complex generated agent
   commands. Failed handoff `ho-eaba234fc4ff7c1b` recorded exit code 127 and a
   heavily re-quoted command ending in `codex: command not found`, although
   `/home/dostos/.local/bin/codex` is present in a fresh login shell. The nested
   `bash -lc` wrapping in `withLaunchTerminalReceipt` needs an argv-safe design.
   Test the exact generated Cursor, Codex, and Claude commands, including hook
   configuration, not simplified specimens.

4. A disposable direct-job canary, `ho-0e44caed5f450c98`, emitted its result but
   remained displayed as running during the interrupted verification. Diagnose
   the terminal-state/cursor path without mutating unrelated work.

5. One malformed refactor goal was stored on failed handoff
   `ho-eaba234fc4ff7c1b` because backticks inside a shell double-quoted argument
   were expanded. Do not copy that record as the goal. This document is the
   source of truth.

## Target design

### One executable

Ship one primary executable:

```sh
relay service run
relay viz serve
relay agent start ...
relay doctor
```

Keep `relayd` temporarily as a narrow compatibility shim that execs the
corresponding `relay` subcommand and emits a deprecation diagnostic. Define and
document a removal condition based on migrated service units and client build
floor, not elapsed time alone.

### One authoritative service

`relay service run` hosts independently supervised components in one process.
Each component needs explicit readiness, liveness, build identity, last-success,
last-failure, restart count, and durable-effect checks. A component failure may
restart that component internally when safe, but must not leave overall health
green while inert. Shared-process cancellation must not corrupt cursors or
authority state.

Use one systemd unit and one atomic install/restart path. Avoid three processes
that can run different builds. The service should own sockets and child
forwarders and shut down in an order that preserves receipts and retryability.

### Stateless CLI client

CLI commands parse locally only enough to form a typed request and display a
response. The home service authenticates the caller, resolves identity/lineage
and declared scope, authorizes once, writes the audit receipt, and executes the
operation against authoritative state.

Do not retain a semantic command allowlist in the bridge/client. Transport may
validate framing, maximum sizes, encoding, and authentication material. Policy
belongs at home.

Host-local event emission may retain a low-latency local path, but its durable
identity and replay behavior must remain equivalent to service submission.

### Viz as consumer

Canonical presentation projection state belongs to home. `relay viz serve`
subscribes, applies changes to cmux, and returns idempotent acknowledgements.
It cannot enroll/reparent sessions, decide authorization, hold the only copy of
lineage, or become a prerequisite for message delivery.

When Viz is disconnected, home queues/coalesces pending projection intent and
continues hierarchy and delivery work. Reconnection must converge from a fresh
snapshot plus cursor without duplicating panes or accepting stale authority.

## Migration constraints

1. Introduce the unified service behind disposable state and sockets.
2. Prove parity and component fault isolation before touching live units.
3. Add compatibility routing from old `relayd` commands and unit names.
4. Install the single binary without stopping healthy system-owned services.
5. Produce an explicit migration/doctor receipt naming old and new builds,
   sockets, process owners, and rollback action.
6. System-service mutation or `sudo` requires a fresh, explicit human decision.
7. After migration, verify only one authoritative service/process owns the
   canonical sockets and state. Retire obsolete units only after read-back.
8. Keep Viz separate because it runs on a different host/trust/lifecycle
   boundary, even though it uses the same binary distribution.

## Required verification

Add invariant-level unit and integration coverage for:

- typed command classification and single boundary authorization;
- one idempotent audit receipt under retries, concurrency, restart, and large
  unrelated ledger records;
- apex, ancestor-repair, immediate-parent, unrelated-subtree, and declared
  worktree/sandbox decisions;
- external trust/login/credential/security requests returning human-required
  without a selected answer;
- service component start, failure, restart, shutdown ordering, and honest
  aggregate health;
- event/watcher cursor survival across component and whole-service restarts;
- ask/result/security-gate deduplication and typed failover outcomes;
- exact generated commands for every supported agent CLI and direct jobs;
- CLI request forwarding and response/effect confirmation;
- Viz snapshot, disconnect, reconnect, stale ack, duplicate ack, and convergence;
- compatibility shim and old-service migration/rollback;
- concurrent socket ownership and prevention of dual authority;
- malformed, oversized, cancelled, and partially written requests/receipts.

Run at minimum:

```sh
gofmt on changed Go files
git diff --check
go vet ./...
go build ./...
go test ./...
go test -race on authority, bridge, service, delivery, coordinator, and Viz packages
```

Add safe fault-injection and repeated integration runs without timing-only
sleeps. Use disposable state directories, sockets, tmux servers, and service
fixtures. Never use unrelated live sessions as tests.

Before/after evidence must include:

- authoritative process and system-service count;
- installed binary count and bytes;
- build identities and upgrade/restart steps;
- manager bytes and wakeups;
- semantic duplicates and suppressed duplicates;
- emit-to-confirm latency (p50/p95/max);
- first-unavailable-to-failure-notification latency;
- watcher/component restart recovery time;
- Viz disconnect/reconnect convergence time;
- direct-job and supported-agent launch/result/exit behavior.

## Live acceptance

Deployment is complete only when all tests pass, install succeeds, the human
has explicitly authorized any required system-service mutation, and live
read-back proves:

- one current-build authoritative home service owns the sockets/state;
- doctor reports every unified component honestly;
- no stale old service can accept commands or write authority state;
- a disposable direct job completes with one result and terminal state;
- each supported agent CLI reaches ready or surfaces a genuine external gate;
- one child ask and one result reach the immediate parent once;
- watcher and whole-service restart preserve delivery without duplicates;
- Viz may disconnect/reconnect without affecting delivery or hierarchy;
- unrelated pre-existing doctor findings are reported but not repaired unless
  separately authorized.

Report only a material verified milestone, a genuine external human gate, or
the final deployed result. Do not claim deployment while compatibility units or
old processes still own authority.
