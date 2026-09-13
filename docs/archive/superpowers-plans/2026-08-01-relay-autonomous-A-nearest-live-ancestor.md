# Relay Autonomous Mode Part A — Nearest-Live-Ancestor Escalation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a child agent escalates and its immediate manager is disconnected, deliver the escalation to the nearest *live* ancestor instead of stalling until that exact manager reconnects.

**Architecture:** `ParentService.RouteChildEvent` currently resolves exactly one edge up (`ho.SourceSessionID`) and delivers there. We add an upward ancestor walk that picks the first ancestor to which delivery actually succeeds, allocate the escalation against that resolved ancestor, and record who was skipped. Liveness is not a separate presence oracle — it is the *outcome of attempting delivery* (`Notifier.NotifyParent` for local roots, `Sessions.Send` for remote managers), so there is no second source of truth and no TOCTOU race.

**Tech Stack:** Go 1.26.5, stdlib `testing` only. Single non-stdlib dep is `gopkg.in/yaml.v3` (unused here).

## Global Constraints

- Language/toolchain: Go 1.26.5. Test command is exactly `go test ./...` (README.md:288). No build tags, no `TestMain`, no `t.Parallel`, no third-party assertion libraries.
- Tests are colocated, same-package (`package core`), plain stdlib `testing`, hand-written `if cond { t.Fatalf(...) }` assertions.
- Test isolation is exactly one mechanism: `t.Setenv("RELAY_STATE_DIR", t.TempDir())`.
- **Security invariant (never violate):** a *live* manager is never skipped. Only disconnected/missing/terminal ancestors are passed over. This preserves relay's rule that descendants cannot bypass an active manager.
- Commit style: module-prefixed conventional commits, e.g. `[orchestration] feat: ...`.
- All new `ParentMessage` fields must be additive with zero-value defaults (no migration; existing records stay valid).
- The ancestor walk must be bounded: max depth + visited-set. Nothing in the codebase enforces acyclicity.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/core/ancestor.go` (create) | The upward lineage walk: enumerate a handoff's ancestor chain, bounded and cycle-safe. Pure, no I/O beyond `Registry` reads. |
| `internal/core/ancestor_test.go` (create) | Tests for the walk in isolation (chains, roots, cycles, depth bound, missing sessions). |
| `internal/core/parent.go` (modify) | `ParentMessage` new fields; `RouteChildEvent` target resolution; `deliverMessage` bounded attempt; `pendingAttention` chain-scoping. |
| `internal/core/parent_test.go` (modify) | Failover behaviour tests using existing `fakeParentNotifier.notifyFail` and a failing persistence fake. |

Rationale for a new file: `parent.go` is already ~1200 lines. The ancestor walk is a distinct, independently testable responsibility with a clean interface, so it gets its own file rather than growing `parent.go` further.

---

### Task 1: Bounded upward ancestor walk

**Files:**
- Create: `internal/core/ancestor.go`
- Test: `internal/core/ancestor_test.go`

**Interfaces:**
- Consumes: `Registry.GetSession(id string) (*Session, error)` (`internal/core/registry.go`), `Session.SourceSessionID` (`internal/core/types.go:11`).
- Produces: `func AncestorChain(reg *Registry, startSessionID string) []*Session` — ordered nearest-first, excluding the start session itself. Used by Tasks 2 and 4.

Background facts the implementer needs:
- A session's parent is `Session.SourceSessionID`. There is **no** existing helper for this; it is always `reg.GetSession(id)` then read `.SourceSessionID`.
- A root (local parent) has an **empty** `SourceSessionID` — that is the natural termination.
- `Registry` is a concrete struct (not an interface); `&Registry{}` after `t.Setenv("RELAY_STATE_DIR", …)` is the test registry.
- Nothing enforces acyclicity, so the walk needs a visited-set *and* a depth cap.

- [ ] **Step 1: Write the failing test**

Create `internal/core/ancestor_test.go`:

```go
package core

import (
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func putChain(t *testing.T, reg *Registry, ids ...string) {
	t.Helper()
	now := time.Now().UTC()
	for i, id := range ids {
		sess := &Session{
			ID: id, HostID: "h", Persist: ports.PersistHandle{Kind: "tmux", Name: id},
			CreatedAt: now,
		}
		if i+1 < len(ids) {
			sess.SourceSessionID = ids[i+1]
		}
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAncestorChainReturnsNearestFirstAndStopsAtRoot(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	// child -> mid -> root (root has empty SourceSessionID)
	putChain(t, reg, "sess-child", "sess-mid", "sess-root")

	chain := AncestorChain(reg, "sess-child")
	if len(chain) != 2 {
		t.Fatalf("want 2 ancestors, got %d: %+v", len(chain), chain)
	}
	if chain[0].ID != "sess-mid" || chain[1].ID != "sess-root" {
		t.Fatalf("wrong order: %s, %s", chain[0].ID, chain[1].ID)
	}
}

func TestAncestorChainStopsOnCycle(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	a := &Session{ID: "sess-a", SourceSessionID: "sess-b", CreatedAt: now}
	b := &Session{ID: "sess-b", SourceSessionID: "sess-a", CreatedAt: now}
	for _, s := range []*Session{a, b} {
		if err := reg.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	chain := AncestorChain(reg, "sess-a")
	if len(chain) != 1 || chain[0].ID != "sess-b" {
		t.Fatalf("cycle not bounded, got %+v", chain)
	}
}

func TestAncestorChainStopsOnMissingSession(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	orphan := &Session{ID: "sess-orphan", SourceSessionID: "sess-gone", CreatedAt: now}
	if err := reg.PutSession(orphan); err != nil {
		t.Fatal(err)
	}
	chain := AncestorChain(reg, "sess-orphan")
	if len(chain) != 0 {
		t.Fatalf("want empty chain, got %+v", chain)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestAncestorChain -v`
Expected: FAIL — `undefined: AncestorChain`

- [ ] **Step 3: Write minimal implementation**

Create `internal/core/ancestor.go`:

```go
package core

// maxAncestorDepth bounds the upward lineage walk. Relay does not enforce
// acyclicity anywhere, and a corrupted or hand-repaired lineage must never
// hang escalation routing.
const maxAncestorDepth = 32

// AncestorChain returns the ancestors of startSessionID, nearest first,
// excluding the start session. It stops at a root (empty SourceSessionID), a
// missing session, a cycle, or maxAncestorDepth — whichever comes first.
func AncestorChain(reg *Registry, startSessionID string) []*Session {
	if reg == nil || startSessionID == "" {
		return nil
	}
	var out []*Session
	visited := map[string]bool{startSessionID: true}
	current := startSessionID
	for depth := 0; depth < maxAncestorDepth; depth++ {
		sess, err := reg.GetSession(current)
		if err != nil || sess == nil || sess.SourceSessionID == "" {
			return out
		}
		next := sess.SourceSessionID
		if visited[next] {
			return out
		}
		visited[next] = true
		parent, err := reg.GetSession(next)
		if err != nil || parent == nil {
			return out
		}
		out = append(out, parent)
		current = next
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestAncestorChain -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**

```bash
cd ~/dev/relay
git add internal/core/ancestor.go internal/core/ancestor_test.go
git commit -m "[orchestration] feat: add bounded upward ancestor walk"
```

---

### Task 2: Record failover attribution on ParentMessage

**Files:**
- Modify: `internal/core/parent.go:35-55` (the `ParentMessage` struct)
- Test: `internal/core/parent_test.go`

**Interfaces:**
- Consumes: `ParentMessage` (`parent.go:35`).
- Produces: three additive fields used by Tasks 3 and 4:
  - `IntendedParentSessionID string` — the immediate parent that was skipped (empty when delivered directly).
  - `SkippedSessionIDs []string` — every disconnected ancestor passed over, for audit.
  - `ResolvedBySessionID string` — who actually ruled.

Note: `ParentSessionID` keeps its existing meaning — *the session whose inbox holds this message*. After Part A that is the **resolved** ancestor, which is why all existing storage/listing/delivery code keeps working unchanged (`parentMessageDir` keys off it, `FindMessage` already scans every parent directory).

- [ ] **Step 1: Write the failing test**

Append to `internal/core/parent_test.go`:

```go
func TestParentMessageCarriesFailoverAttribution(t *testing.T) {
	msg := &ParentMessage{
		V: 1, ID: "pm-x", ParentSessionID: "sess-root",
		IntendedParentSessionID: "sess-mid",
		SkippedSessionIDs:       []string{"sess-mid"},
		ResolvedBySessionID:     "sess-root",
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back ParentMessage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.IntendedParentSessionID != "sess-mid" {
		t.Fatalf("intended parent lost: %+v", back)
	}
	if len(back.SkippedSessionIDs) != 1 || back.SkippedSessionIDs[0] != "sess-mid" {
		t.Fatalf("skipped ids lost: %+v", back)
	}
	if back.ResolvedBySessionID != "sess-root" {
		t.Fatalf("resolver lost: %+v", back)
	}
}

func TestParentMessageOmitsFailoverFieldsWhenDeliveredDirectly(t *testing.T) {
	msg := &ParentMessage{V: 1, ID: "pm-y", ParentSessionID: "sess-root"}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"intended_parent_session_id", "skipped_session_ids", "resolved_by_session_id"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("field %s must be omitted when empty: %s", field, raw)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestParentMessage -v`
Expected: FAIL — `unknown field IntendedParentSessionID in struct literal`

- [ ] **Step 3: Write minimal implementation**

In `internal/core/parent.go`, inside the `ParentMessage` struct, add these three fields immediately after the `ChildSessionID` line (`parent.go:40`):

```go
	// Failover attribution. Empty on the common path where the immediate
	// parent received the escalation directly.
	IntendedParentSessionID string   `json:"intended_parent_session_id,omitempty"`
	SkippedSessionIDs       []string `json:"skipped_session_ids,omitempty"`
	ResolvedBySessionID     string   `json:"resolved_by_session_id,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestParentMessage -v`
Expected: PASS (2 tests)

- [ ] **Step 5: Commit**

```bash
cd ~/dev/relay
git add internal/core/parent.go internal/core/parent_test.go
git commit -m "[orchestration] feat: record escalation failover attribution"
```

---

### Task 3: Bound each delivery attempt

**Files:**
- Modify: `internal/core/parent.go:636-670` (`deliverMessage`)
- Test: `internal/core/parent_test.go`

**Interfaces:**
- Consumes: `deliverMessage(ctx context.Context, parent *Session, ho *Handoff, msg *ParentMessage) error` (`parent.go:636`).
- Produces: same signature; delivery attempts now carry their own timeout. Task 4 relies on this to fail over quickly instead of hanging.

Why: `SessionService.Send` (`session.go:378`) passes the caller's context straight through to the transport with **no internal timeout**. A delivery attempt to a dead SSH host would otherwise hang the whole ancestor walk. `session.go:574` already establishes a `12*time.Second` bound for a remote op; per-hop must be well under that.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/parent_test.go`:

```go
// blockingPersistence simulates a dead SSH host: Send hangs until ctx dies.
type blockingPersistence struct {
	renamePersistence
}

func (p *blockingPersistence) Send(ctx context.Context, _ ports.Transport, _ ports.PersistHandle, _ string, _ bool) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestDeliveryAttemptIsBounded(t *testing.T) {
	service, _, reg := newParentTestService(t)
	service.Sessions.Persist = &blockingPersistence{}
	now := time.Now().UTC()
	manager := &Session{ID: "sess-manager", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "manager"}, CreatedAt: now}
	if err := reg.PutSession(manager); err != nil {
		t.Fatal(err)
	}
	ho := &Handoff{ID: "ho-1", SessionID: "sess-child", HostID: "c3", Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now}
	msg := &ParentMessage{V: 1, ID: "pm-block", ParentSessionID: manager.ID, ChildSessionID: "sess-child", HandoffID: ho.ID, Kind: "ask", State: ParentMessagePending, CreatedAt: now}

	start := time.Now()
	err := service.deliverMessage(context.Background(), manager, ho, msg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want delivery error when the transport hangs")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("delivery was not bounded, took %s", elapsed)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestDeliveryAttemptIsBounded -v`
Expected: FAIL — the test hangs and is killed by the Go test timeout (`panic: test timed out`), because `Send` blocks forever on a context that is never cancelled.

- [ ] **Step 3: Write minimal implementation**

In `internal/core/parent.go`, add this constant next to `parentTextLimit` (`parent.go:23`):

```go
// deliveryAttemptTimeout bounds ONE delivery hop. SessionService.Send passes
// the caller's context straight to the transport with no timeout of its own,
// so without this a dead SSH host would stall the whole ancestor walk.
const deliveryAttemptTimeout = 5 * time.Second
```

Then in `deliverMessage`, replace the delivery branch (`parent.go:649-656`):

```go
	var err error
	if isLocalParent(parent) && p.Notifier != nil {
		err = p.Notifier.NotifyParent(ctx, parent.ID, notice)
	} else if !isLocalParent(parent) && p.Sessions != nil {
		err = p.Sessions.Send(ctx, parent.ID, FormatParentNotice(notice), true)
	} else {
		err = fmt.Errorf("no delivery path for parent %s", parent.ID)
	}
```

with:

```go
	var err error
	if isLocalParent(parent) && p.Notifier != nil {
		attemptCtx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
		err = p.Notifier.NotifyParent(attemptCtx, parent.ID, notice)
		cancel()
	} else if !isLocalParent(parent) && p.Sessions != nil {
		attemptCtx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
		err = p.Sessions.Send(attemptCtx, parent.ID, FormatParentNotice(notice), true)
		cancel()
	} else {
		err = fmt.Errorf("no delivery path for parent %s", parent.ID)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestDeliveryAttemptIsBounded -v`
Expected: PASS (returns in ~5s with a `context deadline exceeded` error)

- [ ] **Step 5: Run the full suite for regressions**

Run: `cd ~/dev/relay && go test ./...`
Expected: all packages `ok`

- [ ] **Step 6: Commit**

```bash
cd ~/dev/relay
git add internal/core/parent.go internal/core/parent_test.go
git commit -m "[orchestration] fix: bound each escalation delivery attempt"
```

---

### Task 4: Fail escalation over to the nearest live ancestor

**Files:**
- Modify: `internal/core/parent.go:746-803` (`RouteChildEvent`)
- Test: `internal/core/parent_test.go`

**Interfaces:**
- Consumes: `AncestorChain(reg, startSessionID)` (Task 1); the three attribution fields (Task 2); bounded `deliverMessage` (Task 3).
- Produces: `RouteChildEvent` unchanged signature — `(ctx, *Handoff, coord.Event) (*ParentMessage, error)`. On return, `msg.ParentSessionID` is the ancestor that actually received it.

Design constraints the implementer must respect:
- **Resolve the target *before* allocating the message.** `parentMessageID` hashes the parent ID (`parent.go:459`) and `parentMessageDir` keys the inbox directory off `ParentSessionID` (`parent.go:464`). Allocating first and retargeting later would mint a second ID in a second directory for one logical escalation.
- **Never skip a live manager.** The walk stops at the first ancestor whose delivery *succeeds*.
- Keep the existing `writeParentMessage(msg, true)` exclusive-create + `AppendCommunication` + `applyPolicy` ordering for the chosen target.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/parent_test.go`:

```go
// failingPersistence simulates an unreachable remote manager.
type failingPersistence struct {
	renamePersistence
	attempts []string
}

func (p *failingPersistence) Send(_ context.Context, _ ports.Transport, handle ports.PersistHandle, _ string, _ bool) error {
	p.attempts = append(p.attempts, handle.Name)
	return errors.New("host unreachable")
}

func TestEscalationFailsOverToNearestLiveAncestor(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	// The remote manager is unreachable; the local root is live.
	service.Sessions.Persist = &failingPersistence{}
	now := time.Now().UTC()
	root := &Session{
		ID: "sess-root", HostID: LocalHostID,
		Persist:   ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"},
		Labels:    map[string]string{"role": ParentRole, "wake_mode": "notify"},
		CreatedAt: now,
	}
	manager := &Session{
		ID: "sess-manager", HostID: "c1",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "manager"},
		SourceSessionID: root.ID, CreatedAt: now,
	}
	child := &Session{
		ID: "sess-child", HostID: "c3",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "worker"},
		SourceSessionID: manager.ID, CreatedAt: now,
	}
	for _, sess := range []*Session{root, manager, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{
		ID: "ho-worker", SessionID: child.ID, HostID: child.HostID,
		Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now,
	}

	msg, err := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve tool?"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg == nil {
		t.Fatal("want an escalation message")
	}
	if msg.ParentSessionID != root.ID {
		t.Fatalf("want delivery to the live root, got %s", msg.ParentSessionID)
	}
	if msg.IntendedParentSessionID != manager.ID {
		t.Fatalf("want the skipped manager recorded, got %q", msg.IntendedParentSessionID)
	}
	if len(msg.SkippedSessionIDs) != 1 || msg.SkippedSessionIDs[0] != manager.ID {
		t.Fatalf("want the manager in skipped ids, got %+v", msg.SkippedSessionIDs)
	}
	if msg.DeliveredAt == nil {
		t.Fatal("want the escalation delivered")
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("want exactly one human-facing notice, got %d", len(notifier.notices))
	}
}

func TestEscalationNeverSkipsALiveManager(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	recorder := &recordingPersistence{}
	service.Sessions.Persist = recorder
	now := time.Now().UTC()
	root := &Session{
		ID: "sess-root", HostID: LocalHostID,
		Persist:   ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"},
		Labels:    map[string]string{"role": ParentRole, "wake_mode": "notify"},
		CreatedAt: now,
	}
	manager := &Session{
		ID: "sess-manager", HostID: "c1",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "manager"},
		SourceSessionID: root.ID, CreatedAt: now,
	}
	child := &Session{
		ID: "sess-child", HostID: "c3",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "worker"},
		SourceSessionID: manager.ID, CreatedAt: now,
	}
	for _, sess := range []*Session{root, manager, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{
		ID: "ho-worker", SessionID: child.ID, HostID: child.HostID,
		Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now,
	}

	msg, err := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve tool?"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ParentSessionID != manager.ID {
		t.Fatalf("a live manager must not be skipped, went to %s", msg.ParentSessionID)
	}
	if msg.IntendedParentSessionID != "" {
		t.Fatalf("no failover expected, got intended=%q", msg.IntendedParentSessionID)
	}
	if len(notifier.notices) != 0 {
		t.Fatalf("the human root must not be interrupted, got %d notices", len(notifier.notices))
	}
	if len(recorder.sent) != 1 {
		t.Fatalf("want one tmux injection to the manager, got %d", len(recorder.sent))
	}
}

func TestEscalationStaysPendingWhenNoAncestorIsLive(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Sessions.Persist = &failingPersistence{}
	notifier.notifyFail = true
	now := time.Now().UTC()
	root := &Session{
		ID: "sess-root", HostID: LocalHostID,
		Persist:   ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"},
		Labels:    map[string]string{"role": ParentRole, "wake_mode": "notify"},
		CreatedAt: now,
	}
	manager := &Session{
		ID: "sess-manager", HostID: "c1",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "manager"},
		SourceSessionID: root.ID, CreatedAt: now,
	}
	child := &Session{
		ID: "sess-child", HostID: "c3",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "worker"},
		SourceSessionID: manager.ID, CreatedAt: now,
	}
	for _, sess := range []*Session{root, manager, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{
		ID: "ho-worker", SessionID: child.ID, HostID: child.HostID,
		Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now,
	}

	msg, _ := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve tool?"}})
	if msg == nil {
		t.Fatal("the escalation must still be durably recorded")
	}
	if msg.DeliveredAt != nil {
		t.Fatal("nothing was reachable; it must not be marked delivered")
	}
	if msg.State != ParentMessagePending {
		t.Fatalf("want it left pending for reconnect retry, got %s", msg.State)
	}
	// It must be retryable by DeliverPending against the parent that holds it.
	if _, err := service.ListMessages(msg.ParentSessionID, true); err != nil {
		t.Fatalf("pending message must be listable: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestEscalation -v`
Expected: FAIL — `TestEscalationFailsOverToNearestLiveAncestor` fails with `want delivery to the live root, got sess-manager` (today delivery is strictly one edge up and simply errors out).

- [ ] **Step 3: Write minimal implementation**

In `internal/core/parent.go`, add this method just above `RouteChildEvent` (`parent.go:746`):

```go
// resolveDeliveryTarget picks the ancestor that will receive an escalation.
// Liveness is the OUTCOME of attempting delivery, not a separate presence
// oracle, so this returns the candidate chain and the caller stops at the
// first hop whose delivery succeeds. A live manager is therefore never
// skipped — only ancestors that genuinely cannot receive the envelope.
func (p *ParentService) deliveryCandidates(ho *Handoff) []*Session {
	immediate, err := p.Reg.GetSession(ho.SourceSessionID)
	if err != nil || immediate == nil {
		return nil
	}
	candidates := []*Session{immediate}
	return append(candidates, AncestorChain(p.Reg, immediate.ID)...)
}
```

Then replace the body of `RouteChildEvent` from the parent lookup (`parent.go:754-757`) through the final delivery (`parent.go:800-802`). Replace:

```go
	parent, err := p.Reg.GetSession(ho.SourceSessionID)
	if err != nil {
		return nil, err
	}
```

with:

```go
	candidates := p.deliveryCandidates(ho)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("handoff %s has no reachable parent lineage", ho.ID)
	}
	parent := candidates[0]
```

and replace the tail:

```go
	// Delivery is strictly one edge up the tree. Only a local root owns a
	// human-facing cmux surface; every other parent is an agent manager.
	return msg, p.deliverMessage(ctx, parent, ho, msg)
```

with:

```go
	// Delivery walks UP to the nearest ancestor that can actually receive the
	// envelope. A live manager is never skipped: the walk stops at the first
	// successful delivery. Only a local root owns a human-facing cmux surface;
	// every other ancestor is an agent manager.
	var deliverErr error
	for i, candidate := range candidates {
		if i > 0 {
			// Retarget: this envelope now belongs to the ancestor's inbox.
			if err := p.retargetMessage(msg, candidates[i-1], candidate); err != nil {
				return msg, err
			}
		}
		deliverErr = p.deliverMessage(ctx, candidate, ho, msg)
		if deliverErr == nil {
			return msg, nil
		}
	}
	// Nothing in the chain was reachable. The envelope stays durably pending
	// with its last target so DeliverPending retries it on reconnect.
	return msg, deliverErr
}

// retargetMessage moves a still-undelivered envelope from a skipped ancestor's
// inbox to the next candidate's, preserving one logical escalation: the inbox
// is a directory per parent session and the message id hashes the parent id,
// so the old record is removed rather than left as a duplicate ask.
func (p *ParentService) retargetMessage(msg *ParentMessage, from, to *Session) error {
	if msg.IntendedParentSessionID == "" {
		msg.IntendedParentSessionID = from.ID
	}
	msg.SkippedSessionIDs = append(msg.SkippedSessionIDs, from.ID)
	oldPath := parentMessagePath(from.ID, msg.ID)
	msg.ParentSessionID = to.ID
	if err := writeParentMessage(msg, false); err != nil {
		return err
	}
	_ = os.Remove(oldPath)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestEscalation -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Run the full suite for regressions**

Run: `cd ~/dev/relay && go test ./...`
Expected: all packages `ok`. In particular `TestRouteChildEventDeduplicatesAndKeepsMessageCompact` and `TestRemoteManagerReceivesChildEventWithoutHumanNotification` must still pass — they assert the no-failover path.

- [ ] **Step 6: Commit**

```bash
cd ~/dev/relay
git add internal/core/parent.go internal/core/parent_test.go
git commit -m "[orchestration] feat: escalate to nearest live ancestor"
```

---

### Task 5: Keep one unresolved ask per handoff across a retarget

**Files:**
- Modify: `internal/core/parent.go:623-634` (`pendingAttention`)
- Test: `internal/core/parent_test.go`

**Interfaces:**
- Consumes: `AncestorChain` (Task 1), `pendingAttention(parentID, handoffID string) *ParentMessage` (`parent.go:623`).
- Produces: `pendingAttention` gains chain awareness while keeping its signature, so `RouteChildEvent`'s idle-coalescing branch (`parent.go:772-779`) is unchanged.

Why: `DeliverPending` documents the invariant "at most one unresolved ask per child handoff" (`parent.go:672-674`). `pendingAttention` only scans **one** parent's directory. After Task 4 a laptop that sleeps (retarget to the root) and wakes (retarget back) could otherwise leave two live asks for one question.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/parent_test.go`:

```go
func TestPendingAttentionFindsAskHeldByAnAncestor(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	root := &Session{
		ID: "sess-root", HostID: LocalHostID,
		Persist:   ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"},
		Labels:    map[string]string{"role": ParentRole, "wake_mode": "notify"},
		CreatedAt: now,
	}
	manager := &Session{
		ID: "sess-manager", HostID: "c1",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "manager"},
		SourceSessionID: root.ID, CreatedAt: now,
	}
	for _, sess := range []*Session{root, manager} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	// An unresolved ask for this handoff is already held by the ROOT,
	// because the manager was disconnected when it was raised.
	held := &ParentMessage{
		V: 1, ID: "pm-held", ParentSessionID: root.ID, ChildSessionID: "sess-child",
		HandoffID: "ho-worker", Kind: "ask", State: ParentMessagePending,
		IntendedParentSessionID: manager.ID, CreatedAt: now,
	}
	if err := writeParentMessage(held, true); err != nil {
		t.Fatal(err)
	}

	got := service.pendingAttention(manager.ID, "ho-worker")
	if got == nil {
		t.Fatal("want the ancestor-held ask to be found from the manager")
	}
	if got.ID != "pm-held" {
		t.Fatalf("want pm-held, got %s", got.ID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestPendingAttentionFindsAskHeldByAnAncestor -v`
Expected: FAIL — `want the ancestor-held ask to be found from the manager` (it only scans the manager's own directory)

- [ ] **Step 3: Write minimal implementation**

In `internal/core/parent.go`, replace `pendingAttention` (`parent.go:623-634`) with:

```go
// pendingAttention finds an existing unresolved ask for this handoff. It scans
// the given parent AND its ancestors, because an escalation raised while this
// parent was disconnected is held by whichever ancestor received it. Without
// the chain scan a reconnecting parent would raise a second ask for one
// question, breaking the one-unresolved-ask-per-handoff invariant.
func (p *ParentService) pendingAttention(parentID, handoffID string) *ParentMessage {
	holders := []string{parentID}
	for _, ancestor := range AncestorChain(p.Reg, parentID) {
		holders = append(holders, ancestor.ID)
	}
	for _, holder := range holders {
		messages, err := p.ListMessages(holder, true)
		if err != nil {
			continue
		}
		for _, msg := range messages {
			if msg.HandoffID == handoffID && attentionMessage(msg.Kind) {
				return msg
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestPendingAttention -v`
Expected: PASS

- [ ] **Step 5: Run the full suite for regressions**

Run: `cd ~/dev/relay && go test ./...`
Expected: all packages `ok`

- [ ] **Step 6: Commit**

```bash
cd ~/dev/relay
git add internal/core/parent.go internal/core/parent_test.go
git commit -m "[orchestration] fix: keep one unresolved ask across failover"
```

---

### Task 6: Attribute the resolver and document the behaviour

**Files:**
- Modify: `internal/core/parent.go:964-1007` (`Reply`), `internal/core/parent.go:1008` (`Ack`)
- Modify: `README.md:107-111` (the lineage/management-tree paragraph)
- Test: `internal/core/parent_test.go`

**Interfaces:**
- Consumes: `ResolvedBySessionID` (Task 2); `Reply(ctx context.Context, messageID, text string) (*ParentMessage, error)` (`parent.go:964`).
- Produces: resolved messages carry `ResolvedBySessionID`, completing the audit trail ("who *should* have handled this, and who actually did").

Note on authorization: `authorizeParentCaller` (`internal/cli/app.go:2169`) compares the caller against `msg.ParentSessionID`, and `cmdResolve` (`app.go:1959`) calls it with exactly that. Because Task 4 sets `ParentSessionID` to the ancestor that actually received the envelope, the resolving ancestor authorizes naturally — **no CLI authorization change is required.** Do not widen `authorizeParentCaller`; widening it would weaken the strict-tree guarantee.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/parent_test.go`:

```go
func TestReplyRecordsTheResolvingSession(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	root := &Session{
		ID: "sess-root", HostID: LocalHostID,
		Persist:   ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"},
		Labels:    map[string]string{"role": ParentRole, "wake_mode": "notify"},
		CreatedAt: now,
	}
	child := &Session{
		ID: "sess-child", HostID: "c3",
		Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now,
	}
	for _, sess := range []*Session{root, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{
		ID: "ho-worker", SessionID: child.ID, HostID: child.HostID,
		Kind: KindAgent, Status: StatusNeedsInput, SourceSessionID: root.ID, CreatedAt: now,
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	msg := &ParentMessage{
		V: 1, ID: "pm-resolve", ParentSessionID: root.ID, ChildSessionID: child.ID,
		HandoffID: ho.ID, Kind: "ask", State: ParentMessagePending,
		IntendedParentSessionID: "sess-manager", CreatedAt: now,
	}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}

	resolved, err := service.Reply(context.Background(), msg.ID, "approved")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ResolvedBySessionID != root.ID {
		t.Fatalf("want the root recorded as resolver, got %q", resolved.ResolvedBySessionID)
	}
	if resolved.State != ParentMessageReplied {
		t.Fatalf("want replied state, got %s", resolved.State)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestReplyRecordsTheResolvingSession -v`
Expected: FAIL — `want the root recorded as resolver, got ""`

- [ ] **Step 3: Write minimal implementation**

In `internal/core/parent.go`, inside `Reply`, set the resolver at the same place the state becomes `ParentMessageReplied` (alongside the existing `msg.Reply = ...` / `msg.RepliedAt = ...` assignments):

```go
	msg.ResolvedBySessionID = msg.ParentSessionID
```

Do the same inside `Ack` where it sets `ParentMessageAcked`:

```go
	msg.ResolvedBySessionID = msg.ParentSessionID
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/relay && go test ./internal/core/ -run TestReplyRecordsTheResolvingSession -v`
Expected: PASS

- [ ] **Step 5: Update the README**

In `README.md`, replace the paragraph at lines 107-111 that currently reads:

```
The lineage is a strict management tree. A child can address only its
authenticated immediate parent. Remote parents are agent managers: they
resolve or escalate one level. Only a local cmux root receives human-facing
notifications, so descendants cannot bypass their manager and interrupt the
human directly.
```

with:

```
The lineage is a strict management tree. A child can address only its
authenticated immediate parent. Remote parents are agent managers: they
resolve or escalate one level. Only a local cmux root receives human-facing
notifications, so descendants cannot bypass their manager and interrupt the
human directly.

Escalation is delivered to the nearest **live** ancestor. A manager that
cannot receive the envelope — laptop asleep, cmux quit, SSH dropped — is
passed over so the child never stalls on a sleeping manager. A live manager
is never skipped, so this adds resilience without weakening the tree: the
envelope still travels the lineage, and the skipped manager is recorded on
the message (`intended_parent_session_id`) and sees the question already
resolved when it returns.
```

- [ ] **Step 6: Run the full suite**

Run: `cd ~/dev/relay && go test ./...`
Expected: all packages `ok`

- [ ] **Step 7: Commit**

```bash
cd ~/dev/relay
git add internal/core/parent.go internal/core/parent_test.go README.md
git commit -m "[orchestration] feat: attribute escalation resolver"
```

---

## Self-Review

**Spec coverage** (against `docs/superpowers/specs/2026-08-01-relay-autonomous-A-nearest-live-ancestor-design.md`):

| Spec decision | Task |
|---|---|
| 1. Delivery target = nearest live ancestor | 4 |
| 2. Liveness = delivery outcome; bounded attempts | 3, 4 |
| 3. Invariant enforced in the walk (never skip a live manager) | 4 (`TestEscalationNeverSkipsALiveManager`) |
| 4. Reconnect reconciliation — notify, don't re-ask | 5 (chain-scoped dedup) + 6 (resolver attribution) |
| 5. Reply path generalizes | 6 — resolved as a **no-op**: `ParentSessionID` *is* the resolver, so `authorizeParentCaller` already passes |
| 6. Grace window before failover | **Deliberately deferred** — see below |
| 7. Target-independent identity / chain-scoped dedup | 4 (resolve-before-allocate + retarget), 5 (`pendingAttention`) |
| Data changes (3 additive fields) | 2 |
| Testing section | 1, 3, 4, 5, 6 |

**Deliberate deferrals, with rationale:**
- **Grace window (spec decision 6).** Task 3's bounded attempt already prevents a hang, and `deliverMessage` is naturally retried by `DeliverPending` on rebind. A separate grace window adds a timer and a second retry path for a case (transient blip *during* an escalation) that the 5s bound largely covers. Ship A without it; add it if real use shows premature failover.
- **`applyPolicy` chain-scoped seen/pending (spec decision 7, third bullet).** The built-in coalescing rules degrade *safely* across a retarget (they may fail to collapse a duplicate idle sample, never auto-approve something they shouldn't), and Task 5 already prevents the duplicate-ask case that matters. Left out to keep A small.

Both deferrals are recorded in the spec's Open Questions rather than silently dropped.

**Placeholder scan:** none — every step has runnable code or an exact command.

**Type consistency:** `AncestorChain(reg *Registry, startSessionID string) []*Session` is defined in Task 1 and used with that exact signature in Tasks 4 and 5. `IntendedParentSessionID` / `SkippedSessionIDs` / `ResolvedBySessionID` are defined in Task 2 and used with those exact names in Tasks 4, 5, and 6. `deliveryCandidates` and `retargetMessage` are both defined and used in Task 4. `deliveryAttemptTimeout` is defined in Task 3 and used there only.
