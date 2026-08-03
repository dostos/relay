package core

import (
	"context"
	"strings"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

// fakeScreen is a Viz that can read pane text, like cmux. The embedded
// interface is nil: only the methods the test exercises are implemented, so an
// unexpected call panics loudly instead of passing silently.
type fakeScreen struct {
	ports.Viz
	text    string
	gotID   string
	gotLine int
	calls   int
	sent    string
}

func (f *fakeScreen) CaptureScreen(_ context.Context, sessionID string, lines int) (string, error) {
	f.calls++
	f.gotID, f.gotLine = sessionID, lines
	return f.text, nil
}
func (f *fakeScreen) SendScreen(_ context.Context, _ string, text string, _ bool) error {
	f.sent = text
	return nil
}

// plainViz has no screen-reading capability, like a headless adapter.
type plainViz struct{ ports.Viz }

// fakePersist records whether the tmux path was taken.
type fakePersist struct {
	ports.Persistence
	calls int
}

func TestSendToDesktopSessionUsesScreenCapability(t *testing.T) {
	svc, fp, _ := captureFixture(t, LocalPersistKind)
	screen := &fakeScreen{}
	svc.Screen = screen
	if err := svc.Send(context.Background(), "sess-cap", "manager notice", true); err != nil {
		t.Fatal(err)
	}
	if screen.sent != "manager notice" || fp.calls != 0 {
		t.Fatalf("screen=%q persist_calls=%d", screen.sent, fp.calls)
	}
}

func (f *fakePersist) Capture(_ context.Context, _ ports.Transport, _ ports.PersistHandle, _ int) (string, error) {
	f.calls++
	return "tmux-text", nil
}

func captureFixture(t *testing.T, kind string) (*SessionService, *fakePersist, *Session) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	sess := &Session{
		ID: "sess-cap", HostID: LocalHostID,
		Persist: ports.PersistHandle{Kind: kind, Name: "beholder-pdf-main"},
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

// A cmux-backed session has no tmux server behind it. Capturing it through the
// persistence adapter fails with "no server running", which is what made every
// local root pane unreadable — an apex governing subtrees it could not observe.
func TestCaptureOfACmuxSessionReadsThePaneNotTmux(t *testing.T) {
	svc, fp, _ := captureFixture(t, LocalPersistKind)
	screen := &fakeScreen{text: "pane contents"}
	svc.Screen = screen

	got, err := svc.Capture(context.Background(), "sess-cap", 12)
	if err != nil {
		t.Fatal(err)
	}
	if got != "pane contents" {
		t.Fatalf("want the pane text, got %q", got)
	}
	if fp.calls != 0 {
		t.Fatal("a cmux session must not be captured through tmux")
	}
	if screen.gotID != "sess-cap" || screen.gotLine != 12 {
		t.Fatalf("capture args not forwarded: id=%q lines=%d", screen.gotID, screen.gotLine)
	}
}

// tmux sessions must keep working exactly as before.
func TestCaptureOfATmuxSessionStillUsesPersistence(t *testing.T) {
	svc, fp, _ := captureFixture(t, "tmux")
	screen := &fakeScreen{text: "pane contents"}
	svc.Screen = screen

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
	if screen.calls != 0 {
		t.Fatal("a tmux session must not be read through the viz")
	}
}

// If the viz cannot read screens, say so plainly rather than falling through to
// tmux and reporting "no server running", which names the wrong subsystem.
func TestCaptureOfACmuxSessionWithoutAScreenReaderExplainsItself(t *testing.T) {
	svc, _, _ := captureFixture(t, LocalPersistKind)
	svc.Screen = nil

	_, err := svc.Capture(context.Background(), "sess-cap", 5)
	if err == nil {
		t.Fatal("want an error naming the real limitation")
	}
	if !strings.Contains(err.Error(), "cmux") {
		t.Fatalf("the error must name the adapter that cannot capture, got %q", err)
	}
}
