package core

import (
	"context"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

// fakePersist records whether the tmux path was taken.
type fakePersist struct {
	ports.Persistence
	calls int
}

func (f *fakePersist) Capture(_ context.Context, _ ports.Transport, _ ports.PersistHandle, _ int) (string, error) {
	f.calls++
	return "tmux-text", nil
}

func captureFixture(t *testing.T) (*SessionService, *fakePersist, *Session) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	sess := &Session{
		ID: "sess-cap", HostID: LocalHostID,
		Persist: ports.PersistHandle{Kind: "tmux", Name: "beholder-pdf-main"},
	}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	fp := &fakePersist{}
	svc := &SessionService{
		Reg:     reg,
		Persist: fp,
		NewTransport: func(string) (ports.Transport, error) {
			return nil, nil
		},
	}
	return svc, fp, sess
}

// Every session is a tmux session now — relay has no presenter-owned panes —
// so capture always goes through the persistence adapter.
func TestCaptureUsesPersistence(t *testing.T) {
	svc, fp, _ := captureFixture(t)
	got, err := svc.Capture(context.Background(), "sess-cap", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tmux-text" {
		t.Fatalf("want the tmux capture, got %q", got)
	}
	if fp.calls != 1 {
		t.Fatalf("tmux path must be used exactly once, got %d", fp.calls)
	}
}
