package core

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

// fakeTransport implements ports.Transport. Run returns per-host canned tmux
// output; a host absent from outputs is treated as unreachable (error).
type fakeTransport struct {
	id      string
	outputs map[string]string // hostID -> `tmux list-sessions` stdout
}

func (f *fakeTransport) ID() string { return f.id }
func (f *fakeTransport) Run(ctx context.Context, cwd, command string) (string, string, error) {
	out, ok := f.outputs[f.id]
	if !ok {
		return "", "", fmt.Errorf("unreachable host %s", f.id)
	}
	return out, "", nil
}
func (f *fakeTransport) RunStream(ctx context.Context, cwd, command string, w io.Writer) error {
	return nil
}
func (f *fakeTransport) ReadFile(ctx context.Context, path string) ([]byte, error) { return nil, nil }
func (f *fakeTransport) WriteFile(ctx context.Context, path string, data []byte, mode string) error {
	return nil
}
func (f *fakeTransport) Interactive(ctx context.Context, command string) error { return nil }
func (f *fakeTransport) InteractiveCommand(remoteCmd string) string            { return remoteCmd }

func TestReapDead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)

	reg := &Registry{}
	put := func(id, host, persist string) {
		if err := reg.PutSession(&Session{
			ID: id, HostID: host, Persist: ports.PersistHandle{Kind: "tmux", Name: persist},
		}); err != nil {
			t.Fatal(err)
		}
		RememberResume(&Session{ID: id, HostID: host, Persist: ports.PersistHandle{Kind: "tmux", Name: persist}})
	}
	put("sess-alive", "h-ok", "alive-x")
	put("sess-dead", "h-ok", "dead-x")
	put("sess-unreach", "h-down", "unreach-x")

	// h-ok reachable, only alive-x present. h-down absent → unreachable.
	outputs := map[string]string{"h-ok": "alive-x\n"}
	sessions := &SessionService{
		Reg:          reg,
		NewTransport: func(host string) (ports.Transport, error) { return &fakeTransport{id: host, outputs: outputs}, nil },
	}
	h := &HandoffService{Sessions: sessions, Reg: reg} // Viz nil

	res, err := h.ReapDead(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	has := func(xs []string, want string) bool {
		for _, x := range xs {
			if x == want {
				return true
			}
		}
		return false
	}
	if !has(res.KeptAlive, "alive-x") {
		t.Fatalf("alive-x should be kept: %+v", res)
	}
	if !has(res.Reaped, "dead-x") {
		t.Fatalf("dead-x should be reaped: %+v", res)
	}
	if !has(res.Skipped, "unreach-x") {
		t.Fatalf("unreach-x should be skipped (host unreachable): %+v", res)
	}

	// Reaped session row is gone; alive + unreachable rows survive.
	if _, err := reg.GetSession("sess-dead"); err == nil {
		t.Fatal("sess-dead row should be deleted")
	}
	if _, err := reg.GetSession("sess-alive"); err != nil {
		t.Fatalf("sess-alive must survive: %v", err)
	}
	if _, err := reg.GetSession("sess-unreach"); err != nil {
		t.Fatalf("sess-unreach must survive (host was unreachable): %v", err)
	}
	// Reaped name is now a cleaned tombstone so resume refuses cleanly.
	if p, _, _ := reg.ClassifyResume("dead-x"); p != PresenceCleaned {
		t.Fatalf("dead-x presence = %q, want cleaned", p)
	}
}

func TestReapDeadDryRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	reg := &Registry{}
	_ = reg.PutSession(&Session{ID: "s1", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "gone"}})
	RememberResume(&Session{ID: "s1", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "gone"}})
	outputs := map[string]string{"h-ok": ""} // reachable, no tmux
	sessions := &SessionService{Reg: reg, NewTransport: func(host string) (ports.Transport, error) {
		return &fakeTransport{id: host, outputs: outputs}, nil
	}}
	h := &HandoffService{Sessions: sessions, Reg: reg}

	res, err := h.ReapDead(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != "gone" {
		t.Fatalf("dry-run should report 'gone' reapable: %+v", res)
	}
	// Dry run must NOT mutate.
	if _, err := reg.GetSession("s1"); err != nil {
		t.Fatalf("dry-run must not delete session: %v", err)
	}
	if p, _, _ := reg.ClassifyResume("gone"); p == PresenceCleaned {
		t.Fatal("dry-run must not mark cleaned")
	}
}
