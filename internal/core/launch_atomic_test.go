package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

const launchTestProfile = `version: 1
host_id: hamburg
agents:
  - name: cursor-agent
    command: cursor-agent
defaults:
  preferred_agent: cursor-agent
  silence_sec: 1
`

type launchTransport struct{}

func (*launchTransport) ID() string { return "hamburg" }
func (*launchTransport) Run(context.Context, string, string) (string, string, error) {
	return "", "", nil
}
func (*launchTransport) RunStream(context.Context, string, string, io.Writer) error { return nil }
func (*launchTransport) ReadFile(context.Context, string) ([]byte, error) {
	return []byte(launchTestProfile), nil
}
func (*launchTransport) WriteFile(context.Context, string, []byte, string) error { return nil }
func (*launchTransport) Interactive(context.Context, string) error               { return nil }
func (*launchTransport) InteractiveCommand(command string) string                { return command }

type launchPersistence struct {
	renamePersistence
	launchErr, sensorsErr error
	capture               string
	created, destroyed    bool
}

func (p *launchPersistence) Create(_ context.Context, _ ports.Transport, name, _, _ string) (ports.PersistHandle, error) {
	p.created = true
	return ports.PersistHandle{Kind: "tmux", Name: name}, nil
}
func (p *launchPersistence) Launch(context.Context, ports.Transport, ports.PersistHandle, string) error {
	return p.launchErr
}
func (p *launchPersistence) InstallSensors(context.Context, ports.Transport, ports.PersistHandle, int, func(string) (string, error)) error {
	return p.sensorsErr
}
func (p *launchPersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return p.capture, nil
}
func (p *launchPersistence) Destroy(context.Context, ports.Transport, ports.PersistHandle) error {
	p.destroyed = true
	return nil
}

type launchCoord struct {
	*fakeCoord
	emitErr error
}

func (c *launchCoord) Emit(ctx context.Context, t ports.Transport, session, kind string, meta map[string]any) (int64, error) {
	if c.emitErr != nil {
		return 0, c.emitErr
	}
	return c.fakeCoord.Emit(ctx, t, session, kind, meta)
}

type launchViz struct {
	deletionViz
	err      error
	ref      string
	projects []ports.ProjectionEvent
}

func (v *launchViz) ApplyProjection(_ context.Context, event ports.ProjectionEvent) (string, error) {
	v.projects = append(v.projects, event)
	if v.err != nil {
		return "", v.err
	}
	if v.ref != "" {
		return v.ref, nil
	}
	return "surface:test", nil
}

func newAtomicLaunchService(t *testing.T, reg *Registry, persist *launchPersistence) (*HandoffService, *launchCoord) {
	t.Helper()
	transport := &launchTransport{}
	profiles := &ProfileService{NewTransport: func(string) (ports.Transport, error) { return transport, nil }}
	sessions := &SessionService{
		Reg: reg, Profiles: profiles, Persist: persist,
		NewTransport: func(string) (ports.Transport, error) { return transport, nil },
	}
	coord := &launchCoord{fakeCoord: newFakeCoord()}
	return &HandoffService{
		Reg: reg, Profiles: profiles, Sessions: sessions, Persist: persist, Coord: coord,
		NewTransport: func(string) (ports.Transport, error) { return transport, nil },
	}, coord
}

func TestPrePersistenceRejectionReturnsDurableAttempt(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	service := &HandoffService{Reg: reg}
	resp, err := service.AgentStart(context.Background(), HandoffOpts{Agent: "cursor-agent", Goal: "goal"})
	if err == nil || resp == nil || resp.AttemptID == "" || resp.HandoffID != resp.AttemptID || resp.Extra["failure_stage"] != "validate" {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	stored, getErr := reg.GetHandoff(resp.AttemptID)
	if getErr != nil || stored.Status != StatusFailed || stored.LaunchState != EffectFailed || !stored.RetrySafe {
		t.Fatalf("durable attempt=%+v err=%v", stored, getErr)
	}
	raw, marshalErr := json.Marshal(resp)
	if marshalErr != nil || len(raw) == 0 || !strings.Contains(string(raw), `"attempt_id":"`+resp.AttemptID+`"`) || !strings.Contains(string(raw), `"failure_stage":"validate"`) {
		t.Fatalf("structured failure=%s err=%v", raw, marshalErr)
	}
}

func TestTransportFailureCleansPartialSessionAndReturnsIDs(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg, persist := &Registry{}, &launchPersistence{}
	service, _ := newAtomicLaunchService(t, reg, persist)
	service.NewTransport = func(string) (ports.Transport, error) { return nil, errors.New("transport blank exit") }
	resp, err := service.AgentStart(context.Background(), HandoffOpts{HostID: "hamburg", RemoteCWD: "/tmp", Command: "true", Name: "blank-launch", NoPane: true})
	if err == nil || resp == nil || resp.AttemptID == "" || resp.SessionID == "" || resp.Extra["failure_stage"] != "transport" {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	if !persist.destroyed {
		t.Fatal("partial session was not cleaned")
	}
	if _, getErr := reg.GetSession(resp.SessionID); getErr == nil {
		t.Fatal("partial session survived in authority")
	}
}

func TestBlankLauncherFailureIsDurableAndCleaned(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg, persist := &Registry{}, &launchPersistence{launchErr: errors.New("launcher returned blank acknowledgement")}
	service, coord := newAtomicLaunchService(t, reg, persist)
	resp, err := service.AgentStart(context.Background(), HandoffOpts{HostID: "hamburg", RemoteCWD: "/tmp", Command: "true", Name: "blank-launch", NoPane: true})
	if err == nil || resp.Extra["failure_stage"] != "launch" || resp.Extra["launch_state"] != EffectFailed || !persist.destroyed {
		t.Fatalf("response=%+v destroyed=%v err=%v", resp, persist.destroyed, err)
	}
	if events := coord.events["blank-launch"]; len(events) != 1 || events[0].Kind != "result" || events[0].Meta["source"] != "launcher" {
		t.Fatalf("failure events=%+v", events)
	}
}

func TestEventStreamAndVisualizationFailuresAreAtomic(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		configure   func(*HandoffService, *launchCoord)
	}{
		{name: "sensor install", stage: "install_sensors", configure: func(s *HandoffService, _ *launchCoord) {
			s.Persist.(*launchPersistence).sensorsErr = errors.New("sensor refused")
		}},
		{name: "started event", stage: "emit_started", configure: func(_ *HandoffService, c *launchCoord) { c.emitErr = errors.New("event bus blank exit") }},
		{name: "visualization", stage: "visualization", configure: func(s *HandoffService, _ *launchCoord) { s.Viz = &launchViz{err: errors.New("projection refused")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_STATE_DIR", t.TempDir())
			reg, persist := &Registry{}, &launchPersistence{}
			service, coord := newAtomicLaunchService(t, reg, persist)
			tc.configure(service, coord)
			resp, err := service.AgentStart(context.Background(), HandoffOpts{HostID: "hamburg", RemoteCWD: "/tmp", Command: "true", Name: "atomic-job"})
			if err == nil || resp.Extra["failure_stage"] != tc.stage || !persist.destroyed {
				t.Fatalf("response=%+v destroyed=%v err=%v", resp, persist.destroyed, err)
			}
		})
	}
}

func TestAgentEarlyExitReturnsDurableFailureAndCleansSession(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	oldDelay := agentReadyPollDelay
	agentReadyPollDelay = 0
	t.Cleanup(func() { agentReadyPollDelay = oldDelay })
	reg, persist := &Registry{}, &launchPersistence{capture: ""}
	service, coord := newAtomicLaunchService(t, reg, persist)
	resp, err := service.AgentStart(context.Background(), HandoffOpts{HostID: "hamburg", RemoteCWD: "/tmp", Agent: "cursor-agent", Goal: "bounded goal", Name: "early-exit", NoPane: true})
	if err == nil || resp == nil || resp.Extra["failure_stage"] != "delivery" || resp.Extra["delivery_state"] != EffectFailed || !persist.destroyed {
		t.Fatalf("response=%+v destroyed=%v err=%v", resp, persist.destroyed, err)
	}
	events := coord.events["early-exit"]
	if len(events) != 3 || events[0].Kind != "started" || events[1].Kind != "exit" || events[2].Kind != "result" {
		t.Fatalf("events=%+v", events)
	}
}

func TestManagedPreflightFailureNotifiesParent(t *testing.T) {
	parentService, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-apex", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "apex"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	service := &HandoffService{Reg: reg, ParentRouter: parentService}
	resp, err := service.AgentStart(context.Background(), HandoffOpts{Agent: "cursor-agent", Goal: "goal", SourceSessionID: parent.ID})
	if err == nil || resp == nil || resp.SessionID != "" || resp.Extra["failure_stage"] != "validate" {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	messages, listErr := parentService.ListMessages(parent.ID, false)
	if listErr != nil || len(messages) != 1 || messages[0].HandoffID != resp.AttemptID || len(notifier.notices) != 1 {
		t.Fatalf("messages=%+v notices=%d err=%v", messages, len(notifier.notices), listErr)
	}
}

func TestSuccessfulStartEmitsEventAndProjectsPane(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg, persist := &Registry{}, &launchPersistence{}
	service, coord := newAtomicLaunchService(t, reg, persist)
	viz := &launchViz{}
	service.Viz = viz
	resp, err := service.AgentStart(context.Background(), HandoffOpts{HostID: "hamburg", RemoteCWD: "/tmp", Command: "true", Name: "projected-job"})
	if err != nil || !resp.OK || resp.SessionID == "" || resp.Extra["presentation_state"] != EffectAcknowledged || resp.Extra["pane"] != true {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	if len(coord.events["projected-job"]) != 1 || coord.events["projected-job"][0].Kind != "started" || len(viz.projects) != 1 {
		t.Fatalf("events=%+v projections=%+v", coord.events, viz.projects)
	}
	stored, getErr := reg.GetSession(resp.SessionID)
	if getErr != nil || stored.VizSurfaceRef != "surface:test" {
		t.Fatalf("session=%+v err=%v", stored, getErr)
	}
}

func TestQueuedVisualizationRemainsPendingUntilClientAck(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg, persist := &Registry{}, &launchPersistence{}
	service, _ := newAtomicLaunchService(t, reg, persist)
	service.Viz = &launchViz{ref: "viz:queued:17"}
	resp, err := service.AgentStart(context.Background(), HandoffOpts{HostID: "hamburg", RemoteCWD: "/tmp", Command: "true", Name: "queued-job"})
	if err != nil || resp.Extra["presentation_state"] != EffectPending || resp.Extra["pane"] != false {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	stored, getErr := reg.GetHandoff(resp.HandoffID)
	if getErr != nil || stored.PresentationState != EffectPending {
		t.Fatalf("handoff=%+v err=%v", stored, getErr)
	}
}

func TestManagedFailurePersistsParentEnvelopeAndDirectResponseOnce(t *testing.T) {
	parentService, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-apex", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "apex"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	persist := &launchPersistence{launchErr: errors.New("password=must-not-leak")}
	service, _ := newAtomicLaunchService(t, reg, persist)
	service.ParentRouter = parentService
	notifier.notifyFail = true
	resp, err := service.AgentStart(context.Background(), HandoffOpts{HostID: "hamburg", RemoteCWD: "/tmp", Command: "true", Name: "failed-job", NoPane: true, SourceSessionID: parent.ID})
	if err == nil || resp == nil || resp.AttemptID == "" {
		t.Fatalf("direct response=%+v err=%v", resp, err)
	}
	messages, listErr := parentService.ListMessages(parent.ID, false)
	if listErr != nil || len(messages) != 1 || messages[0].HandoffID != resp.AttemptID || !strings.Contains(messages[0].Text, "stage=launch") || strings.Contains(messages[0].Text, "must-not-leak") {
		t.Fatalf("messages=%+v err=%v", messages, listErr)
	}
	stored, getErr := reg.GetHandoff(resp.AttemptID)
	if getErr != nil || stored.FailureNoticeState != EffectPending || stored.FailureNoticeError == "" {
		t.Fatalf("notice state=%+v err=%v", stored, getErr)
	}
	notifier.notifyFail = false
	ageDeliveryRetry(t, messages[0])
	if delivered, deliverErr := parentService.DeliverPending(context.Background(), parent.ID); deliverErr != nil || delivered != 1 {
		messages, _ = parentService.ListMessages(parent.ID, false)
		t.Fatalf("reconnect delivery=%d err=%v message=%+v", delivered, deliverErr, *messages[0])
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("manager wakeups=%d", len(notifier.notices))
	}
	stored, getErr = reg.GetHandoff(resp.AttemptID)
	if getErr != nil || stored.FailureNoticeState != EffectAcknowledged {
		t.Fatalf("recovered notice state=%+v err=%v", stored, getErr)
	}
}
