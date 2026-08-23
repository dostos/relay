package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

type attachProjectionViz struct {
	ports.Viz
	panes []ports.ProjectedSession
}

func (v attachProjectionViz) ProjectionSessions(context.Context) ([]ports.ProjectedSession, error) {
	return v.panes, nil
}

// TestAttachUsesProjectionWhenLocalAuthorityUnavailable covers the exact
// failure the user hit: `relay session attach ID` on a `.viz-projection-only`
// client (e.g. a Mac whose durable authority lives on a home host) errored
// with ErrProjectionOnlyAuthority instead of attaching, because Attach only
// ever consulted the (deliberately unavailable) local registry. It must fall
// back to the live viz projection inventory, the same way `session list`
// already does.
func TestAttachUsesProjectionWhenLocalAuthorityUnavailable(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var attachedHost string
	transport := &fakeTransport{id: "hamburg"}
	svc := &SessionService{
		Reg:     &Registry{},
		Persist: resumePersist{},
		NewTransport: func(host string) (ports.Transport, error) {
			attachedHost = host
			return transport, nil
		},
		Viz: attachProjectionViz{panes: []ports.ProjectedSession{
			{SessionID: "sess-engram", TmuxName: "engram", Target: "hamburg", Surface: "surface:9"},
		}},
	}
	if _, err := svc.Reg.GetSession("sess-engram"); !errors.Is(err, ErrProjectionOnlyAuthority) {
		t.Fatalf("expected local authority to fail closed first, got %v", err)
	}
	if err := svc.Attach(context.Background(), "sess-engram"); err != nil {
		t.Fatalf("Attach failed: %v", err)
	}
	if attachedHost != "hamburg" {
		t.Fatalf("attached host = %q, want hamburg", attachedHost)
	}
}

func TestAttachStillFailsClosedWithoutProjectionSupport(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &SessionService{Reg: &Registry{}, Persist: resumePersist{}, NewTransport: func(string) (ports.Transport, error) {
		return &fakeTransport{}, nil
	}}
	if err := svc.Attach(context.Background(), "sess-unknown"); !errors.Is(err, ErrProjectionOnlyAuthority) {
		t.Fatalf("expected fail-closed without a projection-capable Viz, got %v", err)
	}
}

// TestOtherReadOnlySessionOpsAlsoUseProjectionFallback covers the
// generalization requested after the Attach fix: every read-only
// SessionService operation a projection client might invoke directly (not
// just Attach) must fall back to the live viz projection inventory rather
// than failing closed on the (deliberately unavailable) local registry.
func TestOtherReadOnlySessionOpsAlsoUseProjectionFallback(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newSvc := func() *SessionService {
		return &SessionService{
			Reg:     &Registry{},
			Persist: &renamePersistence{},
			NewTransport: func(host string) (ports.Transport, error) {
				return &fakeTransport{id: host, outputs: map[string]string{host: "ok"}}, nil
			},
			Viz: attachProjectionViz{panes: []ports.ProjectedSession{
				{SessionID: "sess-engram", TmuxName: "engram", Target: "hamburg", Surface: "surface:9"},
			}},
		}
	}
	ctx := context.Background()

	if sess, err := newSvc().Get("sess-engram"); err != nil || sess.HostID != "hamburg" {
		t.Fatalf("Get: sess=%v err=%v", sess, err)
	}
	if out, err := newSvc().Capture(ctx, "sess-engram", 10); err != nil || out == "" {
		t.Fatalf("Capture: out=%q err=%v", out, err)
	}
	if ok, err := newSvc().Exists(ctx, "sess-engram"); err != nil || !ok {
		t.Fatalf("Exists: ok=%v err=%v", ok, err)
	}
	if err := newSvc().Send(ctx, "sess-engram", "hello", true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, _, err := newSvc().Exec(ctx, "sess-engram", "true"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := newSvc().Resize(ctx, "sess-engram"); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if cmd, err := newSvc().AttachCommand("sess-engram"); err != nil {
		t.Fatalf("AttachCommand: cmd=%q err=%v", cmd, err)
	}
}
