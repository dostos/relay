package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

type fakeParentNotifier struct {
	bound      []string
	notices    []ParentNotice
	notifyFail bool
}

type fakeRetirementViz struct {
	closed    []string
	reparents map[string]string
}

type recordingPersistence struct {
	renamePersistence
	sent []string
}

type capturePersistence struct {
	renamePersistence
	capture string
}

func (p *capturePersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return p.capture, nil
}

func (p *recordingPersistence) Send(_ context.Context, _ ports.Transport, _ ports.PersistHandle, text string, _ bool) error {
	p.sent = append(p.sent, text)
	return nil
}

func (f *fakeRetirementViz) Kind() string                   { return "test" }
func (f *fakeRetirementViz) Available(context.Context) bool { return true }
func (f *fakeRetirementViz) Present(context.Context, string, string, ports.Layout) (string, error) {
	return "", nil
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
func (f *fakeRetirementViz) ReparentBinding(childSessionID, parentSessionID string) error {
	if f.reparents == nil {
		f.reparents = map[string]string{}
	}
	f.reparents[childSessionID] = parentSessionID
	return nil
}

func (f *fakeParentNotifier) BindLocalParent(_ context.Context, sessionID, surface string) (string, error) {
	f.bound = append(f.bound, sessionID+"@"+surface)
	return surface, nil
}
func (f *fakeParentNotifier) NotifyParent(_ context.Context, _ string, notice ParentNotice) error {
	f.notices = append(f.notices, notice)
	if f.notifyFail {
		return errors.New("parent disconnected")
	}
	return nil
}

func newParentTestService(t *testing.T) (*ParentService, *fakeParentNotifier, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	notifier := &fakeParentNotifier{}
	sessions := &SessionService{Reg: reg, Persist: &renamePersistence{}, NewTransport: func(host string) (ports.Transport, error) {
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
	if msg.Kind != "permission_required" || msg.CorrelationID != "req-7" || len(msg.Text) > parentTextLimit {
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
	if len(recorder.sent) != 1 || !strings.Contains(recorder.sent[0], "relay parent reply "+msg.ID) {
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
	if len(got) > parentTextLimit {
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
	service.Sessions.Persist = &capturePersistence{capture: "Run this command?\nNot in allowlist: echo, hostname, test\n"}
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

func TestDisconnectedParentRetriesOneDurableAttentionEnvelope(t *testing.T) {
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
	first, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "idle"})
	if err == nil || first.DeliveredAt != nil {
		t.Fatalf("disconnected first=%+v err=%v", first, err)
	}
	notifier.notifyFail = false
	second, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "idle"})
	if err != nil || second.ID != first.ID || second.DeliveredAt == nil {
		t.Fatalf("reconnect second=%+v err=%v", second, err)
	}
	messages, err := service.ListMessages(parent.ID, false)
	if err != nil || len(messages) != 1 || len(notifier.notices) != 2 {
		t.Fatalf("messages=%+v notices=%d err=%v", messages, len(notifier.notices), err)
	}
}

func TestFormatParentNoticeQualifiesRemoteHandoff(t *testing.T) {
	got := FormatParentNotice(ParentNotice{MessageID: "pm-1", HandoffID: "ho-1", Kind: "ask", Child: "worker@cancun", Text: "inspect remote", Action: "reply"})
	if !strings.Contains(got, "child=worker@cancun handoff=ho-1") || !strings.Contains(got, "relay parent reply pm-1") {
		t.Fatalf("notice lacks remote routing context: %q", got)
	}
}

func TestReparentChildMovesPendingInboxAndHistoryEdge(t *testing.T) {
	service, _, reg := newParentTestService(t)
	viz := &fakeRetirementViz{}
	service.Viz = viz
	now := time.Now().UTC()
	oldParent := &Session{ID: "sess-old-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "beholder"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	newParent := &Session{ID: "sess-new-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "beholder-pdf"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
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
	if viz.reparents[child.ID] != newParent.ID {
		t.Fatalf("pane parent=%q", viz.reparents[child.ID])
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
	if viz.reparents[child.ID] != parent.ID {
		t.Fatalf("pane binding not refreshed: %v", viz.reparents)
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
		{V: 1, ID: "pm-live", ParentSessionID: parent.ID, ChildSessionID: live.SessionID, HandoffID: live.ID, Kind: "ask", State: ParentMessagePending, CreatedAt: now.Add(2 * time.Second)},
		{V: 1, ID: "pm-unknown", ParentSessionID: parent.ID, ChildSessionID: "sess-missing", HandoffID: "ho-missing", Kind: "idle", State: ParentMessagePending, CreatedAt: now.Add(3 * time.Second)},
	} {
		if err := writeParentMessage(msg, true); err != nil {
			t.Fatal(err)
		}
	}
	acked, byHandoff, err := service.SweepTerminal(parent.ID)
	if err != nil || acked != 2 || byHandoff[terminal.ID] != 2 {
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
	if err != nil || second.ID != first.ID || second.State != ParentMessagePending {
		t.Fatalf("idle=%+v err=%v", second, err)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("redundant idle notified root: %d", len(notifier.notices))
	}
}

func TestPolicyAutoReplyIsAuditedAndSkipsManagerPing(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	recorder := &recordingPersistence{}
	service.Sessions.Persist = recorder
	policy := &PolicyService{Path: filepath.Join(t.TempDir(), "policy.yaml")}
	if err := policy.Add(PolicyRule{ID: "cursor-safe-read", Kind: "permission_required", Agent: "cursor-agent", Contains: []string{"git status"}, Action: "reply", Reply: "y"}); err != nil {
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
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "Run git status?", "command": "git status"}})
	if err != nil || msg.State != ParentMessageReplied || msg.Reply != "y" || !msg.AutoHandled || msg.PolicyID != "cursor-safe-read" {
		t.Fatalf("message=%+v err=%v", msg, err)
	}
	if len(notifier.notices) != 0 || len(recorder.sent) != 1 || recorder.sent[0] != "y" {
		t.Fatalf("notices=%d sent=%v", len(notifier.notices), recorder.sent)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Communications) != 2 || graph.Communications[1].PolicyID != "cursor-safe-read" || !graph.Communications[1].AutoHandled {
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
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve?"}})
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
		if msg.Kind != kind || msg.CorrelationID != correlation || msg.State != ParentMessagePending {
			t.Fatalf("%s message = %+v", kind, msg)
		}
		wantAction := "ack"
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

func TestReplyAndAckCloseInboxWithCorrelatedHistory(t *testing.T) {
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
	acked, err := service.Ack(result.ID)
	if err != nil || acked.State != ParentMessageAcked {
		t.Fatalf("ack=%+v err=%v", acked, err)
	}
	pending, _ := service.ListMessages(parent.ID, true)
	if len(pending) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
	graph, _ := LoadHistory()
	if len(graph.Communications) != 4 {
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
	retired, err := service.Retire(context.Background(), parent.ID, false)
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
	for _, redundant := range []string{"parent_session_id", "event_seq", "created_at", "\"state\""} {
		if strings.Contains(text, redundant) {
			t.Fatalf("compact inbox leaked %s: %s", redundant, text)
		}
	}
	if item.Next != "reply" || len(item.Argv) == 0 || item.Argv[len(item.Argv)-1] != "<decision>" {
		t.Fatalf("compact inbox omitted executable decision: %s", text)
	}
	if len(raw) > 300 {
		t.Fatalf("compact inbox item is not compact: %d bytes: %s", len(raw), raw)
	}
}
