package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestLaunchTerminalReceiptAcrossAgentCLIsAndJob(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	cases := []struct {
		name string
		cmd  string
		rc   int
	}{
		{"cursor-agent", (&AgentSpec{Name: "cursor-agent"}).LaunchCommand("goal"), 0},
		{"codex", (&AgentSpec{Name: "codex"}).LaunchCommand("goal"), 0},
		{"claude", (&AgentSpec{Name: "claude"}).LaunchCommand("goal"), 0},
		{"direct-job", "printf job-output; exit 7", 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := withLaunchTerminalReceipt(tc.cmd, "ho-canary")
			if !strings.Contains(wrapped, "relay-launch-terminal handoff=ho-canary") {
				t.Fatalf("generated command lacks terminal receipt: %s", wrapped)
			}
			// Provider commands are inspected structurally so this test never starts
			// an authenticated CLI or encounters/answers one of its gates. The direct
			// job is the disposable live shell canary.
			if tc.name != "direct-job" {
				return
			}
			out, err := exec.Command("bash", "-lc", wrapped).CombinedOutput()
			if err == nil {
				t.Fatal("non-zero direct job was reported successful")
			}
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != tc.rc {
				t.Fatalf("wrapped exit = %v, want %d", err, tc.rc)
			}
			if !strings.Contains(string(out), "[relay-launch-terminal handoff=ho-canary rc=7]") {
				t.Fatalf("missing correlated receipt: %q", out)
			}
		})
	}
}

func TestLaunchTerminalReceiptPreservesExactProviderPayloads(t *testing.T) {
	for _, tc := range []struct {
		spec  AgentSpec
		wants []string
	}{
		{AgentSpec{Name: "cursor-agent", Command: "cursor-agent --model fast", Args: []string{"--print"}}, []string{"cursor-agent --model fast", "--print", "--force", "signal exit"}},
		{AgentSpec{Name: "codex"}, []string{"--dangerously-bypass-approvals-and-sandbox", "hooks.PermissionRequest", "hooks.Stop", "signal exit"}},
		{AgentSpec{Name: "claude", Args: []string{"--permission-mode", "bypassPermissions"}}, []string{"--permission-mode", "bypassPermissions", "--settings", "PermissionRequest", "Stop", "signal exit"}},
	} {
		spec := tc.spec
		payload := spec.launchScript("goal containing 'quotes' and `backticks`")
		wrapped := withLaunchTerminalReceipt(payload, "ho-exact")
		encoded := base64.StdEncoding.EncodeToString([]byte(payload))
		if !strings.Contains(wrapped, shellQuote(encoded)) {
			t.Fatalf("%s payload was re-quoted instead of encoded exactly: %s", spec.Name, wrapped)
		}
		if strings.Count(wrapped, "bash -ilc") != 1 || strings.Contains(wrapped, "bash -lc ") {
			t.Fatalf("%s launch regained nested login shells: %s", spec.Name, wrapped)
		}
		for _, want := range []string{"relay-launch-terminal handoff=ho-exact", "base64 -d", `bash -- "$relay_launch_tmp"`} {
			if !strings.Contains(wrapped, want) {
				t.Fatalf("%s wrapper missing %q: %s", spec.Name, want, wrapped)
			}
		}
		for _, want := range tc.wants {
			if !strings.Contains(payload, want) {
				t.Fatalf("%s exact payload missing %q: %s", spec.Name, want, payload)
			}
		}
	}
}

func TestCollectTerminalDiagnosticsBeforeCleanup(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-failed", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "failed"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-failed", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusRunning, CreatedAt: now}
	_ = reg.PutSession(sess)
	_ = reg.PutHandoff(ho)
	persist := &gatePersistence{capture: "provider error\n[relay-launch-terminal handoff=ho-failed rc=23]\n$ "}
	sessions := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil }}
	service := &HandoffService{Reg: reg, Persist: persist, Sessions: sessions, NewTransport: sessions.NewTransport}
	_ = service.failDelivery(context.Background(), ho, sess, errors.New("early exit"))
	stored, err := reg.GetHandoff(ho.ID)
	if err != nil || stored.TerminalExitCode == nil || *stored.TerminalExitCode != 23 {
		t.Fatalf("terminal exit diagnostic = %+v, err=%v", stored, err)
	}
	if !strings.Contains(stored.TerminalCapture, "provider error") || !persist.destroyed {
		t.Fatalf("capture/cleanup ordering lost: %+v destroyed=%t", stored, persist.destroyed)
	}
}

func TestReconcileFinalizesLiveHoldingShellFromTerminalReceipt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		status HandoffStatus
	}{
		{name: "success", code: 0, status: StatusDone},
		{name: "failure", code: 127, status: StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_STATE_DIR", t.TempDir())
			reg := &Registry{}
			now := time.Now().UTC()
			sess := &Session{ID: "sess-job", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "job"}, CreatedAt: now}
			ho := &Handoff{ID: "ho-job", SessionID: sess.ID, HostID: "self", Kind: KindJob, Status: StatusRunning, CreatedAt: now}
			if err := reg.PutSession(sess); err != nil {
				t.Fatal(err)
			}
			if err := reg.PutHandoff(ho); err != nil {
				t.Fatal(err)
			}
			persist := &gatePersistence{capture: fmt.Sprintf("job output\n[relay-launch-terminal handoff=ho-job rc=%d]\n$ ", tc.code)}
			transport := func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil }
			sessions := &SessionService{Reg: reg, Persist: persist, NewTransport: transport}
			service := &HandoffService{Reg: reg, Persist: persist, Sessions: sessions, NewTransport: transport}

			finalized, err := service.Reconcile(context.Background())
			if err != nil || finalized != 1 {
				t.Fatalf("reconcile finalized=%d err=%v", finalized, err)
			}
			stored, err := reg.GetHandoff(ho.ID)
			if err != nil || stored.Status != tc.status || stored.ExitCode == nil || *stored.ExitCode != tc.code || stored.TerminalExitCode == nil || *stored.TerminalExitCode != tc.code {
				t.Fatalf("terminal handoff=%+v err=%v", stored, err)
			}
			if !persist.destroyed {
				t.Fatal("terminal holding shell was not cleaned up")
			}
		})
	}
}

func TestTerminalCaptureIsBoundedAndRedacted(t *testing.T) {
	if got := redactedTerminalCapture("Authorization: Bearer abc"); got != "sensitive terminal output redacted" {
		t.Fatalf("credential leaked: %q", got)
	}
	got := redactedTerminalCapture(strings.Repeat("x", terminalCaptureLimit+50))
	if len(got) != terminalCaptureLimit {
		t.Fatalf("bounded capture length=%d", len(got))
	}
	match := launchTerminalReceipt.FindStringSubmatch("[relay-launch-terminal handoff=ho-7 rc=19]")
	if len(match) != 3 || match[1] != "ho-7" || match[2] != strconv.Itoa(19) || !regexp.MustCompile(`^ho-`).MatchString(match[1]) {
		t.Fatalf("receipt parse = %v", match)
	}
}

type sensorRecordingPersistence struct {
	renamePersistence
	handle  ports.PersistHandle
	command string
}

type gatePersistence struct {
	renamePersistence
	capture     string
	sent        []string
	sendErr     error
	destroyed   bool
	choices     []int
	afterChoice string
}

func (p *gatePersistence) ResolveGateChoice(_ context.Context, _ ports.Transport, _ ports.PersistHandle, offset int) error {
	p.choices = append(p.choices, offset)
	if p.afterChoice != "" {
		p.capture = p.afterChoice
	}
	return nil
}

func (p *gatePersistence) Destroy(context.Context, ports.Transport, ports.PersistHandle) error {
	p.destroyed = true
	return nil
}

func (p *gatePersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return p.capture, nil
}

func (p *gatePersistence) Send(_ context.Context, _ ports.Transport, _ ports.PersistHandle, text string, _ bool) error {
	p.sent = append(p.sent, text)
	return p.sendErr
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
	if stored.DeliveryState != EffectBlocked {
		t.Fatalf("delivery state = %q, want blocked", stored.DeliveryState)
	}
}

func TestGoalDeliveryRequiresComposerSubmissionEffect(t *testing.T) {
	oldDelay := agentReadyPollDelay
	agentReadyPollDelay = 0
	t.Cleanup(func() { agentReadyPollDelay = oldDelay })
	for _, tc := range []struct {
		name      string
		sendErr   error
		wantState EffectState
		wantErr   bool
	}{
		{name: "successful delivery", wantState: EffectAcknowledged},
		{name: "composer visible but submission unacknowledged", sendErr: errors.New("composer still holds message"), wantState: EffectPending, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_STATE_DIR", t.TempDir())
			reg := &Registry{}
			now := time.Now().UTC()
			sess := &Session{ID: "sess-agent", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "agent"}, CreatedAt: now}
			ho := &Handoff{ID: "ho-agent", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusRunning, LaunchState: EffectAcknowledged, DeliveryState: EffectPending, CreatedAt: now}
			if err := reg.PutHandoff(ho); err != nil {
				t.Fatal(err)
			}
			persist := &gatePersistence{capture: "agent ready\n❯ ", sendErr: tc.sendErr}
			service := &HandoffService{Reg: reg, Coord: newFakeCoord(), Persist: persist}
			err := service.injectAgentGoal(context.Background(), &fakeTransport{id: "self"}, sess, ho, "change code")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			stored, getErr := reg.GetHandoff(ho.ID)
			if getErr != nil || stored.DeliveryState != tc.wantState {
				t.Fatalf("handoff = %+v, err=%v", stored, getErr)
			}
		})
	}
}

func TestNativeGoalDeliveryNeverMutatesComposer(t *testing.T) {
	oldDelay := agentReadyPollDelay
	agentReadyPollDelay = 0
	t.Cleanup(func() { agentReadyPollDelay = oldDelay })
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-native", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "native"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-native", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusRunning, LaunchState: EffectAcknowledged, DeliveryState: EffectPending, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	persist := &gatePersistence{capture: "Cursor Agent\nGenerating response\n❯ "}
	service := &HandoffService{Reg: reg, Coord: newFakeCoord(), Persist: persist}
	if err := service.confirmNativeAgentGoal(context.Background(), &fakeTransport{id: "self"}, sess, ho); err != nil {
		t.Fatal(err)
	}
	stored, err := reg.GetHandoff(ho.ID)
	if err != nil || stored.DeliveryState != EffectAcknowledged || len(persist.sent) != 0 {
		t.Fatalf("handoff=%+v sent=%v err=%v", stored, persist.sent, err)
	}
}

func TestEarlyAgentExitIsNotSuccessfulDelivery(t *testing.T) {
	oldDelay := agentReadyPollDelay
	agentReadyPollDelay = 0
	t.Cleanup(func() { agentReadyPollDelay = oldDelay })
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-gone", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "gone"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-gone", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusRunning, LaunchState: EffectAcknowledged, DeliveryState: EffectPending, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	persist := &gatePersistence{capture: ""}
	service := &HandoffService{Reg: reg, Coord: newFakeCoord(), Persist: persist}
	if err := service.injectAgentGoal(context.Background(), &fakeTransport{id: "self"}, sess, ho, "change code"); err == nil {
		t.Fatal("early exit was reported as delivered")
	}
	if len(persist.sent) != 0 {
		t.Fatalf("goal sent to absent agent: %v", persist.sent)
	}
}

func TestFailedDeliveryTerminalizesBeforeCleanup(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-failed", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "failed"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-failed", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusRunning, LaunchState: EffectAcknowledged, DeliveryState: EffectPending, SourceSessionID: "sess-parent", CreatedAt: now}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	persist := &gatePersistence{}
	sessions := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil }}
	service := &HandoffService{Reg: reg, Sessions: sessions}
	cause := errors.New("composer did not acknowledge delivery")
	if err := service.failDelivery(context.Background(), ho, sess, cause); !errors.Is(err, cause) {
		t.Fatalf("failure cause lost: %v", err)
	}
	stored, err := reg.GetHandoff(ho.ID)
	if err != nil || !handoffTerminal(stored) || stored.DeliveryState != EffectFailed {
		t.Fatalf("terminal handoff = %+v, err=%v", stored, err)
	}
	if !persist.destroyed {
		t.Fatal("failed launch session was not torn down")
	}
	if _, err := reg.GetSession(sess.ID); err == nil {
		t.Fatal("failed launch session remained in authority registry")
	}
}

func TestGateResolutionSendsNoKeysUnlessExactGateIsStillVisible(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-gate", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "gate"}, CreatedAt: now}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	persist := &gatePersistence{capture: "agent ready\n› "}
	service := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil }}
	expected := &SecurityGate{Reason: "waiting for folder-trust approval", Directory: "/repo", Choices: []GateChoice{{Index: 1, Label: "Yes, continue", Selected: true}, {Index: 2, Label: "No, quit"}}}
	if err := service.ResolveGateChoice(context.Background(), sess.ID, expected, 1); err == nil {
		t.Fatal("stale gate state accepted")
	}
	if len(persist.choices) != 0 {
		t.Fatalf("keys sent after gate disappeared: %v", persist.choices)
	}
	persist.capture = "You are in /repo\nDo you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit\nPress enter to continue"
	if err := service.ResolveGateChoice(context.Background(), sess.ID, expected, 2); err != nil {
		t.Fatal(err)
	}
	if len(persist.choices) != 1 || persist.choices[0] != 1 {
		t.Fatalf("explicit second choice offset = %v", persist.choices)
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
