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

func TestManagedChildSendRequiresImmediateEdgeAndSubmits(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	child := &Session{ID: "sess-child", HostID: "c3", SourceSessionID: "sess-manager", Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"}, CreatedAt: now}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	persist := &managedSendPersist{}
	svc := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "c3"}, nil }}
	if err := svc.SendManagedChild(context.Background(), "sess-other", child.ID, "review"); err == nil {
		t.Fatal("non-parent manager was allowed to send")
	}
	if err := svc.SendManagedChild(context.Background(), "sess-manager", child.ID, "review"); err != nil {
		t.Fatal(err)
	}
	if persist.text != "review" || !persist.enter {
		t.Fatalf("delivery=%q enter=%v", persist.text, persist.enter)
	}
}
