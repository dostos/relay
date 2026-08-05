package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

type fakeParentNotifier struct {
	bound        []string
	notices      []ParentNotice
	sent         []string
	sendAttempts int
	notifyFail   bool
	failNotices  map[string]bool
	uncertain    bool
	started      chan struct{}
	release      chan struct{}
}

type fakeRetirementViz struct {
	closed    []string
	presented map[string]ports.Layout
}

type recordingPersistence struct {
	renamePersistence
	sent []string
}

type capturePersistence struct {
	renamePersistence
	capture string
}

type captureThenRecordPersistence struct {
	*recordingPersistence
	capture string
}

func (p *capturePersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return p.capture, nil
}

func (p *captureThenRecordPersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return p.capture, nil
}

func (p *recordingPersistence) Send(_ context.Context, _ ports.Transport, _ ports.PersistHandle, text string, _ bool) error {
	p.sent = append(p.sent, text)
	return nil
}

func (f *fakeRetirementViz) Kind() string                   { return "test" }
func (f *fakeRetirementViz) Available(context.Context) bool { return true }
func (f *fakeRetirementViz) Present(_ context.Context, sessionID, _ string, layout ports.Layout) (string, error) {
	if f.presented == nil {
		f.presented = map[string]ports.Layout{}
	}
	f.presented[sessionID] = layout
	return "viz:queued:1", nil
}
func (f *fakeRetirementViz) Focus(context.Context, string) error { return nil }
func (f *fakeRetirementViz) Close(_ context.Context, sessionID string) error {
	f.closed = append(f.closed, sessionID)
	return nil
}
func (f *fakeRetirementViz) Layout(context.Context) (string, error) { return "", nil }
func (f *fakeRetirementViz) SaveRestorable(context.Context) (int, error) {
	return 0, nil
}
func (f *fakeRetirementViz) RestoreSaved(context.Context) (int, error) {
	return 0, nil
}
func (f *fakeRetirementViz) BrandLabels(context.Context, map[string]string) error { return nil }

func (f *fakeParentNotifier) BindLocalParent(_ context.Context, sessionID, surface string) (string, error) {
	f.bound = append(f.bound, sessionID+"@"+surface)
	return surface, nil
}
func (f *fakeParentNotifier) NotifyParent(_ context.Context, _ string, notice ParentNotice) error {
	f.notices = append(f.notices, notice)
	if f.notifyFail || f.failNotices[notice.MessageID] {
		return errors.New("parent disconnected")
	}
	return nil
}
func (f *fakeParentNotifier) CaptureScreen(context.Context, string, int) (string, error) {
	return "› \n", nil
}
func (f *fakeParentNotifier) SendScreen(_ context.Context, _ string, text string, _ bool) error {
	f.sendAttempts++
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
		<-f.release
	}
	if f.uncertain {
		return &ports.DeliveryUncertainError{Err: errors.New("submission acknowledgement unavailable")}
	}
	if f.notifyFail {
		return errors.New("parent disconnected")
	}
	f.sent = append(f.sent, text)
	return nil
}

func newParentTestService(t *testing.T) (*ParentService, *fakeParentNotifier, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	notifier := &fakeParentNotifier{}
	sessions := &SessionService{Reg: reg, Persist: &renamePersistence{}, Screen: notifier, NewTransport: func(host string) (ports.Transport, error) {
		return &fakeTransport{id: host, outputs: map[string]string{host: ""}}, nil
	}}
	return &ParentService{Reg: reg, Sessions: sessions, Notifier: notifier}, notifier, reg
}

func TestRegisterLocalParentCreatesRealSessionAndBinding(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	repo := t.TempDir()
	sess, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{
		Surface: "surface:41", Name: "personal-db-main", RepoRefs: []string{repo}, WakeMode: "notify",
	})
	if err != nil || !created {
		t.Fatalf("register created=%v err=%v", created, err)
	}
	if !isLocalParent(sess) || sess.VizSurfaceRef != "surface:41" || sess.RepoRefs[0] != repo {
		t.Fatalf("session = %+v", sess)
	}
	if len(notifier.bound) != 1 {
		t.Fatalf("bindings = %v", notifier.bound)
	}
	stored, err := reg.GetSession(sess.ID)
	if err != nil || stored.Labels["parent_state"] != "active" {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}

	again, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Surface: "surface:41", RepoRefs: []string{repo}})
	if err != nil || created || again.ID != sess.ID {
		t.Fatalf("idempotent register = %+v created=%v err=%v", again, created, err)
	}
}

func TestBindLocalParentPreservesIdentityOnRestartedSurface(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "engram-main"}, Labels: map[string]string{"role": ParentRole, "parent_state": "complete"}, VizSurfaceRef: "surface:7", CreatedAt: now, UpdatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	bound, err := service.BindLocal(context.Background(), parent.ID, "212")
	if err != nil {
		t.Fatal(err)
	}
	if bound.ID != parent.ID || bound.VizSurfaceRef != "surface:212" || bound.Labels["parent_state"] != "active" {
		t.Fatalf("bound parent=%+v", bound)
	}
	if len(notifier.bound) != 1 || notifier.bound[0] != "sess-parent@surface:212" {
		t.Fatalf("bindings=%v", notifier.bound)
	}
	stored, err := reg.GetSession(parent.ID)
	if err != nil || stored.VizSurfaceRef != "surface:212" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

func TestLinkChildAdoptsExistingGoalExactlyOnce(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "existing-goal"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-existing", SessionID: child.ID, HostID: "c3", Kind: KindAgent, Status: StatusRunning, CreatedAt: now}
	for _, sess := range []*Session{parent, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}

	linked, err := service.LinkChild(parent.ID, ho.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked.SourceSessionID != parent.ID || linked.SourceHostID != LocalHostID || linked.SourcePersistName != parent.Persist.Name {
		t.Fatalf("linked handoff = %+v", linked)
	}
	stored, err := reg.GetSession(child.ID)
	if err != nil || stored.SourceSessionID != parent.ID || stored.CreatedByHandoffID != ho.ID {
		t.Fatalf("linked child = %+v err=%v", stored, err)
	}
	if _, err := service.LinkChild(parent.ID, ho.ID); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Edges) != 1 || graph.Edges[0].HandoffID != ho.ID {
		t.Fatalf("history = %+v err=%v", graph, err)
	}
}

func TestRouteChildEventDeduplicatesAndKeepsMessageCompact(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"}, Labels: map[string]string{"role": ParentRole, "wake_mode": "notify"}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	for _, sess := range []*Session{parent, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: "c3", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	ev := coord.Event{Seq: 7, Kind: "permission_required", Meta: map[string]any{"text": strings.Repeat("approve ", 300), "correlation_id": "req-7"}}
	msg, err := service.RouteChildEvent(context.Background(), ho, ev)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != "permission_required" || msg.CorrelationID != "req-7" || len(msg.Text) > ParentTextLimit {
		t.Fatalf("message = %+v", msg)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("notices = %d", len(notifier.notices))
	}
	dup, err := service.RouteChildEvent(context.Background(), ho, ev)
	if err != nil || dup.ID != msg.ID || len(notifier.notices) != 1 {
		t.Fatalf("duplicate = %+v err=%v notices=%d", dup, err, len(notifier.notices))
	}
	pending, err := service.ListMessages(parent.ID, true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Communications) != 1 || graph.Communications[0].Action != "request" {
		t.Fatalf("history=%+v err=%v", graph, err)
	}
}

func TestInjectWakeModeDoesNotDuplicateDesktopInterruption(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "agent-manager"}, Labels: map[string]string{"role": ParentRole, "wake_mode": "inject"}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "choose A or B"}})
	if err != nil || msg == nil || msg.DeliveredAt == nil || len(notifier.sent) != 1 || len(notifier.notices) != 0 {
		t.Fatalf("inject wake duplicated presentation: msg=%+v sends=%d notices=%d err=%v", msg, len(notifier.sent), len(notifier.notices), err)
	}
}

func TestRemoteManagerReceivesChildEventWithoutHumanNotification(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	recorder := &recordingPersistence{}
	service.Sessions.Persist = recorder
	now := time.Now().UTC()
	manager := &Session{ID: "sess-manager", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "manager"}, SourceSessionID: "sess-root", CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, SourceSessionID: manager.ID, CreatedAt: now}
	for _, sess := range []*Session{manager, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{ID: "ho-worker", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve tool?"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ParentSessionID != manager.ID || len(notifier.notices) != 0 {
		t.Fatalf("event escaped immediate manager: msg=%+v human_notices=%d", msg, len(notifier.notices))
	}
	if len(recorder.sent) != 1 || !strings.Contains(recorder.sent[0], "relay resolve "+msg.ID) {
		t.Fatalf("manager delivery = %v", recorder.sent)
	}
}

func TestDecisionExcerptDropsChromeAndKeepsPermissionPrompt(t *testing.T) {
	capture := `
  $ git -C /repo status Waiting for approval...

────────────────────────────────────────
 Run this command?
 Not in allowlist: git -C, head
  → Run (once) (y)
    Add Shell(git -C), Shell(head) to allowlist? (tab)
    Skip & tell the agent what to do instead (esc or n)

| | Auto | ~/dev/folio · agent/backends-and-adapters · #2
`
	got := decisionExcerpt(capture)
	if !strings.Contains(got, "Run this command?") || !strings.Contains(got, "Not in allowlist") {
		t.Fatalf("decision context missing: %q", got)
	}
	if strings.Contains(got, "Auto") || strings.Contains(got, "~/dev/folio") {
		t.Fatalf("status chrome leaked: %q", got)
	}
	if len(got) > ParentTextLimit {
		t.Fatalf("decision excerpt is unbounded: %d", len(got))
	}
}

func TestPaneStillActiveSuppressesOnlyNonActionableIdle(t *testing.T) {
	active := "⠠⠜ Running  24.03k tokens\nTip: Type ?\n→ Add a follow-up\n"
	if !paneStillActive(active) {
		t.Fatal("active agent was treated as idle")
	}
	permission := active + "Run this command?\nNot in allowlist: git -C, head\n"
	if paneStillActive(permission) {
		t.Fatal("permission prompt was suppressed as active")
	}
	if paneStillActive("Completed checkpoint\n› Add a follow-up\n") {
		t.Fatal("completed pane was treated as active")
	}
}

func TestIdlePermissionPromptIsClassifiedAndNotifiedOnce(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	service.Sessions.Persist = &capturePersistence{capture: "Run this command?\nNot in allowlist: echo, hostname, test\n→ Run (once) (y)\n  Add Shell(echo) to allowlist? (tab)\n  Skip & tell the agent what to do instead (esc or n)\n"}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: child.HostID, Agent: "cursor-agent", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}

	first, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "idle"})
	if err != nil || first.Kind != "permission_required" || first.State != ParentMessagePending || len(notifier.notices) != 1 {
		t.Fatalf("first=%+v err=%v notices=%d", first, err, len(notifier.notices))
	}
	second, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "idle"})
	if err != nil || second.ID != first.ID || second.State != ParentMessagePending {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("repeated prompt notices=%d", len(notifier.notices))
	}
}

func TestQuotedGateProseDoesNotCreatePermissionEnvelope(t *testing.T) {
	for _, capture := range []string{
		`The manager said "do not rephrase permission_required messages." Held. I won't retry.`,
		`Confirmed: no permission is required; do not approve anything.`,
		`The literal gate name permission_required is quoted documentation.`,
		`I was instructed to quote "Do you trust the contents of this directory?" in the report.`,
	} {
		t.Run(capture, func(t *testing.T) {
			service, notifier, reg := newParentTestService(t)
			service.Sessions.Persist = &capturePersistence{capture: capture}
			now := time.Now().UTC()
			parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
			child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
			_ = reg.PutSession(parent)
			_ = reg.PutSession(child)
			ho := &Handoff{ID: "ho-quoted", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
			msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 213, Kind: "idle"})
			if err != nil || msg != nil || len(notifier.notices) != 0 {
				t.Fatalf("quoted prose routed: msg=%+v notices=%d err=%v", msg, len(notifier.notices), err)
			}
		})
	}
}

func TestProseMetadataCannotPromoteAuthorityEvent(t *testing.T) {
	for _, ev := range []coord.Event{
		{Kind: "ask", Meta: map[string]any{"type": "permission_required", "text": "quoted token"}},
		{Kind: "result", Meta: map[string]any{"category": "approval_required", "text": "ordinary result"}},
		{Kind: "needs_input", Meta: map[string]any{"reason": "not_permission_required", "text": "question"}},
	} {
		if got := attentionKind(ev); got == "permission_required" {
			t.Fatalf("metadata manufactured authority event: %+v", ev)
		}
	}
}

func TestChangedStructuredToolGateIsNotDeduplicated(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	capture := &capturePersistence{}
	service.Sessions.Persist = capture
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-tool", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	frame := func(command string) string {
		return "Run this command?\nNot in allowlist: " + command + "\n→ Run (once) (y)\n  Add to allowlist? (tab)\n  Skip (esc or n)"
	}
	capture.capture = frame("git status")
	first, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "idle"})
	if err != nil || first == nil {
		t.Fatalf("first gate=%+v err=%v", first, err)
	}
	capture.capture = frame("git push")
	second, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "idle"})
	if err != nil || second == nil || second.ID == first.ID || len(notifier.notices) != 2 {
		t.Fatalf("changed gate hidden: first=%+v second=%+v notices=%d err=%v", first, second, len(notifier.notices), err)
	}
}

func TestPlainCommandChangesStructuredToolGateIdentity(t *testing.T) {
	first := ClassifyAgentPane("Run this command?\ngit status\n→ Run once (y)\n  Skip (esc or n)")
	second := ClassifyAgentPane("Run this command?\ngit push origin main\n→ Run once (y)\n  Skip (esc or n)")
	if first.Gate == nil || second.Gate == nil || first.Gate.Subject == second.Gate.Subject || sameGateDecision(first.Gate, second.Gate) {
		t.Fatalf("plain commands collapsed: first=%+v second=%+v", first, second)
	}
}

func TestMultilineCommandChangesStructuredToolGateIdentity(t *testing.T) {
	first := ClassifyAgentPane("Run this command?\npython <<EOF\nprint('a')\nEOF\n→ Run once (y)\n  Skip (esc or n)")
	second := ClassifyAgentPane("Run this command?\npython <<EOF\nprint('b')\nEOF\n→ Run once (y)\n  Skip (esc or n)")
	if first.Gate == nil || second.Gate == nil || sameGateDecision(first.Gate, second.Gate) {
		t.Fatalf("multiline commands collapsed: first=%+v second=%+v", first, second)
	}
}

func TestPermissionEnvelopeCannotBeAcknowledgedWithoutResolution(t *testing.T) {
	service, _, _ := newParentTestService(t)
	now := time.Now().UTC()
	msg := &ParentMessage{V: 1, ID: "pm-gate", CorrelationID: "gate", ParentSessionID: "sess-parent", ChildSessionID: "sess-child", HandoffID: "ho-gate", Kind: "permission_required", State: ParentMessagePending, CreatedAt: now}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ack(msg.ID); err == nil || !strings.Contains(err.Error(), "explicit relay resolve") {
		t.Fatalf("permission acknowledgement accepted: %v", err)
	}
	stored, err := service.FindMessage(msg.ID)
	if err != nil || stored.State != ParentMessagePending {
		t.Fatalf("permission state changed: %+v err=%v", stored, err)
	}
}

func TestExplicitPermissionEventIsNotHiddenByStalePendingGate(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Sessions.Persist = &capturePersistence{capture: "agent is running normally"}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	stale := &SecurityGate{Reason: "waiting for folder-trust approval", Directory: "/old", Subject: "/old"}
	ho := &Handoff{ID: "ho-explicit", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusNeedsInput, PendingGate: stale, SourceSessionID: parent.ID, CreatedAt: now}
	prior := &ParentMessage{V: 1, ID: "pm-old", CorrelationID: "old", ParentSessionID: parent.ID, ChildSessionID: child.ID, HandoffID: ho.ID, EventSeq: 1, Kind: "permission_required", Text: "old trust gate", Gate: stale, State: ParentMessagePending, CreatedAt: now}
	if err := writeParentMessage(prior, true); err != nil {
		t.Fatal(err)
	}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "permission_required", Meta: map[string]any{"text": "new tool approval", "structured_gate": true}})
	if err != nil || msg == nil || msg.ID == prior.ID || msg.Gate != nil || len(notifier.notices) != 1 {
		t.Fatalf("explicit decision hidden by stale gate: msg=%+v notices=%d err=%v", msg, len(notifier.notices), err)
	}
}

func TestDisconnectedParentReplayDoesNotRetryDurableAttentionEnvelope(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	service.Sessions.Persist = &capturePersistence{capture: "Completed checkpoint\n→ Add a follow-up\n"}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)

	notifier.notifyFail = true
	first, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "continue?"}})
	if err == nil || first.DeliveredAt != nil {
		t.Fatalf("disconnected first=%+v err=%v", first, err)
	}
	notifier.notifyFail = false
	second, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "ask", Meta: map[string]any{"text": "continue?"}})
	if err != nil || second.ID != first.ID || second.DeliveredAt != nil || len(notifier.sent) != 0 {
		t.Fatalf("reconnect second=%+v err=%v", second, err)
	}
	for seq := int64(3); seq <= 5; seq++ {
		if _, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: seq, Kind: "ask", Meta: map[string]any{"text": "continue?"}}); err != nil {
			t.Fatal(err)
		}
	}
	if notifier.sendAttempts != 1 {
		t.Fatalf("duplicate child frames caused %d delivery attempts, want initial attempt only", notifier.sendAttempts)
	}
	// Automatic delivery owns a durable retry schedule; age the failed attempt
	// instead of depending on a timing sleep.
	ageDeliveryRetry(t, first)
	if delivered, err := service.DeliverPending(context.Background(), parent.ID); err != nil || delivered != 1 {
		t.Fatalf("durable retry delivered=%d err=%v", delivered, err)
	}
	messages, err := service.ListMessages(parent.ID, false)
	if err != nil || len(messages) != 1 || messages[0].DeliveredAt == nil || len(notifier.notices) != 1 || len(notifier.sent) != 1 {
		t.Fatalf("messages=%+v notices=%d err=%v", messages, len(notifier.notices), err)
	}
	t.Logf("five_child_frames_delivery_attempts=5->2")
}

func timePointer(value time.Time) *time.Time { return &value }

func ageDeliveryRetry(t *testing.T, msg *ParentMessage) {
	t.Helper()
	msg.LastAttemptAt = timePointer(time.Now().Add(-deliveryRetryCap))
	if err := writeParentMessage(msg, false); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticDeliveryBackoffIsDurableAndCapped(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		attempts int
		age      time.Duration
		due      bool
	}{
		{attempts: 1, age: 4 * time.Second, due: false},
		{attempts: 1, age: 5 * time.Second, due: true},
		{attempts: 2, age: 9 * time.Second, due: false},
		{attempts: 2, age: 10 * time.Second, due: true},
		{attempts: 15_464, age: 14 * time.Minute, due: false},
		{attempts: 15_464, age: 15 * time.Minute, due: true},
	} {
		msg := &ParentMessage{DeliveryAttempts: tc.attempts, LastAttemptAt: timePointer(now.Add(-tc.age)), LastAttemptOutcome: "retryable"}
		if got := deliveryRetryDue(msg, now); got != tc.due {
			t.Fatalf("attempts=%d age=%s due=%v want=%v", tc.attempts, tc.age, got, tc.due)
		}
	}
}

func TestDeliverPendingDoesNotBurstUnchangedApexFailure(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	apex := &Session{ID: "sess-apex", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: "tmux", Name: "apex"}, Labels: map[string]string{ApexLabel: "true"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-apex", SessionID: apex.ID, HostID: apex.HostID, Kind: KindAgent, Status: StatusRunning, CreatedAt: now}
	_ = reg.PutSession(apex)
	_ = reg.PutHandoff(ho)
	notifier.notifyFail = true

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "result", Meta: map[string]any{"text": "unchanged stuck result"}})
	if err == nil || msg == nil || msg.DeliveryAttempts != 1 || len(notifier.notices) != 1 {
		t.Fatalf("initial failure msg=%+v attempts=%d err=%v", msg, len(notifier.notices), err)
	}
	for i := 0; i < 100; i++ {
		if delivered, retryErr := service.DeliverPending(context.Background(), apex.ID); retryErr != nil || delivered != 0 {
			t.Fatalf("backoff retry %d delivered=%d err=%v", i, delivered, retryErr)
		}
	}
	stored, err := service.FindMessage(msg.ID)
	if err != nil || stored.DeliveryAttempts != 1 || len(notifier.notices) != 1 {
		t.Fatalf("unchanged failure burst: msg=%+v attempts=%d err=%v", stored, len(notifier.notices), err)
	}
}

func TestFailedEnvelopeDoesNotBlockDueSibling(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	apex := &Session{ID: "sess-apex", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: "tmux", Name: "apex"}, Labels: map[string]string{ApexLabel: "true"}, CreatedAt: now}
	_ = reg.PutSession(apex)
	for _, handoffID := range []string{"ho-stuck", "ho-ready"} {
		_ = reg.PutHandoff(&Handoff{ID: handoffID, SessionID: apex.ID, HostID: apex.HostID, Kind: KindAgent, Status: StatusRunning, CreatedAt: now})
	}
	stuck := &ParentMessage{V: 1, ID: "pm-stuck", CorrelationID: "stuck", ParentSessionID: apex.ID, ChildSessionID: apex.ID, HandoffID: "ho-stuck", EventSeq: 1, Kind: "result", Text: "stuck", State: ParentMessagePending, CreatedAt: now}
	ready := &ParentMessage{V: 1, ID: "pm-ready", CorrelationID: "ready", ParentSessionID: apex.ID, ChildSessionID: apex.ID, HandoffID: "ho-ready", EventSeq: 2, Kind: "result", Text: "ready", State: ParentMessagePending, CreatedAt: now.Add(time.Second)}
	if err := writeParentMessage(stuck, true); err != nil {
		t.Fatal(err)
	}
	if err := writeParentMessage(ready, true); err != nil {
		t.Fatal(err)
	}
	notifier.failNotices = map[string]bool{stuck.ID: true}

	delivered, err := service.DeliverPending(context.Background(), apex.ID)
	if delivered != 1 || err == nil || !strings.Contains(err.Error(), stuck.ID) {
		t.Fatalf("delivered=%d err=%v", delivered, err)
	}
	storedReady, findErr := service.FindMessage(ready.ID)
	if findErr != nil || storedReady.DeliveredAt == nil || len(notifier.notices) != 2 {
		t.Fatalf("ready=%+v notices=%d err=%v", storedReady, len(notifier.notices), findErr)
	}
}

func TestUncertainSubmissionIsNotRetypedAcrossSupervisorRetries(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "apex"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	notifier.uncertain = true

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 7, Kind: "result", Meta: map[string]any{"text": "identical terminal result"}})
	if err == nil || msg == nil || msg.DeliveredAt != nil || msg.DeliveryMethod != "session_send_uncertain" || notifier.sendAttempts != 1 {
		t.Fatalf("first msg=%+v attempts=%d err=%v", msg, notifier.sendAttempts, err)
	}
	// Model an old bridge/new supervisor restart boundary: the durable inbox is
	// reloaded and DeliverPending runs repeatedly. The uncertain attempted
	// effect remains visible, but the manager composer is never mutated again.
	for i := 0; i < 3; i++ {
		if delivered, retryErr := service.DeliverPending(context.Background(), parent.ID); retryErr != nil || delivered != 0 {
			t.Fatalf("retry %d delivered=%d err=%v", i, delivered, retryErr)
		}
	}
	if retryErr := service.deliverMessage(context.Background(), parent, ho, msg); retryErr == nil {
		t.Fatal("uncertain delivery returned nil and could be mistaken for confirmed failover")
	}
	if notifier.sendAttempts != 1 {
		t.Fatalf("uncertain terminal result was typed %d times", notifier.sendAttempts)
	}
	stored, err := service.ListMessages(parent.ID, false)
	if err != nil || len(stored) != 1 || stored[0].State != ParentMessagePending || stored[0].DeliveryError == "" {
		t.Fatalf("uncertain effect lost from audit: %+v err=%v", stored, err)
	}
	item := CompactParentMessage(stored[0], false)
	if item.Delivery != deliveryUncertain || item.DeliveryError == "" || item.Next != "redeliver" {
		t.Fatalf("uncertain effect hidden from manager projection: %+v", item)
	}
	ho.Status = StatusDone
	_ = reg.PutHandoff(ho)
	if swept, _, sweepErr := service.SweepTerminal(parent.ID); sweepErr != nil || swept != 0 {
		t.Fatalf("terminal sweep erased uncertainty: swept=%d err=%v", swept, sweepErr)
	}
}

func TestUncertainAttentionDoesNotFailOverPastItsManager(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	now := time.Now().UTC()
	root := &Session{ID: "sess-root", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	manager := &Session{ID: "sess-manager", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "manager"}, Labels: map[string]string{"role": ParentRole}, SourceSessionID: root.ID, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, SourceSessionID: manager.ID, CreatedAt: now}
	for _, sess := range []*Session{root, manager, child} {
		_ = reg.PutSession(sess)
	}
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	notifier.uncertain = true

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "choose A or B"}})
	if err == nil || msg == nil || msg.ParentSessionID != manager.ID || msg.DeliveryMethod != deliveryUncertain {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
	if notifier.sendAttempts != 1 {
		t.Fatalf("uncertain attention crossed hierarchy with %d attempts", notifier.sendAttempts)
	}
}

func TestConcurrentDeliveryClaimsMutateComposerOnce(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "apex"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	msg := &ParentMessage{V: 1, ID: "pm-concurrent", ParentSessionID: parent.ID, ChildSessionID: child.ID, HandoffID: ho.ID, EventSeq: 1, Kind: "result", Text: "done", State: ParentMessagePending, CreatedAt: now}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	notifier.started, notifier.release = make(chan struct{}, 1), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() { firstDone <- service.deliverMessage(context.Background(), parent, ho, msg) }()
	<-notifier.started
	secondCopy := *msg
	if err := service.deliverMessage(context.Background(), parent, ho, &secondCopy); err == nil {
		t.Fatal("losing concurrent claimant reported confirmed delivery")
	}
	stored, err := service.FindMessage(msg.ID)
	if err != nil || stored.DeliveryMethod != deliveryAttempting {
		t.Fatalf("loser cleared active claim: %+v err=%v", stored, err)
	}
	thirdCopy := *msg
	if err := service.deliverMessage(context.Background(), parent, ho, &thirdCopy); err == nil {
		t.Fatal("third claimant entered while first claim was active")
	}
	if notifier.sendAttempts != 1 {
		t.Fatalf("active claim allowed %d composer mutations", notifier.sendAttempts)
	}
	close(notifier.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if notifier.sendAttempts != 1 {
		t.Fatalf("concurrent delivery mutated composer %d times", notifier.sendAttempts)
	}
}

func TestDeliveryFinalizationPreservesConcurrentManagerReply(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "apex"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	msg := &ParentMessage{V: 1, ID: "pm-reply-race", ParentSessionID: parent.ID, ChildSessionID: child.ID, HandoffID: ho.ID, EventSeq: 1, Kind: "ask", Text: "A or B?", State: ParentMessagePending, CreatedAt: now}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	notifier.started, notifier.release = make(chan struct{}, 1), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- service.deliverMessage(context.Background(), parent, ho, msg) }()
	<-notifier.started
	if _, err := service.Reply(context.Background(), msg.ID, "A"); err != nil {
		t.Fatal(err)
	}
	close(notifier.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stored, err := service.FindMessage(msg.ID)
	if err != nil || stored.State != ParentMessageReplied || stored.Reply != "A" || stored.DeliveredAt == nil || stored.DeliveryMethod != "session_send_confirmed" {
		t.Fatalf("delivery finalization erased reply: %+v err=%v", stored, err)
	}
}

func TestDeliveryFinalizationStopsAfterAuthorityRetirement(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "apex"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	msg := &ParentMessage{V: 1, ID: "pm-retired", ParentSessionID: parent.ID, ChildSessionID: child.ID, HandoffID: ho.ID, EventSeq: 1, Kind: "result", Text: "done", State: ParentMessagePending, CreatedAt: now}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	notifier.started, notifier.release = make(chan struct{}, 1), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- service.deliverMessage(context.Background(), parent, ho, msg) }()
	<-notifier.started
	if err := os.WriteFile(ProjectionOnlyMarkerPath(), []byte("authority retired"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(notifier.release)
	if err := <-done; err == nil {
		t.Fatal("retired authority finalized a delivery")
	}
	stored, err := readParentMessage(parentMessagePath(parent.ID, msg.ID))
	if err != nil || stored.DeliveryMethod != deliveryAttempting || stored.DeliveredAt != nil {
		t.Fatalf("retired authority mutated claim: %+v err=%v", stored, err)
	}
}

func TestFormatParentNoticeQualifiesRemoteHandoff(t *testing.T) {
	got := FormatParentNotice(ParentNotice{MessageID: "pm-1", Kind: "ask", Child: "worker@cancun", Text: "inspect remote", Action: "reply"})
	if !strings.Contains(got, "[relay ask worker@cancun]") || !strings.Contains(got, "relay resolve pm-1 --") || strings.Count(got, "pm-1") != 1 || strings.Contains(got, "ho-1") {
		t.Fatalf("notice lacks remote routing context: %q", got)
	}
	receipt := FormatParentNotice(ParentNotice{MessageID: "pm-2", Kind: "result", Child: "worker@cancun", Text: "done"})
	if strings.Contains(receipt, "resolve") || strings.Count(receipt, "pm-2") != 1 || receipt != "[relay result worker@cancun pm-2] done" {
		t.Fatalf("receipt created a handshake: %q", receipt)
	}
}

func TestReparentChildMovesPendingInboxAndHistoryEdge(t *testing.T) {
	service, _, reg := newParentTestService(t)
	viz := &fakeRetirementViz{}
	service.Viz = viz
	now := time.Now().UTC()
	oldParent := &Session{ID: "sess-old-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "beholder"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	newParent := &Session{ID: "sess-new-parent", HostID: "home-relay", Persist: ports.PersistHandle{Kind: "tmux", Name: "apex-v2"}, Labels: map[string]string{"agent": "future-agent"}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "cancun", Persist: ports.PersistHandle{Kind: "tmux", Name: "folio-cycle"}, SourceSessionID: oldParent.ID, CreatedAt: now}
	for _, sess := range []*Session{oldParent, newParent, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{ID: "ho-cycle", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: oldParent.ID, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	if err := AppendRelayHandoffEdge(oldParent.ID, child.ID, ho.ID); err != nil {
		t.Fatal(err)
	}
	msg := &ParentMessage{V: 1, ID: "pm-pending", ParentSessionID: oldParent.ID, ChildSessionID: child.ID, HandoffID: ho.ID, Kind: "ask", State: ParentMessagePending, CreatedAt: now}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	moved, oldID, err := service.ReparentChild(newParent.ID, ho.ID)
	if err != nil || oldID != oldParent.ID || moved.SourceSessionID != newParent.ID {
		t.Fatalf("moved=%+v old=%q err=%v", moved, oldID, err)
	}
	storedChild, err := reg.GetSession(child.ID)
	if err != nil || storedChild.SourceSessionID != newParent.ID {
		t.Fatalf("child=%+v err=%v", storedChild, err)
	}
	if viz.presented[child.ID].SourceSessionID != newParent.ID {
		t.Fatalf("projection parent=%q", viz.presented[child.ID].SourceSessionID)
	}
	oldInbox, _ := service.ListMessages(oldParent.ID, true)
	newInbox, _ := service.ListMessages(newParent.ID, true)
	if len(oldInbox) != 0 || len(newInbox) != 1 || newInbox[0].ParentSessionID != newParent.ID {
		t.Fatalf("old inbox=%+v new inbox=%+v", oldInbox, newInbox)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Edges) != 1 || graph.Edges[0].SourceSessionID != newParent.ID || graph.Edges[0].HandoffID != ho.ID {
		t.Fatalf("history=%+v err=%v", graph, err)
	}
}

func TestReparentChildRefreshesPaneBindingWhenLineageAlreadyCorrect(t *testing.T) {
	service, _, reg := newParentTestService(t)
	viz := &fakeRetirementViz{}
	service.Viz = viz
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, SourceSessionID: parent.ID, CreatedAt: now}
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	_ = reg.PutHandoff(ho)
	if _, _, err := service.ReparentChild(parent.ID, ho.ID); err != nil {
		t.Fatal(err)
	}
	if viz.presented[child.ID].SourceSessionID != parent.ID {
		t.Fatalf("projection not refreshed: %v", viz.presented)
	}
}

func TestReparentChildRejectsManagerCycle(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	child := &Session{ID: "sess-child", SourceSessionID: "sess-parent", CreatedAt: now}
	parent := &Session{ID: "sess-parent", CreatedAt: now}
	descendant := &Session{ID: "sess-descendant", SourceSessionID: child.ID, CreatedAt: now}
	for _, sess := range []*Session{child, parent, descendant} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, SourceSessionID: parent.ID, Status: StatusRunning, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ReparentChild(descendant.ID, ho.ID); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle err=%v", err)
	}
}

func TestSweepTerminalOnlyAcknowledgesFinishedChildren(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	_ = reg.PutSession(parent)
	terminal := &Handoff{ID: "ho-done", SessionID: "sess-done", Status: StatusDone, SourceSessionID: parent.ID, CreatedAt: now}
	live := &Handoff{ID: "ho-live", SessionID: "sess-live", Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(terminal)
	_ = reg.PutHandoff(live)
	for _, msg := range []*ParentMessage{
		{V: 1, ID: "pm-done-1", ParentSessionID: parent.ID, ChildSessionID: terminal.SessionID, HandoffID: terminal.ID, Kind: "idle", State: ParentMessagePending, CreatedAt: now},
		{V: 1, ID: "pm-done-2", ParentSessionID: parent.ID, ChildSessionID: terminal.SessionID, HandoffID: terminal.ID, Kind: "result", State: ParentMessagePending, CreatedAt: now.Add(time.Second)},
		{V: 1, ID: "pm-done-gate", ParentSessionID: parent.ID, ChildSessionID: terminal.SessionID, HandoffID: terminal.ID, Kind: "permission_required", State: ParentMessagePending, CreatedAt: now.Add(2 * time.Second)},
		{V: 1, ID: "pm-live", ParentSessionID: parent.ID, ChildSessionID: live.SessionID, HandoffID: live.ID, Kind: "ask", State: ParentMessagePending, CreatedAt: now.Add(3 * time.Second)},
		{V: 1, ID: "pm-unknown", ParentSessionID: parent.ID, ChildSessionID: "sess-missing", HandoffID: "ho-missing", Kind: "idle", State: ParentMessagePending, CreatedAt: now.Add(4 * time.Second)},
	} {
		if err := writeParentMessage(msg, true); err != nil {
			t.Fatal(err)
		}
	}
	acked, byHandoff, err := service.SweepTerminal(parent.ID)
	if err != nil || acked != 3 || byHandoff[terminal.ID] != 3 {
		t.Fatalf("acked=%d by_handoff=%v err=%v", acked, byHandoff, err)
	}
	pending, _ := service.ListMessages(parent.ID, true)
	if len(pending) != 2 || pending[0].ID != "pm-live" || pending[1].ID != "pm-unknown" {
		t.Fatalf("pending=%+v", pending)
	}
}

func TestTerminalHandoffEventsNeverReachParent(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	_ = reg.PutSession(parent)
	for _, ho := range []*Handoff{
		{ID: "ho-done", SessionID: "sess-done", Status: StatusDone, SourceSessionID: parent.ID},
		{ID: "ho-outcome", SessionID: "sess-outcome", Status: StatusRunning, Outcome: "done", SourceSessionID: parent.ID},
		{ID: "ho-launch-failed", SessionID: "sess-launch-failed", Status: StatusFailed, LaunchState: EffectFailed, SourceSessionID: parent.ID},
	} {
		msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 99, Kind: "idle"})
		if err != nil || msg != nil {
			t.Fatalf("terminal event routed: msg=%+v err=%v", msg, err)
		}
	}
	if len(notifier.notices) != 0 {
		t.Fatalf("terminal notices=%d", len(notifier.notices))
	}
}

func TestPendingPermissionAbsorbsIdleWithoutSecondNotification(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: child.HostID, Agent: "codex", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	first, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve?"}})
	if err != nil || first.State != ParentMessagePending || len(notifier.notices) != 1 {
		t.Fatalf("permission=%+v err=%v notices=%d", first, err, len(notifier.notices))
	}
	second, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "idle"})
	if err != nil || second != nil {
		t.Fatalf("idle telemetry should not allocate an envelope: %+v err=%v", second, err)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("redundant idle notified root: %d", len(notifier.notices))
	}
}

func TestRepeatedPermissionFramesUseOneEnvelope(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-gate", SessionID: child.ID, Kind: KindAgent, Status: StatusNeedsInput, DeliveryState: EffectBlocked, SourceSessionID: parent.ID, CreatedAt: now}
	first, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "directory: /repo | 1. Yes | 2. No"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "permission_required", Meta: map[string]any{"text": "directory: /repo | 1. Yes | 2. No"}})
	if err != nil || second.ID != first.ID || len(notifier.notices) != 1 {
		t.Fatalf("duplicate gate envelope: first=%+v second=%+v notices=%d err=%v", first, second, len(notifier.notices), err)
	}
}

func TestBlockedSecurityGateIgnoresAutoReplyPolicy(t *testing.T) {
	service, _, reg := newParentTestService(t)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	// Preserve an old policy file as an adversarial upgrade case. Runtime must
	// not execute a permission reply even if an earlier build accepted it.
	if err := os.WriteFile(path, []byte("version: 1\nrules:\n  - id: must-not-trust\n    kind: permission_required\n    contains: [directory]\n    action: reply\n    reply: approve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.Policies = &PolicyService{Path: path}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-gate", SessionID: child.ID, Kind: KindAgent, Status: StatusNeedsInput, DeliveryState: EffectBlocked, PendingGate: &SecurityGate{Reason: "trust"}, SourceSessionID: parent.ID, CreatedAt: now}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "directory: /repo"}})
	if err != nil || msg.State != ParentMessagePending || msg.AutoHandled {
		t.Fatalf("security gate policy-selected: msg=%+v err=%v", msg, err)
	}
}

func TestAgentParentAcceptsDirectChildWorkspaceTrust(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	persist := &gatePersistence{
		capture: `Accessing workspace:
/home/jingyulee/gh/dostos-workspace
Quick safety check: Is this a project you created or one you trust?
Claude Code'll be able to read, edit, and execute files here.
❯ 1. Yes, I trust this folder
  2. No, exit
Enter to confirm · Esc to cancel`,
		afterChoice: "Claude ready\n› ",
	}
	service.Sessions.Persist = persist
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: "home", RemoteCWD: "~/dev/relay", Persist: ports.PersistHandle{Kind: "tmux", Name: "manager"}, Labels: map[string]string{"agent": "codex", "role": "handoff"}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", RemoteCWD: "~/gh/dostos-workspace", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, Labels: map[string]string{"agent": "claude", "role": "handoff"}, SourceSessionID: parent.ID, CreatedByHandoffID: "ho-gate", CreatedAt: now}
	ho := &Handoff{ID: "ho-gate", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Agent: "claude", Status: StatusRunning, LaunchState: EffectAcknowledged, DeliveryState: EffectPending, Goal: "run the canary", RemoteCWD: child.RemoteCWD, SourceSessionID: parent.ID, CreatedAt: now}
	for _, sess := range []*Session{parent, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "idle"})
	if err != nil || msg == nil || msg.State != ParentMessageReplied || msg.Reply != "approve" || !msg.AutoHandled || msg.PolicyID != agentChildWorkspaceTrustPolicyID {
		t.Fatalf("workspace trust result = %+v err=%v", msg, err)
	}
	if msg.Gate == nil || !msg.Gate.DirectoryObserved || len(persist.choices) != 1 || persist.choices[0] != 0 {
		t.Fatalf("workspace trust decision = gate %+v choices %v", msg.Gate, persist.choices)
	}
	if len(persist.sent) != 1 || !strings.Contains(persist.sent[0], "run the canary") || len(notifier.notices) != 0 {
		t.Fatalf("goal delivery=%v manager notices=%d", persist.sent, len(notifier.notices))
	}
	stored, err := reg.GetHandoff(ho.ID)
	if err != nil || stored.DeliveryState != EffectAcknowledged || stored.PendingGate != nil {
		t.Fatalf("handoff after trust = %+v err=%v", stored, err)
	}
}

func TestAgentParentWorkspaceTrustRequiresObservedMatchingDirectChild(t *testing.T) {
	for _, tc := range []struct {
		name, capture, childCWD, childParent, createdBy string
	}{
		{name: "different workspace", capture: "You are in /srv/other\nDo you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit", childCWD: "/srv/project", childParent: "sess-parent", createdBy: "ho-gate"},
		{name: "directory inferred", capture: "Do you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit", childCWD: "/srv/project", childParent: "sess-parent", createdBy: "ho-gate"},
		{name: "not direct child", capture: "You are in /srv/project\nDo you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit", childCWD: "/srv/project", childParent: "sess-other", createdBy: "ho-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, reg := newParentTestService(t)
			persist := &gatePersistence{capture: tc.capture, afterChoice: "ready\n› "}
			service.Sessions.Persist = persist
			now := time.Now().UTC()
			parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "manager"}, Labels: map[string]string{"agent": "codex", "role": ParentRole}, CreatedAt: now}
			child := &Session{ID: "sess-child", HostID: "c1", RemoteCWD: tc.childCWD, Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, SourceSessionID: tc.childParent, CreatedByHandoffID: tc.createdBy, CreatedAt: now}
			_ = reg.PutSession(parent)
			_ = reg.PutSession(child)
			ho := &Handoff{ID: "ho-gate", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, DeliveryState: EffectPending, SourceSessionID: parent.ID, CreatedAt: now}
			_ = reg.PutHandoff(ho)

			msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "idle"})
			if err != nil || msg == nil || msg.State != ParentMessagePending || msg.AutoHandled || len(persist.choices) != 0 {
				t.Fatalf("unsafe trust auto-resolved: msg=%+v choices=%v err=%v", msg, persist.choices, err)
			}
		})
	}
}

func TestSameRemoteWorkspace(t *testing.T) {
	for _, tc := range []struct {
		observed, declared string
		want               bool
	}{
		{"/home/jingyu/gh/relay", "~/gh/relay", true},
		{"/Users/jingyu/gh/relay", "~/gh/relay", true},
		{"/srv/relay", "/srv/relay/.", true},
		{"/home/jingyu/gh/other", "~/gh/relay", false},
		{"/tmp/gh/relay", "~/gh/relay", false},
		{"/home/alice/relay", "/home/bob/relay", false},
	} {
		if got := sameRemoteWorkspace(tc.observed, tc.declared); got != tc.want {
			t.Fatalf("sameRemoteWorkspace(%q, %q) = %v, want %v", tc.observed, tc.declared, got, tc.want)
		}
	}
}

func TestResolveGateDecisionRequiresExplicitUnambiguousChoice(t *testing.T) {
	gate := &SecurityGate{Choices: []GateChoice{{Index: 1, Label: "Yes, continue"}, {Index: 2, Label: "No, quit"}}}
	for _, tc := range []struct {
		decision string
		choice   int
		approve  bool
	}{
		{"approve", 1, true}, {"deny", 2, false}, {"1", 1, true}, {"2", 2, false},
	} {
		choice, approve, err := resolveGateDecision(gate, tc.decision)
		if err != nil || choice != tc.choice || approve != tc.approve {
			t.Fatalf("decision %q = %d/%v/%v", tc.decision, choice, approve, err)
		}
	}
	if _, _, err := resolveGateDecision(gate, "whatever"); err == nil {
		t.Fatal("ambiguous gate decision accepted")
	}
}

func TestExplicitGateApproveDeliversPendingGoalAndDenyCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		wantChoice     int
		wantState      EffectState
		wantTerminal   bool
	}{
		{name: "approve", decision: "approve", wantChoice: 0, wantState: EffectAcknowledged},
		{name: "deny", decision: "deny", wantChoice: 1, wantState: EffectDenied, wantTerminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_STATE_DIR", t.TempDir())
			reg := &Registry{}
			now := time.Now().UTC()
			parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
			child := &Session{ID: "sess-child", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, SourceSessionID: parent.ID, CreatedAt: now}
			gate := &SecurityGate{Reason: "waiting for folder-trust approval", Directory: "/repo", Choices: []GateChoice{{Index: 1, Label: "Yes, continue", Selected: true}, {Index: 2, Label: "No, quit"}}}
			ho := &Handoff{ID: "ho-gate", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusNeedsInput, Goal: "finish the task", LaunchState: EffectAcknowledged, DeliveryState: EffectBlocked, PendingGate: gate, SourceSessionID: parent.ID, CreatedAt: now}
			for _, sess := range []*Session{parent, child} {
				if err := reg.PutSession(sess); err != nil {
					t.Fatal(err)
				}
			}
			if err := reg.PutHandoff(ho); err != nil {
				t.Fatal(err)
			}
			msg := &ParentMessage{V: 1, ID: "pm-gate", CorrelationID: "gate", ParentSessionID: parent.ID, ChildSessionID: child.ID, HandoffID: ho.ID, Kind: "permission_required", State: ParentMessagePending, CreatedAt: now}
			if err := writeParentMessage(msg, true); err != nil {
				t.Fatal(err)
			}
			persist := &gatePersistence{capture: "You are in /repo\nDo you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit\nPress enter to continue", afterChoice: "Codex ready\n› "}
			sessions := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil }}
			service := &ParentService{Reg: reg, Sessions: sessions}
			if _, err := service.Reply(context.Background(), msg.ID, tc.decision); err != nil {
				t.Fatal(err)
			}
			stored, err := reg.GetHandoff(ho.ID)
			if err != nil || stored.DeliveryState != tc.wantState || handoffTerminal(stored) != tc.wantTerminal {
				t.Fatalf("resolved handoff = %+v err=%v", stored, err)
			}
			if len(persist.choices) != 1 || persist.choices[0] != tc.wantChoice {
				t.Fatalf("choice offsets = %v", persist.choices)
			}
			if tc.wantTerminal {
				if !persist.destroyed {
					t.Fatal("denied gate session not destroyed")
				}
			} else if len(persist.sent) != 1 || !strings.Contains(persist.sent[0], "finish the task") {
				t.Fatalf("pending goal was not delivered once: %v", persist.sent)
			}
		})
	}
}

func TestPolicyAutoReplyToAskIsAuditedAndSkipsManagerPing(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	recorder := &recordingPersistence{}
	service.Sessions.Persist = recorder
	policy := &PolicyService{Path: filepath.Join(t.TempDir(), "policy.yaml")}
	if err := policy.Add(PolicyRule{ID: "choose-a", Kind: "ask", Agent: "cursor-agent", Contains: []string{"choose"}, Action: "reply", Reply: "A"}); err != nil {
		t.Fatal(err)
	}
	service.Policies = policy
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "cancun", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: child.HostID, Agent: "cursor-agent", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "choose A or B"}})
	if err != nil || msg.State != ParentMessageReplied || msg.Reply != "A" || !msg.AutoHandled || msg.PolicyID != "choose-a" {
		t.Fatalf("message=%+v err=%v", msg, err)
	}
	if len(notifier.notices) != 0 || len(recorder.sent) != 1 || recorder.sent[0] != "A" {
		t.Fatalf("notices=%d sent=%v", len(notifier.notices), recorder.sent)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Communications) != 2 || graph.Communications[1].PolicyID != "choose-a" || !graph.Communications[1].AutoHandled {
		t.Fatalf("policy audit history=%+v err=%v", graph, err)
	}
}

func TestPolicyFailureFallsBackToManagerAndIsAudited(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("version: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.Policies = &PolicyService{Path: path}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: child.HostID, Agent: "codex", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "choose?"}})
	if err != nil || msg.State != ParentMessagePending || msg.PolicyError == "" {
		t.Fatalf("message=%+v err=%v", msg, err)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("policy failure did not reach manager: notices=%d", len(notifier.notices))
	}
	messages, err := service.ListMessages(parent.ID, false)
	if err != nil || len(messages) != 1 {
		t.Fatalf("audited messages=%+v err=%v", messages, err)
	}
	item := CompactParentMessage(messages[0], true)
	if item.PolicyError == "" {
		t.Fatalf("policy error missing from compact inbox: %+v", item)
	}
}

func TestRouteChildEventSupportsAllGoalControlKinds(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"}, Labels: map[string]string{"role": ParentRole, "wake_mode": "notify"}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-control", SessionID: child.ID, HostID: "c3", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)

	kinds := []string{"ask", "permission_required", "result", "exit"}
	for i, kind := range kinds {
		correlation := "corr-" + kind
		msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{
			Seq: int64(i + 1), Kind: kind,
			Meta: map[string]any{"text": kind + " payload", "correlation_id": correlation},
		})
		if err != nil {
			t.Fatalf("route %s: %v", kind, err)
		}
		wantState := ParentMessagePending
		if kind == "result" || kind == "exit" {
			wantState = ParentMessageAcked
		}
		if msg.Kind != kind || msg.CorrelationID != correlation || msg.State != wantState {
			t.Fatalf("%s message = %+v", kind, msg)
		}
		wantAction := ""
		if kind == "ask" || kind == "permission_required" {
			wantAction = "reply"
		}
		if got := notifier.notices[len(notifier.notices)-1].Action; got != wantAction {
			t.Fatalf("%s action = %s, want %s", kind, got, wantAction)
		}
	}
	if len(notifier.notices) != len(kinds) {
		t.Fatalf("notices = %d, want %d", len(notifier.notices), len(kinds))
	}
}

func TestResolveAndDeliveryCloseInboxWithCorrelatedHistory(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: "c3", Kind: KindAgent, Status: StatusNeedsInput, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	ask, _ := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"q": "deploy?"}})
	replied, err := service.Reply(context.Background(), ask.ID, "yes")
	if err != nil || replied.State != ParentMessageReplied {
		t.Fatalf("reply=%+v err=%v", replied, err)
	}
	storedHandoff, err := reg.GetHandoff(ho.ID)
	if err != nil || storedHandoff.Status != StatusRunning {
		t.Fatalf("replied handoff=%+v err=%v", storedHandoff, err)
	}
	result, _ := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "result", Meta: map[string]any{"text": "done"}})
	if result.State != ParentMessageAcked || result.AckedAt == nil {
		t.Fatalf("result was not closed on delivery: %+v", result)
	}
	pending, _ := service.ListMessages(parent.ID, true)
	if len(pending) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
	graph, _ := LoadHistory()
	if len(graph.Communications) != 3 || graph.Communications[1].Action != "resolve" || graph.Communications[2].Action != "event" {
		t.Fatalf("communications = %+v", graph.Communications)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Relay Test", "GIT_AUTHOR_EMAIL=relay@example.invalid", "GIT_COMMITTER_NAME=Relay Test", "GIT_COMMITTER_EMAIL=relay@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func cleanPushedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo, remote := filepath.Join(root, "repo"), filepath.Join(root, "remote.git")
	if out, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare: %v %s", err, out)
	}
	if out, err := exec.Command("git", "init", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README")
	gitRun(t, repo, "commit", "-m", "init")
	gitRun(t, repo, "remote", "add", "origin", remote)
	gitRun(t, repo, "push", "-u", "origin", "main")
	return repo
}

func TestRetirementGateRequiresStateChildrenInboxAndPushedRepos(t *testing.T) {
	service, _, reg := newParentTestService(t)
	repo := cleanPushedRepo(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local"}, RepoRef: repo, RepoRefs: []string{repo}, Labels: map[string]string{"role": ParentRole, "parent_state": "complete"}, CreatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	gate, err := service.RetirementStatus(context.Background(), parent.ID)
	if err != nil || !gate.Eligible {
		t.Fatalf("clean gate=%+v err=%v", gate, err)
	}

	if _, err := service.SetState(parent.ID, "active"); err != nil {
		t.Fatal(err)
	}
	gate, _ = service.RetirementStatus(context.Background(), parent.ID)
	if gate.Eligible || !strings.Contains(strings.Join(gate.Reasons, " "), "idle/complete") {
		t.Fatalf("active parent eligible: %+v", gate)
	}
	if _, err := service.SetState(parent.ID, "complete"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "dirty"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate, _ = service.RetirementStatus(context.Background(), parent.ID)
	if gate.Eligible {
		t.Fatalf("dirty repo eligible: %+v", gate)
	}
	if err := os.Remove(filepath.Join(repo, "dirty")); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("unpushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README")
	gitRun(t, repo, "commit", "-m", "unpushed")
	gate, _ = service.RetirementStatus(context.Background(), parent.ID)
	if gate.Eligible || len(gate.Repos) != 1 || gate.Repos[0].Pushed {
		t.Fatalf("unpushed commit eligible: %+v", gate)
	}
	gitRun(t, repo, "push", "origin", "main")

	pending := &ParentMessage{
		V: 1, ID: "pm-pending", CorrelationID: "corr-pending",
		ParentSessionID: parent.ID, ChildSessionID: "sess-finished", HandoffID: "ho-finished",
		EventSeq: 1, Kind: "result", State: ParentMessagePending, CreatedAt: now,
	}
	if err := writeParentMessage(pending, true); err != nil {
		t.Fatal(err)
	}
	gate, _ = service.RetirementStatus(context.Background(), parent.ID)
	if gate.Eligible || len(gate.PendingInbox) != 1 {
		t.Fatalf("pending inbox eligible: %+v", gate)
	}
	if _, err := service.Ack(pending.ID); err != nil {
		t.Fatal(err)
	}

	ho := &Handoff{ID: "ho-active", SessionID: "sess-child", HostID: "c3", Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	gate, _ = service.RetirementStatus(context.Background(), parent.ID)
	if gate.Eligible || len(gate.ActiveChildren) != 1 {
		t.Fatalf("active child gate=%+v", gate)
	}
	ho.Status, ho.Outcome = StatusDone, "done"
	_ = reg.PutHandoff(ho)
	viz := &fakeRetirementViz{}
	service.Viz = viz
	retired, err := service.Retire(context.Background(), parent.ID, false, false, false)
	if err != nil || !retired.Eligible || !retired.Closed {
		t.Fatalf("retire=%+v err=%v", retired, err)
	}
	if _, err := reg.GetSession(parent.ID); err == nil {
		t.Fatal("retired parent remains registered")
	}
	if len(viz.closed) != 1 || viz.closed[0] != parent.ID {
		t.Fatalf("closed surfaces = %v", viz.closed)
	}
}

func TestSessionDestroyCannotBypassParentGate(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	if err := service.Sessions.Destroy(context.Background(), parent.ID, false); err == nil || !strings.Contains(err.Error(), "parent retire") {
		t.Fatalf("unguarded destroy error = %v", err)
	}
}

func TestForcedRetireCannotOrphanDirectChild(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "parent"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", SourceSessionID: parent.ID, HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "child"}, CreatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Retire(context.Background(), parent.ID, false, true, true); err == nil || !strings.Contains(err.Error(), "direct child") {
		t.Fatalf("forced retirement error=%v", err)
	}
	if _, err := reg.GetSession(parent.ID); err != nil {
		t.Fatalf("parent was deleted: %v", err)
	}
}

func TestCompactParentMessageOmitsDurableRoutingMetadata(t *testing.T) {
	now := time.Now().UTC()
	msg := &ParentMessage{
		V: 1, ID: "pm-1", CorrelationID: "deploy-1",
		ParentSessionID: "sess-parent", ChildSessionID: "sess-child",
		HandoffID: "ho-1", EventSeq: 91, Kind: "permission_required",
		Text: "approve deploy?", State: ParentMessagePending, CreatedAt: now,
	}
	item := CompactParentMessage(msg, false)
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, redundant := range []string{"parent_session_id", "child_session_id", "handoff_id", "correlation_id", "event_seq", "created_at", "\"state\""} {
		if strings.Contains(text, redundant) {
			t.Fatalf("compact inbox leaked %s: %s", redundant, text)
		}
	}
	if item.Next != "resolve" || len(item.Argv) == 0 || item.Argv[1] != "resolve" || item.Argv[len(item.Argv)-1] != "<decision>" {
		t.Fatalf("compact inbox omitted executable decision: %s", text)
	}
	if len(raw) > 300 {
		t.Fatalf("compact inbox item is not compact: %d bytes: %s", len(raw), raw)
	}
	legacy, err := json.Marshal(struct {
		ID             string   `json:"id"`
		HandoffID      string   `json:"handoff_id"`
		ChildSessionID string   `json:"child_session_id"`
		CorrelationID  string   `json:"correlation_id,omitempty"`
		Kind           string   `json:"kind"`
		Text           string   `json:"text,omitempty"`
		Next           string   `json:"next"`
		Argv           []string `json:"argv"`
	}{
		ID: msg.ID, HandoffID: msg.HandoffID, ChildSessionID: msg.ChildSessionID,
		CorrelationID: msg.CorrelationID, Kind: msg.Kind, Text: msg.Text,
		Next: item.Next, Argv: item.Argv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= len(legacy) {
		t.Fatalf("compact inbox did not shrink: before=%d after=%d", len(legacy), len(raw))
	}
	t.Logf("serialized_inbox_bytes=%d->%d token_estimate=%d->%d", len(legacy), len(raw), (len(legacy)+3)/4, (len(raw)+3)/4)
}

func TestCompactParentMessageDoesNotRepeatStructuredGateText(t *testing.T) {
	gate := &SecurityGate{Reason: "folder trust required", Directory: "/repo", Choices: []GateChoice{{Index: 1, Label: "Trust"}, {Index: 2, Label: "Exit"}}}
	msg := &ParentMessage{ID: "pm-gate", Kind: "permission_required", Text: formatSecurityGate(gate), Gate: gate}
	item := CompactParentMessage(msg, false)
	if item.Text != "" || item.Gate == nil || len(item.Gate.Choices) != 2 {
		t.Fatalf("structured gate projection = %+v", item)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	withDuplicate := item
	withDuplicate.Text = msg.Text
	legacy, err := json.Marshal(withDuplicate)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= len(legacy) {
		t.Fatalf("structured gate projection did not shrink: before=%d after=%d", len(legacy), len(raw))
	}
	t.Logf("structured_gate_bytes=%d->%d token_estimate=%d->%d", len(legacy), len(raw), (len(legacy)+3)/4, (len(raw)+3)/4)

	unparsed := CompactParentMessage(&ParentMessage{ID: "pm-raw", Kind: "permission_required", Text: "approval required"}, false)
	if unparsed.Text != "approval required" || unparsed.Gate != nil {
		t.Fatalf("unparsed permission lost failure text: %+v", unparsed)
	}
}

func TestParentMessageCarriesFailoverAttribution(t *testing.T) {
	msg := &ParentMessage{
		V: 1, ID: "pm-x", ParentSessionID: "sess-root",
		IntendedParentSessionID: "sess-mid",
		SkippedSessionIDs:       []string{"sess-mid"},
		ResolvedBySessionID:     "sess-root",
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back ParentMessage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.IntendedParentSessionID != "sess-mid" {
		t.Fatalf("intended parent lost: %+v", back)
	}
	if len(back.SkippedSessionIDs) != 1 || back.SkippedSessionIDs[0] != "sess-mid" {
		t.Fatalf("skipped ids lost: %+v", back)
	}
	if back.ResolvedBySessionID != "sess-root" {
		t.Fatalf("resolver lost: %+v", back)
	}
}

func TestParentMessageOmitsFailoverFieldsWhenDeliveredDirectly(t *testing.T) {
	msg := &ParentMessage{V: 1, ID: "pm-y", ParentSessionID: "sess-root"}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"intended_parent_session_id", "skipped_session_ids", "resolved_by_session_id"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("field %s must be omitted when empty: %s", field, raw)
		}
	}
}

// blockingPersistence simulates a dead SSH host: Send hangs until ctx dies.
type blockingPersistence struct {
	renamePersistence
}

func (p *blockingPersistence) Send(ctx context.Context, _ ports.Transport, _ ports.PersistHandle, _ string, _ bool) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestDeliveryAttemptIsBounded(t *testing.T) {
	service, _, reg := newParentTestService(t)
	service.Sessions.Persist = &blockingPersistence{}
	now := time.Now().UTC()
	manager := &Session{ID: "sess-manager", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "manager"}, CreatedAt: now}
	if err := reg.PutSession(manager); err != nil {
		t.Fatal(err)
	}
	ho := &Handoff{ID: "ho-1", SessionID: "sess-child", HostID: "c3", Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now}
	msg := &ParentMessage{V: 1, ID: "pm-block", ParentSessionID: manager.ID, ChildSessionID: "sess-child", HandoffID: ho.ID, Kind: "ask", State: ParentMessagePending, CreatedAt: now}

	start := time.Now()
	err := service.deliverMessage(context.Background(), manager, ho, msg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want delivery error when the transport hangs")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("delivery was not bounded, took %s", elapsed)
	}
}

func TestRedeliverReceiptRequiresConfirmedSessionSend(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "parent"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-result", SessionID: child.ID, HostID: "c1", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	msg := &ParentMessage{V: 1, ID: "pm-result", ParentSessionID: parent.ID, ChildSessionID: child.ID, HandoffID: ho.ID, EventSeq: 9, Kind: "result", Text: "complete", State: ParentMessageAcked, DeliveredAt: &now, AckedAt: &now, DeliveryMethod: deliveryUncertain, DeliveryError: "old ambiguous effect", CreatedAt: now}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	got, err := service.RedeliverReceipt(context.Background(), msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ParentMessageAcked || got.DeliveryMethod != "session_send_confirmed" || got.DeliveryBuild == "" || got.DeliveryError != "" {
		t.Fatalf("redelivery=%+v", got)
	}
}

// failingPersistence simulates an unreachable remote manager.
type failingPersistence struct {
	renamePersistence
	attempts []string
}

func (p *failingPersistence) Send(_ context.Context, _ ports.Transport, handle ports.PersistHandle, _ string, _ bool) error {
	p.attempts = append(p.attempts, handle.Name)
	return &ports.TargetUnavailableError{Err: errors.New("host unreachable")}
}

func failoverTree(t *testing.T, reg *Registry) (root, manager, child *Session, ho *Handoff) {
	t.Helper()
	now := time.Now().UTC()
	root = &Session{
		ID: "sess-root", HostID: LocalHostID,
		Persist:   ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"},
		Labels:    map[string]string{"role": ParentRole, "wake_mode": "notify"},
		CreatedAt: now,
	}
	manager = &Session{
		ID: "sess-manager", HostID: "c1",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "manager"},
		SourceSessionID: root.ID, CreatedAt: now,
	}
	child = &Session{
		ID: "sess-child", HostID: "c3",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "worker"},
		SourceSessionID: manager.ID, CreatedAt: now,
	}
	for _, sess := range []*Session{root, manager, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho = &Handoff{
		ID: "ho-worker", SessionID: child.ID, HostID: child.HostID,
		Kind: KindAgent, Status: StatusRunning, SourceSessionID: manager.ID, CreatedAt: now,
	}
	return root, manager, child, ho
}

func TestEscalationFailsOverToNearestLiveAncestor(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	// The remote manager is unreachable; the local root is live.
	service.Sessions.Persist = &failingPersistence{}
	root, manager, _, ho := failoverTree(t, reg)

	msg, err := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve tool?"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg == nil {
		t.Fatal("want an escalation message")
	}
	if msg.ParentSessionID != root.ID {
		t.Fatalf("want delivery to the live root, got %s", msg.ParentSessionID)
	}
	if msg.IntendedParentSessionID != manager.ID {
		t.Fatalf("want the skipped manager recorded, got %q", msg.IntendedParentSessionID)
	}
	if len(msg.SkippedSessionIDs) != 1 || msg.SkippedSessionIDs[0] != manager.ID {
		t.Fatalf("want the manager in skipped ids, got %+v", msg.SkippedSessionIDs)
	}
	if msg.DeliveredAt == nil {
		t.Fatal("want the escalation delivered")
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("want exactly one human-facing notice, got %d", len(notifier.notices))
	}
	// Exactly one durable envelope must exist across the whole tree.
	rootMsgs, _ := service.ListMessages(root.ID, false)
	managerMsgs, _ := service.ListMessages(manager.ID, false)
	if len(rootMsgs) != 1 || len(managerMsgs) != 0 {
		t.Fatalf("want one envelope held by the root, got root=%d manager=%d", len(rootMsgs), len(managerMsgs))
	}
}

func TestEscalationNeverSkipsALiveManager(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	recorder := &recordingPersistence{}
	service.Sessions.Persist = recorder
	_, manager, _, ho := failoverTree(t, reg)

	msg, err := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve tool?"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ParentSessionID != manager.ID {
		t.Fatalf("a live manager must not be skipped, went to %s", msg.ParentSessionID)
	}
	if msg.IntendedParentSessionID != "" {
		t.Fatalf("no failover expected, got intended=%q", msg.IntendedParentSessionID)
	}
	if len(notifier.notices) != 0 {
		t.Fatalf("the human root must not be interrupted, got %d notices", len(notifier.notices))
	}
	if len(recorder.sent) != 1 {
		t.Fatalf("want one tmux injection to the manager, got %d", len(recorder.sent))
	}
}

func TestEscalationStaysPendingWhenNoAncestorIsLive(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Sessions.Persist = &failingPersistence{}
	notifier.notifyFail = true
	_, _, _, ho := failoverTree(t, reg)

	msg, _ := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve tool?"}})
	if msg == nil {
		t.Fatal("the escalation must still be durably recorded")
	}
	if msg.DeliveredAt != nil {
		t.Fatal("nothing was reachable; it must not be marked delivered")
	}
	if msg.State != ParentMessagePending {
		t.Fatalf("want it left pending for reconnect retry, got %s", msg.State)
	}
	held, err := service.ListMessages(msg.ParentSessionID, true)
	if err != nil {
		t.Fatalf("pending message must be listable: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("want exactly one pending envelope, got %d", len(held))
	}
}

func TestPendingAttentionFindsAskHeldByAnAncestor(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	root, manager, _, _ := failoverTree(t, reg)
	// An unresolved ask for this handoff is already held by the ROOT,
	// because the manager was disconnected when it was raised.
	held := &ParentMessage{
		V: 1, ID: "pm-held", ParentSessionID: root.ID, ChildSessionID: "sess-child",
		HandoffID: "ho-worker", Kind: "ask", State: ParentMessagePending,
		IntendedParentSessionID: manager.ID, CreatedAt: now,
	}
	if err := writeParentMessage(held, true); err != nil {
		t.Fatal(err)
	}

	got, err := service.pendingAttention(manager.ID, "ho-worker")
	if err != nil || got == nil {
		t.Fatal("want the ancestor-held ask to be found from the manager")
	}
	if got.ID != "pm-held" {
		t.Fatalf("want pm-held, got %s", got.ID)
	}
}

func TestReplyRecordsTheResolvingSession(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	root, manager, child, ho := failoverTree(t, reg)
	ho.Status = StatusNeedsInput
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	// The root holds the ask because the manager was disconnected.
	msg := &ParentMessage{
		V: 1, ID: "pm-resolve", ParentSessionID: root.ID, ChildSessionID: child.ID,
		HandoffID: ho.ID, Kind: "ask", State: ParentMessagePending,
		IntendedParentSessionID: manager.ID, CreatedAt: now,
	}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}

	resolved, err := service.Reply(context.Background(), msg.ID, "approved")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ResolvedBySessionID != root.ID {
		t.Fatalf("want the root recorded as resolver, got %q", resolved.ResolvedBySessionID)
	}
	if resolved.State != ParentMessageReplied {
		t.Fatalf("want replied state, got %s", resolved.State)
	}
}

// selectivePersistence fails only for the named tmux handles, so a test can
// down one manager while leaving the rest of the tree reachable.
type selectivePersistence struct {
	renamePersistence
	fail map[string]bool
	sent []string
}

func (p *selectivePersistence) Send(_ context.Context, _ ports.Transport, handle ports.PersistHandle, text string, _ bool) error {
	if p.fail[handle.Name] {
		return &ports.TargetUnavailableError{Err: errors.New("host unreachable")}
	}
	p.sent = append(p.sent, handle.Name+"|"+text)
	return nil
}

// flakyPersistence fails the first N sends to a handle, then succeeds.
type flakyPersistence struct {
	renamePersistence
	remaining int
	sent      []string
}

func (p *flakyPersistence) Send(_ context.Context, _ ports.Transport, handle ports.PersistHandle, text string, _ bool) error {
	if p.remaining > 0 {
		p.remaining--
		return errors.New("transient hiccup")
	}
	p.sent = append(p.sent, handle.Name+"|"+text)
	return nil
}

// A replayed event must never erase a decision the human already recorded.
func TestReplayAfterFailoverDoesNotEraseRecordedDecision(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Sessions.Persist = &selectivePersistence{fail: map[string]bool{"manager": true}}
	_, _, _, ho := failoverTree(t, reg)
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	ev := coord.Event{Seq: 9, Kind: "permission_required", Meta: map[string]any{"text": "delete production bucket?"}}

	msg, err := service.RouteChildEvent(context.Background(), ho, ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reply(context.Background(), msg.ID, "DENY - do not delete"); err != nil {
		t.Fatal(err)
	}

	// The same event is routed again (Watch and AgentWait run on separate cursors).
	replayed, err := service.RouteChildEvent(context.Background(), ho, ev)
	if err != nil {
		t.Fatalf("replay must be a no-op, got %v", err)
	}
	if replayed.State != ParentMessageReplied {
		t.Fatalf("replay erased the decision: state=%s", replayed.State)
	}
	if replayed.Reply != "DENY - do not delete" {
		t.Fatalf("replay erased the reply: %q", replayed.Reply)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("replay re-asked the human: %d notices", len(notifier.notices))
	}
}

// A routine receipt must not walk the tree and interrupt a human.
func TestReceiptsDoNotFailOverToTheHuman(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Sessions.Persist = &selectivePersistence{fail: map[string]bool{"manager": true}}
	_, manager, _, ho := failoverTree(t, reg)

	msg, _ := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 3, Kind: "result", Meta: map[string]any{"text": "build finished"}})

	if len(notifier.notices) != 0 {
		t.Fatalf("a receipt must never reach the human root, got %d notices", len(notifier.notices))
	}
	if msg != nil && msg.ParentSessionID != manager.ID {
		t.Fatalf("a receipt must stay with its manager, went to %s", msg.ParentSessionID)
	}
}

func TestNoteAndProgressAdvanceWithoutManagerWake(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	_, manager, _, ho := failoverTree(t, reg)
	persist := &selectivePersistence{}
	service.Sessions.Persist = persist

	for seq, kind := range []string{"note", "progress"} {
		msg, err := service.RouteChildEvent(context.Background(), ho,
			coord.Event{Seq: int64(seq + 1), Kind: kind, Meta: map[string]any{"text": kind + " update"}})
		if err != nil {
			t.Fatal(err)
		}
		if msg != nil {
			t.Fatalf("%s allocated a manager envelope: %+v", kind, msg)
		}
	}
	if len(persist.sent) != 0 {
		t.Fatalf("manager deliveries = %v", persist.sent)
	}
	if len(notifier.notices) != 0 {
		t.Fatalf("informational updates reached the human root: %d", len(notifier.notices))
	}
	_ = manager
}

func TestLifecycleCommunicationMeasurement(t *testing.T) {
	service, _, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	_, manager, _, ho := failoverTree(t, reg)
	persist := &selectivePersistence{}
	service.Sessions.Persist = persist
	events := []coord.Event{
		{Seq: 1, Kind: "progress", Meta: map[string]any{"text": "taxonomy frozen"}},
		{Seq: 2, Kind: "note", Meta: map[string]any{"text": "receipt stored"}},
		{Seq: 3, Kind: "result", Meta: map[string]any{"source": "hook", "text": "stopped at composer"}},
		{Seq: 4, Kind: "result", Meta: map[string]any{"source": "hook", "text": "stopped at composer"}},
		{Seq: 5, Kind: "result", Meta: map[string]any{"source": "hook", "text": "stopped at composer"}},
		{Seq: 6, Kind: "ask", Meta: map[string]any{"text": "choose dataset A or B"}},
		{Seq: 7, Kind: "result", Meta: map[string]any{"text": "paired canary complete"}},
		{Seq: 8, Kind: "exit", Meta: map[string]any{"text": "agent exited"}},
	}
	legacyBytes, legacyWakeups := 0, 0
	for _, ev := range events {
		kind := attentionKind(ev)
		if kind != "" && ev.Kind != "exit" { // legacy already coalesced result+exit
			text := eventText(&ev)
			legacy := fmt.Sprintf("[relay %s %s %s] %s", kind, "worker@c1", ho.ID, compactText(text))
			if attentionMessage(kind) {
				legacy = fmt.Sprintf("[relay %s %s %s %s] %s | relay resolve pm-legacy <decision>", kind, "pm-legacy", "worker@c1", ho.ID, compactText(text))
			}
			legacyBytes += len(legacy)
			legacyWakeups++
		}
		if _, err := service.RouteChildEvent(context.Background(), ho, ev); err != nil {
			t.Fatal(err)
		}
	}
	currentBytes := 0
	for _, sent := range persist.sent {
		currentBytes += len(strings.SplitN(sent, "|", 2)[1])
	}
	messages, err := service.ListMessages(manager.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if legacyWakeups != 7 || len(persist.sent) != 2 || len(messages) != 3 {
		t.Fatalf("lifecycle counts: legacy_wakes=%d current_wakes=%d envelopes=%d", legacyWakeups, len(persist.sent), len(messages))
	}
	if currentBytes >= legacyBytes {
		t.Fatalf("manager bytes did not shrink: before=%d after=%d", legacyBytes, currentBytes)
	}
	t.Logf("events=8 envelopes=8->3 wakeups=7->2 manager_bytes=%d->%d token_estimate=%d->%d retry_opportunities=7->2", legacyBytes, currentBytes, (legacyBytes+3)/4, (currentBytes+3)/4)
}

func TestConcurrentAskFramesCreateOneSemanticEnvelope(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "parent"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, SourceSessionID: parent.ID, CreatedAt: now}
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	_ = reg.PutHandoff(ho)

	const frames = 24
	start := make(chan struct{})
	errs := make(chan error, frames)
	var wg sync.WaitGroup
	for i := 1; i <= frames; i++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
			<-start
			_, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: seq, Kind: "ask", Meta: map[string]any{"text": "choose A or B"}})
			errs <- err
		}(int64(i))
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	messages, err := service.ListMessages(parent.ID, false)
	if err != nil || len(messages) != 1 || len(notifier.notices) != 1 {
		t.Fatalf("concurrent semantic replay messages=%d notices=%d err=%v", len(messages), len(notifier.notices), err)
	}
}

func TestApexRootSignalRoutesDurablyToHumanSurface(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	apex := &Session{ID: "sess-apex", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: "tmux", Name: "apex"}, Labels: map[string]string{ApexLabel: "true"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-apex", SessionID: apex.ID, HostID: apex.HostID, Kind: KindAgent, Status: StatusRunning, CreatedAt: now}
	_ = reg.PutSession(apex)
	_ = reg.PutHandoff(ho)

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 708, Kind: "ask", Meta: map[string]any{"text": "repair governed edge"}})
	if err != nil || msg == nil || msg.ParentSessionID != apex.ID || msg.DeliveredAt == nil || msg.DeliveryMethod != "human_notification_confirmed" {
		t.Fatalf("root authority route msg=%+v err=%v", msg, err)
	}
	if len(notifier.notices) != 1 || len(notifier.sent) != 0 {
		t.Fatalf("root route notifications=%d pane_sends=%d", len(notifier.notices), len(notifier.sent))
	}
	stored, listErr := service.ListMessages(apex.ID, false)
	if listErr != nil || len(stored) != 1 || stored[0].EventSeq != 708 {
		t.Fatalf("durable root receipt=%+v err=%v", stored, listErr)
	}
	result, resultErr := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 710, Kind: "result", Meta: map[string]any{"text": "authority repair complete"}})
	if resultErr != nil || result == nil || result.DeliveredAt == nil || len(notifier.notices) != 2 {
		t.Fatalf("root result route=%+v notices=%d err=%v", result, len(notifier.notices), resultErr)
	}
}

func TestBlockedImmediateManagerIsNeverSkipped(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Sessions.Persist = &capturePersistence{capture: "Do you trust the contents of this directory?\n1. Yes\n2. No"}
	_, manager, _, ho := failoverTree(t, reg)

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "ordinary choice"}})
	if err == nil || msg == nil {
		t.Fatalf("blocked parent must leave a durable pending envelope: msg=%+v err=%v", msg, err)
	}
	if msg.ParentSessionID != manager.ID || msg.DeliveredAt != nil || len(notifier.notices) != 0 {
		t.Fatalf("blocked manager was bypassed: msg=%+v human_notices=%d", msg, len(notifier.notices))
	}
	if msg.DeliveryAttempts != 0 {
		t.Fatalf("readiness failure must not reserve a mutating attempt: %+v", msg)
	}
}

func TestTelemetryDoesNotProbeOrInjectIntoAbsentManager(t *testing.T) {
	service, _, reg := newParentTestService(t)
	parent := &Session{ID: "sess-manager", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "manager"}, CreatedAt: time.Now().UTC()}
	child := &Session{ID: "sess-child", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, SourceSessionID: parent.ID, CreatedAt: time.Now().UTC()}
	for _, sess := range []*Session{parent, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: "self", SourceSessionID: parent.ID, Kind: KindAgent, Status: StatusRunning, CreatedAt: time.Now().UTC()}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	persist := &selectivePersistence{}
	service.Sessions.Persist = &capturePersistence{renamePersistence: persist.renamePersistence, capture: "dostos@home:~/dev/relay$ "}

	msg, err := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "note", Meta: map[string]any{"text": "must not execute in shell"}})
	if err != nil || msg != nil {
		t.Fatalf("telemetry allocated a manager delivery: msg=%+v err=%v", msg, err)
	}
}

// A live manager having a transient hiccup must not be bypassed.
func TestTransientFailureDoesNotBypassALiveManager(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	flaky := &flakyPersistence{remaining: 1}
	service.Sessions.Persist = flaky
	_, manager, _, ho := failoverTree(t, reg)

	msg, err := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve?"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ParentSessionID != manager.ID {
		t.Fatalf("a transient error bypassed a live manager, went to %s", msg.ParentSessionID)
	}
	if len(notifier.notices) != 0 {
		t.Fatalf("the human must not be interrupted, got %d notices", len(notifier.notices))
	}
	if len(flaky.sent) != 1 {
		t.Fatalf("want the retry to reach the manager, got %d sends", len(flaky.sent))
	}
}

// When nothing is reachable the envelope must stay with its intended manager,
// not be parked on an ancestor that never saw it.
func TestUnreachableChainLeavesEnvelopeWithIntendedManager(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	service.Sessions.Persist = &failingPersistence{}
	notifier.notifyFail = true
	_, manager, _, ho := failoverTree(t, reg)

	msg, _ := service.RouteChildEvent(context.Background(), ho,
		coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve?"}})
	if msg == nil {
		t.Fatal("the escalation must still be recorded")
	}
	if msg.ParentSessionID != manager.ID {
		t.Fatalf("envelope parked on a session that never received it: %s", msg.ParentSessionID)
	}
	held, err := service.ListMessages(manager.ID, true)
	if err != nil || len(held) != 1 {
		t.Fatalf("intended manager must hold it for reconnect retry: %d (%v)", len(held), err)
	}
}

// changingPersistence returns a different capture each call, as a working
// agent's pane does while it redraws.
type changingPersistence struct {
	renamePersistence
	calls int
}

func (p *changingPersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	p.calls++
	return fmt.Sprintf("Cursor Grok 4.5 High Fast · %d%% · %d files edited", p.calls*7, p.calls), nil
}

// steadyPersistence returns the same waiting prompt every call.
type steadyPersistence struct {
	renamePersistence
	calls int
}

func (p *steadyPersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	p.calls++
	return "Should I delete the staging bucket?\n> _", nil
}

// A busy pane whose UI strings paneStillActive does not recognise must still
// not raise an ask. This is the cursor-agent case: its status line matches
// none of the known "working" markers.
func TestIdleOnAChangingPaneRaisesNoAsk(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	changing := &changingPersistence{}
	service.Sessions.Persist = changing
	_, manager, _, ho := failoverTree(t, reg)

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	if msg != nil {
		t.Fatalf("a pane that is still redrawing must not raise an ask: %+v", msg)
	}
	if len(notifier.notices) != 0 {
		t.Fatalf("nobody should have been interrupted, got %d notices", len(notifier.notices))
	}
	if changing.calls != 1 {
		t.Fatalf("idle safety classification should use one read, got %d", changing.calls)
	}
	_ = manager
}

// A settled composer is ambiguous. The child must declare a real question with
// relay ask; an idle sample alone only advances the durable cursor.
func TestIdleOnASettledPaneDoesNotInventAnAsk(t *testing.T) {
	service, _, reg := newParentTestService(t)
	service.Sessions.Persist = &steadyPersistence{}
	_, manager, _, ho := failoverTree(t, reg)

	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	if msg != nil {
		t.Fatalf("idle invented an ask: %+v", msg)
	}
	_ = manager
}
