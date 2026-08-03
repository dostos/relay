package core

import (
	"context"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type managedSendPersist struct {
	ports.Persistence
	text  string
	enter bool
}

func (p *managedSendPersist) Send(_ context.Context, _ ports.Transport, _ ports.PersistHandle, text string, enter bool) error {
	p.text, p.enter = text, enter
	return nil
}

func TestManagedChildSendRequiresImmediateEdgeAndObservableEvents(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	child := &Session{ID: "sess-child", HostID: "c3", SourceSessionID: "sess-manager", Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"}, CreatedAt: now}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	persist := &managedSendPersist{}
	svc := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "c3"}, nil }}
	if _, err := svc.SendManagedChild(context.Background(), "sess-other", child.ID, "review", false); err == nil {
		t.Fatal("non-parent manager was allowed to send")
	}
	receipt, err := svc.SendManagedChild(context.Background(), "sess-manager", child.ID, "review", false)
	if err == nil || receipt == nil || receipt.EventStream != "absent" || persist.text != "" {
		t.Fatalf("submitted-with-zero-events was not stopped: receipt=%+v text=%q err=%v", receipt, persist.text, err)
	}
	if err := reg.PutHandoff(&Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: "sess-manager", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	receipt, err = svc.SendManagedChild(context.Background(), "sess-manager", child.ID, "review", false)
	if err != nil || receipt.EventStream != "active" || receipt.HandoffID != "ho-child" || !receipt.Submitted {
		t.Fatalf("observable delivery receipt=%+v err=%v", receipt, err)
	}
	if persist.text != "review" || !persist.enter {
		t.Fatalf("delivery=%q enter=%v", persist.text, persist.enter)
	}
}

func TestManagedChildDeliveryOnlyMakesMissingEventsExplicit(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	child := &Session{ID: "sess-child", HostID: "c3", SourceSessionID: "sess-manager", Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"}, CreatedAt: now}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	persist := &managedSendPersist{}
	svc := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "c3"}, nil }}
	receipt, err := svc.SendManagedChild(context.Background(), "sess-manager", child.ID, "delivery only", true)
	if err != nil || receipt == nil || !receipt.Submitted || receipt.EventStream != "absent" || persist.text != "delivery only" {
		t.Fatalf("receipt=%+v text=%q err=%v", receipt, persist.text, err)
	}
}

func TestGovernedChildWithoutHandoffFailsDiagnostic(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	child := &Session{ID: "sess-child", HostID: "c3", SourceSessionID: "sess-manager", Labels: map[string]string{"governed": "true"}, Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"}, CreatedAt: now}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	svc := &SessionService{Reg: reg}
	missing, err := svc.UnobservableGovernedChildren()
	if err != nil || len(missing) != 1 || missing[0] != child.ID {
		t.Fatalf("missing=%v err=%v", missing, err)
	}
	if err := reg.PutHandoff(&Handoff{ID: "ho-child", SessionID: child.ID, SourceSessionID: child.SourceSessionID, Status: StatusRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	missing, err = svc.UnobservableGovernedChildren()
	if err != nil || len(missing) != 0 {
		t.Fatalf("observable child still flagged: %v err=%v", missing, err)
	}
}
