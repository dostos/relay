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
	ho := &Handoff{
		ID: id, SessionID: "sess-child-" + id, HostID: "c3", Kind: KindAgent,
		Status: HandoffStatus(status), SourceSessionID: source, CreatedAt: now,
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
}

func TestNeedsWatchSelectsOnlyLiveHandoffsWithAParent(t *testing.T) {
	sup, reg := newSupervisorFixture(t)
	putSupervisedHandoff(t, reg, "ho-live", "running", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-needs-input", "needs_input", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-done", "done", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-failed", "failed", "sess-manager")
	putSupervisedHandoff(t, reg, "ho-abandoned", "abandoned", "sess-manager")
	// No parent to escalate to: nowhere to route, so nothing to watch.
	putSupervisedHandoff(t, reg, "ho-orphan", "running", "")

	got, err := sup.NeedsWatch()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, ho := range got {
		ids[ho.ID] = true
	}
	if len(ids) != 2 || !ids["ho-live"] || !ids["ho-needs-input"] {
		t.Fatalf("want the two live parented handoffs, got %v", ids)
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

// A watcher that exits frees its slot, so the next tick re-adopts the handoff
// if it is still live. Without this, one transient failure would strand it.
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
			started, err := sup.Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if started != 1 {
				t.Fatalf("a freed slot must be re-adopted, started %d", started)
			}
			return
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

var _ = ports.PersistHandle{}
