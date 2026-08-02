package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type sensorRecordingPersistence struct {
	renamePersistence
	handle  ports.PersistHandle
	command string
}

type gatePersistence struct {
	renamePersistence
	capture string
	sent    []string
}

func (p *gatePersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return p.capture, nil
}

func (p *gatePersistence) Send(_ context.Context, _ ports.Transport, _ ports.PersistHandle, text string, _ bool) error {
	p.sent = append(p.sent, text)
	return nil
}

func (p *sensorRecordingPersistence) InstallSensors(_ context.Context, _ ports.Transport, handle ports.PersistHandle, _ int, factory func(string) (string, error)) error {
	p.handle = handle
	p.command, _ = factory("idle")
	return nil
}

func TestHandoffLayoutPreservesSourcePlacement(t *testing.T) {
	layout := handoffLayout(HandoffOpts{Workspace: "workspace:9", Pane: "pane:12", SourceSessionID: "sess-parent"})

	if layout.Mode != "remote" {
		t.Fatalf("mode = %q, want remote", layout.Mode)
	}
	if layout.Workspace != "workspace:9" {
		t.Fatalf("workspace = %q, want workspace:9", layout.Workspace)
	}
	if layout.Pane != "pane:12" || layout.SourceSessionID != "sess-parent" {
		t.Fatalf("source placement lost: %+v", layout)
	}
}

func TestAgentGoalPromptTeachesExplicitAsk(t *testing.T) {
	prompt := agentGoalPrompt("recover the tables")
	if !strings.HasPrefix(prompt, "recover the tables\n") {
		t.Fatalf("goal changed: %q", prompt)
	}
	if !strings.Contains(prompt, `relay ask "<question>"`) {
		t.Fatalf("explicit ask instruction missing: %q", prompt)
	}
}

func TestReinstallSensorsAfterRenameKeepsWatcherEventStream(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	reg := &Registry{}
	sess := &Session{ID: "sess-1", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "new-name"}, CreatedAt: now}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(&Handoff{ID: "ho-1", SessionID: sess.ID, HostID: "self", Name: "original-stream", Status: StatusRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	persist := &sensorRecordingPersistence{}
	coord := newFakeCoord()
	service := &HandoffService{
		Reg: reg, Coord: coord, Persist: persist,
		Sessions:     &SessionService{Reg: reg},
		NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil },
	}
	if err := service.ReinstallSensors(context.Background(), sess.ID, 45); err != nil {
		t.Fatal(err)
	}
	if persist.handle.Name != "new-name" {
		t.Fatalf("sensors installed on %q, want renamed tmux", persist.handle.Name)
	}
	if persist.command != "original-stream:idle" {
		t.Fatalf("sensor command = %q, want watcher stream", persist.command)
	}
}

func TestTrustGateEmitsPermissionWithoutInjectingGoal(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-gated", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "gated"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-gated", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusRunning, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	persist := &gatePersistence{capture: "Do you trust the contents of this directory?\n❯ 1. Yes, continue\n  2. No, quit"}
	coord := newFakeCoord()
	service := &HandoffService{Reg: reg, Coord: coord, Persist: persist}
	if err := service.injectAgentGoal(context.Background(), &fakeTransport{id: "self"}, sess, ho, "change code"); err != nil {
		t.Fatal(err)
	}
	if len(persist.sent) != 0 {
		t.Fatalf("goal was injected into trust gate: %v", persist.sent)
	}
	events := coord.events[sess.Persist.Name]
	if len(events) != 1 || events[0].Kind != "permission_required" || eventText(&events[0]) == "" {
		t.Fatalf("gate event = %+v", events)
	}
	stored, err := reg.GetHandoff(ho.ID)
	if err != nil || stored.Status != StatusNeedsInput {
		t.Fatalf("gated handoff = %+v, err=%v", stored, err)
	}
}

func TestSubscribeRetryStatusShowsStructuredLastError(t *testing.T) {
	status := subscribeRetryStatus("test-host", 2, 3*time.Second, errors.New("ssh stream to test-host: ssh: connect timed out"))
	for _, want := range []string{"waiting test-host", "last error: ssh stream to test-host", "retry 2/6 in 3s"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status %q missing %q", status, want)
		}
	}
}
