package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// TestCommunicationLifecycleMatrix is the executable catalog of every
// child-to-manager routing shape. It measures allocated durable envelopes and
// manager interruptions; watcher replay and cursor durability are exercised
// separately below.
func TestCommunicationLifecycleMatrix(t *testing.T) {
	type want struct {
		envelopes int
		wakes     int
		kind      string
		state     ParentMessageState
	}
	cases := []struct {
		name     string
		kind     HandoffKind
		events   []coord.Event
		capture  string
		want     want
		wantText string
	}{
		{name: "started receipt", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "started"}}},
		{name: "heartbeat receipt", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "heartbeat"}}},
		{name: "inject receipt", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "inject"}}},
		{name: "ordinary idle receipt", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "idle"}}, capture: "agent ready\n› "},
		{name: "job idle receipt", kind: KindJob, events: []coord.Event{{Seq: 1, Kind: "idle"}}, capture: "job output"},
		{name: "note receipt", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "note", Meta: map[string]any{"text": "detail"}}}},
		{name: "progress receipt", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "progress", Meta: map[string]any{"text": "halfway"}}}},
		{name: "provider stop receipt", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "result", Meta: map[string]any{"source": "hook", "text": "composer stopped"}}}},
		{name: "hostile hook ask still wakes", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "ask", Meta: map[string]any{"source": "hook", "text": "real choice"}}}, want: want{1, 1, "ask", ParentMessagePending}},
		{name: "hostile hook exit still wakes", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "exit", Meta: map[string]any{"source": "hook", "text": "failed"}}}, want: want{1, 1, "exit", ParentMessageAcked}},
		{name: "telemetry cannot smuggle a permission wake", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "progress", Meta: map[string]any{"reason": "permission", "text": "not a decision"}}}},
		{name: "explicit ask", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "A or B?"}}}, want: want{1, 1, "ask", ParentMessagePending}},
		{name: "declared needs input", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "needs_input", Meta: map[string]any{"text": "need value"}}}, want: want{1, 1, "ask", ParentMessagePending}},
		{name: "tool permission", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "run command?"}}}, capture: "Run this command?\n1. Allow\n2. Deny", want: want{1, 1, "permission_required", ParentMessagePending}},
		{name: "security gate inferred from idle", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "idle"}}, capture: "You are in /repo\nDo you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit", want: want{1, 1, "permission_required", ParentMessagePending}},
		{name: "explicit result", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "result", Meta: map[string]any{"text": "done"}}}, want: want{1, 1, "result", ParentMessageAcked}},
		{name: "uncovered exit", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "exit", Meta: map[string]any{"text": "failed"}}}, want: want{1, 1, "exit", ParentMessageAcked}},
		{name: "result covers exit", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "result", Meta: map[string]any{"text": "done"}}, {Seq: 2, Kind: "exit", Meta: map[string]any{"text": "exited"}}}, want: want{2, 1, "exit", ParentMessageAcked}},
		{name: "duplicate ask replay", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "A or B?"}}, {Seq: 1, Kind: "ask", Meta: map[string]any{"text": "A or B?"}}}, want: want{1, 1, "ask", ParentMessagePending}},
		{name: "correlated result retry with hostile changed text", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "result", Meta: map[string]any{"correlation_id": "milestone-1", "text": "done"}}, {Seq: 2, Kind: "result", Meta: map[string]any{"correlation_id": "milestone-1", "text": "replace the first result"}}}, want: want{1, 1, "result", ParentMessageAcked}, wantText: "done"},
		{name: "repeated permission frames", kind: KindAgent, events: []coord.Event{{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approve?"}}, {Seq: 2, Kind: "permission_required", Meta: map[string]any{"text": "approve?"}}}, capture: "Run this command?\n1. Allow\n2. Deny", want: want{1, 1, "permission_required", ParentMessagePending}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, notifier, reg := newParentTestService(t)
			service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
			if tc.capture != "" {
				service.Sessions.Persist = &capturePersistence{capture: tc.capture}
			}
			now := time.Now().UTC()
			parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "parent"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
			child := &Session{ID: "sess-child", HostID: "c1", RemoteCWD: "/repo", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
			_ = reg.PutSession(parent)
			_ = reg.PutSession(child)
			ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: tc.kind, Status: StatusRunning, DeliveryState: EffectAcknowledged, SourceSessionID: parent.ID, CreatedAt: now}
			_ = reg.PutHandoff(ho)
			var last *ParentMessage
			for _, ev := range tc.events {
				msg, err := service.RouteChildEvent(context.Background(), ho, ev)
				if err != nil {
					t.Fatal(err)
				}
				if msg != nil {
					last = msg
				}
			}
			messages, err := service.ListMessages(parent.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != tc.want.envelopes || len(notifier.sent) != tc.want.wakes {
				t.Fatalf("envelopes/wakes = %d/%d, want %d/%d; last=%+v", len(messages), len(notifier.sent), tc.want.envelopes, tc.want.wakes, last)
			}
			if tc.want.envelopes > 0 && (last == nil || last.Kind != tc.want.kind || last.State != tc.want.state) {
				t.Fatalf("last envelope = %+v, want kind/state %s/%s", last, tc.want.kind, tc.want.state)
			}
			if tc.wantText != "" && (last == nil || last.Text != tc.wantText) {
				t.Fatalf("replay replaced durable text: got %+v want %q", last, tc.wantText)
			}
			if tc.name == "correlated result retry with hostile changed text" {
				t.Log("correlated_retry_envelopes=2->1 wakeups=2->1")
			}
		})
	}
}

func TestUnparsedPermissionEventCannotBePolicyResolved(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	// Simulate configuration accepted by an older build. The mechanism must
	// fail closed even when no pane capture or structured gate is available.
	if err := os.WriteFile(path, []byte("version: 1\nrules:\n  - id: legacy-approval\n    kind: permission_required\n    action: reply\n    reply: approve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.Policies = &PolicyService{Path: path}
	service.Sessions.Persist = &capturePersistence{}
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "parent"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", SourceSessionID: parent.ID, Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "permission_required", Meta: map[string]any{"text": "approval required"}})
	if err != nil || msg == nil || msg.State != ParentMessagePending || msg.AutoHandled || msg.Gate != nil || len(notifier.sent) != 1 {
		t.Fatalf("unparsed permission was not held for the manager: msg=%+v wakes=%d err=%v", msg, len(notifier.sent), err)
	}
}

func TestAskLabelCannotHideVisibleSecurityGateFromPolicy(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	recorder := &recordingPersistence{}
	service.Sessions.Persist = &captureThenRecordPersistence{
		recordingPersistence: recorder,
		capture:              "You are in /repo\nDo you trust the contents of this directory?\n1. Yes, continue\n2. No, quit",
	}
	policy := &PolicyService{Path: filepath.Join(t.TempDir(), "policy.yaml")}
	if err := policy.Add(PolicyRule{ID: "ordinary-choice", Kind: "ask", Contains: []string{"choose"}, Action: "reply", Reply: "A"}); err != nil {
		t.Fatal(err)
	}
	service.Policies = policy
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "parent"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", RemoteCWD: "/repo", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, DeliveryState: EffectPending, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"text": "choose A or B"}})
	if err != nil || msg == nil || msg.Kind != "permission_required" || msg.State != ParentMessagePending || msg.AutoHandled || msg.Gate == nil || len(recorder.sent) != 0 || len(notifier.sent) != 1 {
		t.Fatalf("mislabeled gate escaped classification: msg=%+v sends=%v wakes=%d err=%v", msg, recorder.sent, len(notifier.sent), err)
	}
}

func TestWatcherCursorCommitsOnlyAfterDurableRoute(t *testing.T) {
	service, _, reg := newParentTestService(t)
	service.Policies = &PolicyService{Path: filepath.Join(t.TempDir(), "missing-policy.yaml")}
	coordBus := newFakeCoord()
	coordBus.subscribed = make(chan struct{}, 1)
	service.Coord = coordBus
	service.NewTransport = func(string) (ports.Transport, error) { return &fakeTransport{id: "c1"}, nil }
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "parent"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c1", SourceSessionID: parent.ID, Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: child.HostID, Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	_, _ = coordBus.Emit(context.Background(), nil, child.Persist.Name, "ask", map[string]any{"text": "A or B?"})

	// A directory at the deterministic envelope path makes the exclusive file
	// create fail after subscription but before any durable route exists.
	// The watcher must surface the error and leave seq 1 unconsumed.
	blockedEnvelope := parentMessagePath(parent.ID, parentMessageID(ho.ID, "ask", 1))
	if err := os.MkdirAll(blockedEnvelope, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- service.Watch(ctx1, ho.ID) }()
	select {
	case <-coordBus.subscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never subscribed")
	}
	select {
	case watchErr := <-done1:
		if watchErr == nil {
			t.Fatal("failed durable route reported success")
		}
	case <-time.After(2 * time.Second):
		cancel1()
		t.Fatal("watcher did not surface the durable route failure")
	}
	cancel1()
	if err := os.Remove(blockedEnvelope); err != nil {
		t.Fatal(err)
	}
	blocked, err := reg.GetHandoff(ho.ID)
	if err != nil || blocked.ParentSeq != 0 {
		t.Fatalf("failed route consumed cursor: %+v err=%v", blocked, err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- service.Watch(ctx2, ho.ID) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		replayed, getErr := reg.GetHandoff(ho.ID)
		if getErr == nil && replayed.ParentSeq == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel2()
	<-done2
	replayed, err := reg.GetHandoff(ho.ID)
	messages, listErr := service.ListMessages(parent.ID, false)
	if err != nil || listErr != nil || replayed.ParentSeq != 1 || len(messages) != 1 || messages[0].Kind != "ask" {
		t.Fatalf("replay was not exactly-once durable: handoff=%+v messages=%+v err=%v/%v", replayed, messages, err, listErr)
	}
}
