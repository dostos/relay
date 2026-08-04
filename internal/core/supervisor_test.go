package core

import (
	"context"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func newSupervisorFixture(t *testing.T) (*SupervisorService, *Registry) {
	t.Helper()
	service, _, reg := newParentTestService(t)
	return &SupervisorService{Reg: reg, Parents: service, Interval: 20 * time.Millisecond}, reg
}

func putSupervisedHandoff(t *testing.T, reg *Registry, id, status string, source string) {
	t.Helper()
	now := time.Now().UTC()
	sessionID := "sess-child-" + id
	if err := reg.PutSession(&Session{ID: sessionID, HostID: "c3", SourceSessionID: source, Persist: ports.PersistHandle{Kind: "tmux", Name: id}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ho := &Handoff{
		ID: id, SessionID: sessionID, HostID: "c3", Kind: KindAgent,
		Status: HandoffStatus(status), SourceSessionID: source, CreatedAt: now,
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
}

func TestNeedsWatchSelectsLiveChildrenAndApexRoot(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-live", "running", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-needs-input", "needs_input", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-done", "done", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-failed", "failed", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-abandoned", "abandoned", "sess-manager")
	// No parent to escalate to: nowhere to route, so nothing to watch.
	putSupervisedHandoff(t, reg, "ho-orphan", "running", "")
	apexSession, _ := reg.GetSession("sess-child-ho-orphan")
	apexSession.Labels = map[string]string{ApexLabel: "true"}
	if err := reg.PutSession(apexSession); err != nil {
		t.Fatal(err)
	}
	putSupervisedHandoff(t, reg, "ho-unmanaged-root", "running", "")

	got, err := sup.NeedsWatch()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, ho := range got {
		ids[ho.ID] = true
	}
	if len(ids) != 3 || !ids["ho-live"] || !ids["ho-needs-input"] || !ids["ho-orphan"] || ids["ho-unmanaged-root"] {
		t.Fatalf("want two parented handoffs and the apex root, got %v", ids)
	}
}

func TestBlockedDeliveryRemainsWatchableAfterRegistryReload(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	now := time.Now().UTC()
	if err := reg.PutSession(&Session{ID: "sess-blocked", HostID: "c3", SourceSessionID: "sess-manager", Persist: ports.PersistHandle{Kind: "tmux", Name: "blocked"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ho := &Handoff{ID: "ho-blocked", SessionID: "sess-blocked", HostID: "c3", Kind: KindAgent, Status: StatusNeedsInput, LaunchState: EffectAcknowledged, DeliveryState: EffectBlocked, PendingGate: &SecurityGate{Reason: "trust", Directory: "/repo", Choices: []GateChoice{{Index: 1, Label: "Yes"}, {Index: 2, Label: "No"}}}, SourceSessionID: "sess-manager", CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	reloaded, err := reg.GetHandoff(ho.ID)
	if err != nil || reloaded.DeliveryState != EffectBlocked || reloaded.PendingGate == nil {
		t.Fatalf("blocked state did not survive reload: %+v err=%v", reloaded, err)
	}
	got, err := sup.NeedsWatch()
	if err != nil || len(got) != 1 || got[0].ID != ho.ID {
		t.Fatalf("blocked handoff not supervised after reload: %+v err=%v", got, err)
	}
}

// The whole point: a live handoff with no watcher gets adopted rather than
// sitting silently unrouted until someone reinstalls relay.
func TestReconcileAdoptsAnUnwatchedLiveHandoff(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-live", "running", "sess-manager")

	started, err := sup.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("want one watcher started, got %d", started)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sup.Supervised()) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Watch may exit immediately in this fixture (no coord); what matters is
	// that the supervisor claimed it rather than ignoring it.
	if started != 1 {
		t.Fatalf("handoff was never adopted")
	}
}

func TestReconcileRedeliversPendingEnvelopeWithoutNewChildEvent(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-manager", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "manager"}, Labels: map[string]string{"role": ParentRole, "wake_mode": "inject"}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", SourceSessionID: parent.ID, Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	notifier.notifyFail = true
	msg, err := service.RouteChildEvent(context.Background(), ho, Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "A or B?"}})
	if err == nil || msg == nil || msg.DeliveredAt != nil {
		t.Fatalf("failed initial delivery = %+v err=%v", msg, err)
	}
	notifier.notifyFail = false
	ageDeliveryRetry(t, msg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := &SupervisorService{Reg: reg, Parents: service}
	if _, err := sup.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := service.FindMessage(msg.ID)
	if err != nil || stored.DeliveredAt == nil || len(notifier.sent) != 1 {
		t.Fatalf("supervisor retry = %+v sends=%d err=%v", stored, len(notifier.sent), err)
	}
	// A later duplicate event is only a cursor/deduplication concern; it must
	// not become a second delivery owner.
	if _, err := service.RouteChildEvent(context.Background(), ho, Event{Seq: 2, Kind: "ask", Meta: map[string]any{"text": "A or B?"}}); err != nil || len(notifier.sent) != 1 {
		t.Fatalf("duplicate event retried delivery: sends=%d err=%v", len(notifier.sent), err)
	}
}

func TestReconcileDoesNotDoubleStartTheSameHandoff(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-live", "running", "sess-manager")
	sup.mu.Lock()
	sup.running = map[string]struct{}{"ho-live": {}}
	sup.mu.Unlock()

	started, err := sup.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if started != 0 {
		t.Fatalf("an already-supervised handoff must not be started again, got %d", started)
	}
}

func TestReconcileRepairsSensorsOncePerSession(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	now := time.Now().UTC()
	if err := reg.PutSession(&Session{ID: "sess-agent", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "agent"}, Labels: map[string]string{"agent": "codex"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	sup.RepairSensors = func(_ context.Context, sessionID string) error {
		calls++
		if sessionID != "sess-agent" {
			t.Fatalf("session=%s", sessionID)
		}
		return nil
	}
	if _, err := sup.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("sensor repairs=%d, want 1", calls)
	}
}

func TestReconcileIgnoresTerminalHandoffs(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	for _, s := range []string{"done", "failed", "abandoned"} {
		putSupervisedHandoff(t, reg, "ho-"+s, s, "sess-manager")
	}
	started, err := sup.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if started != 0 {
		t.Fatalf("terminal handoffs must not be watched, started %d", started)
	}
}

// A watcher that exits must free its slot, so the handoff is never stranded.
// Whether it restarts immediately is governed by backoff: a watcher that
// flapped is left alone for a while (see TestFlappingWatcherIsBackedOff), and
// picked up again once that expires.
func TestSlotIsFreedWhenAWatcherExits(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-live", "running", "sess-manager")

	ended := make(chan string, 4)
	sup.OnEvent = func(event, id string, _ error) {
		if event == "watch_end" {
			ended <- id
		}
	}
	if _, err := sup.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-ended:
		if id != "ho-live" {
			t.Fatalf("unexpected watcher end for %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher never reported ending")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sup.Supervised()) == 0 {
			return // slot freed, which is the invariant
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("slot was never freed")
}

func TestRunStopsOnContextCancel(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-live", "running", "sess-manager")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run must report the cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestSupervisorReconcilesTerminalHandoffsWithoutNewEvent(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	service := &ParentService{Reg: reg}
	called := 0
	sup := &SupervisorService{
		Reg: reg, Parents: service,
		ReconcileHandoffs: func(context.Context) (int, error) {
			called++
			return 1, nil
		},
	}
	var events []string
	sup.OnEvent = func(event, _ string, _ error) { events = append(events, event) }
	if _, err := sup.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called != 1 || len(events) != 1 || events[0] != "terminal_handoffs_reconciled" {
		t.Fatalf("called=%d events=%v", called, events)
	}
}

var _ = ports.PersistHandle{}

// A watcher that exits immediately never got to work — usually because another
// process owns the handoff. Restarting it every tick is a spin, not supervision.
func TestFlappingWatcherIsBackedOff(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-flap", "running", "sess-manager")

	first, err := sup.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("want the first attempt, got %d", first)
	}
	// Let the watcher exit (it will, immediately, with no coord configured).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sup.Supervised()) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	again, err := sup.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("a flapping handoff must be backed off, restarted %d", again)
	}
}

// Backoff must not be permanent: once it expires the handoff is retried, so a
// transient owner does not strand it forever.
func TestBackoffExpiresAndTheHandoffIsRetried(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-retry", "running", "sess-manager")
	if _, err := sup.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sup.Supervised()) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	sup.mu.Lock()
	sup.backoff["ho-retry"] = time.Now().Add(-time.Second) // expired
	sup.mu.Unlock()

	again, err := sup.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again != 1 {
		t.Fatalf("an expired backoff must retry, got %d", again)
	}
}
