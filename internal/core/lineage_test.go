package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/ports"
)

func TestLoadAndFormatHistory(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	root := &Session{
		ID: "sess-root", HostID: "c3", Persist: ports.PersistHandle{Name: "research"},
		Labels: map[string]string{"role": "interactive", "agent": "human"}, CreatedAt: now,
	}
	child := &Session{
		ID: "sess-child", HostID: "c1", Persist: ports.PersistHandle{Name: "agent-run"},
		Labels: map[string]string{"role": "handoff", "agent": "codex"}, CreatedAt: now.Add(time.Second),
		SourceSessionID: root.ID, CreatedByHandoffID: "ho-test",
	}
	if err := AppendSessionStart(root); err != nil {
		t.Fatal(err)
	}
	if err := AppendSessionStart(child); err != nil {
		t.Fatal(err)
	}
	graph, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("unexpected graph: %+v", graph)
	}
	text := FormatHistory(graph)
	for _, want := range []string{"c3/research (human)", "ho-test", "c1/agent-run (codex)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("history missing %q:\n%s", want, text)
		}
	}
}

func TestCommunicationPageIsCompactAndCursorBased(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	first := &ParentMessage{
		ID: "pm-1", CorrelationID: "corr-1", ParentSessionID: "sess-parent",
		ChildSessionID: "sess-child", HandoffID: "ho-1", Kind: "ask",
		Text: strings.Repeat("inspect benchmark evidence ", 20),
	}
	if err := AppendCommunication(first, "request", ""); err != nil {
		t.Fatal(err)
	}
	if err := AppendCommunication(first, "resolve", "continue with observability only"); err != nil {
		t.Fatal(err)
	}
	if err := AppendCommunication(&ParentMessage{
		ID: "pm-other", CorrelationID: "corr-other", ParentSessionID: "sess-other",
		ChildSessionID: "sess-other-child", HandoffID: "ho-other", Kind: "result", Text: "done",
	}, "request", ""); err != nil {
		t.Fatal(err)
	}

	page, err := LoadCommunicationPage("sess-parent", "", 0, 1)
	if err != nil || len(page.Entries) != 1 || !page.HasMore || page.NextAfter != 1 {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	entry := page.Entries[0]
	if entry.MessageID != "pm-1" || entry.Action != "request" || entry.Summary == "" || len(entry.Summary) > 243 {
		t.Fatalf("compact entry=%+v", entry)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(struct {
		Seq           int64  `json:"seq"`
		MessageID     string `json:"message_id"`
		CorrelationID string `json:"correlation_id,omitempty"`
		ChildSession  string `json:"child_session_id"`
		HandoffID     string `json:"handoff_id,omitempty"`
		Kind          string `json:"kind"`
		Action        string `json:"action"`
		Summary       string `json:"summary,omitempty"`
	}{1, entry.MessageID, "corr-1", "sess-child", "ho-1", entry.Kind, entry.Action, entry.Summary})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= len(legacy) {
		t.Fatalf("communication delta did not shrink: before=%d after=%d", len(legacy), len(raw))
	}
	t.Logf("communication_delta_bytes=%d->%d token_estimate=%d->%d", len(legacy), len(raw), (len(legacy)+3)/4, (len(raw)+3)/4)

	next, err := LoadCommunicationPage("sess-parent", "ho-1", page.NextAfter, 20)
	if err != nil || len(next.Entries) != 1 || next.Entries[0].Action != "resolve" || next.NextAfter != 3 || next.HasMore {
		t.Fatalf("next page=%+v err=%v", next, err)
	}
	empty, err := LoadCommunicationPage("sess-parent", "", next.NextAfter, 20)
	if err != nil || len(empty.Entries) != 0 || empty.NextAfter != 3 {
		t.Fatalf("empty page=%+v err=%v", empty, err)
	}
}

func TestCommunicationPageHidesAcknowledgementMechanics(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	dropped := &ParentMessage{
		ID: "pm-drop", CorrelationID: "pm-drop", ParentSessionID: "sess-parent",
		ChildSessionID: "sess-child", HandoffID: "ho-1", Kind: "exit", Text: "duplicate exit",
	}
	if err := AppendCommunication(dropped, "event", ""); err != nil {
		t.Fatal(err)
	}
	dropped.PolicyID, dropped.AutoHandled = "builtin.coalesce_result_exit", true
	if err := AppendCommunication(dropped, "ack", ""); err != nil {
		t.Fatal(err)
	}
	result := &ParentMessage{
		ID: "pm-result", CorrelationID: "pm-result", ParentSessionID: "sess-parent",
		ChildSessionID: "sess-child", HandoffID: "ho-1", Kind: "result", Text: "done",
	}
	if err := AppendCommunication(result, "event", ""); err != nil {
		t.Fatal(err)
	}
	if err := AppendCommunication(result, "ack", ""); err != nil {
		t.Fatal(err)
	}
	page, err := LoadCommunicationPage("sess-parent", "", 0, 20)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].MessageID != "pm-result" || page.NextAfter != 4 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestRelaySessionCommandCarriesBridgeIdentity(t *testing.T) {
	got := relaySessionCommand("bash -l", "sess-123", "c3", "named", "br-secret")
	for _, want := range []string{"RELAY_SESSION_ID='sess-123'", "RELAY_SESSION_HOST='c3'", "RELAY_SESSION_NAME='named'", "RELAY_SOURCE_TOKEN='br-secret'", BridgeRemoteSocket("sess-123")} {
		if !strings.Contains(got, want) {
			t.Fatalf("command missing %q: %s", want, got)
		}
	}
}

func TestAuthorizeBridgeSource(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	if err := (&Registry{}).PutSession(&Session{
		ID: "sess-source", HostID: "c3", Persist: ports.PersistHandle{Name: "source"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rememberBridgeToken("sess-source", "br-secret"); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeBridgeSource(bridge.Source{SessionID: "sess-source", Token: "br-secret"}); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	if err := AuthorizeBridgeSource(bridge.Source{SessionID: "sess-source", Token: "wrong"}); err == nil {
		t.Fatal("expected wrong token to be rejected")
	}
}

func TestLoadBridgeIdentityForPersist(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	if err := EnsureStateDirs(); err != nil {
		t.Fatal(err)
	}
	want := BridgeIdentity{V: 1, SessionID: "sess-adopted", HostID: "c3", PersistName: "beholder", Socket: "/tmp/relay.sock", Token: "br-secret"}
	raw, _ := json.Marshal(want)
	if err := os.WriteFile(filepath.Join(BridgeIdentitiesDir(), "sess-adopted.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadBridgeIdentityForPersist("beholder")
	if err != nil {
		t.Fatal(err)
	}
	if *got != want {
		t.Fatalf("identity = %+v", got)
	}
	if _, err := loadBridgeIdentityForPersist("other"); err == nil {
		t.Fatal("expected unknown tmux session to fail closed")
	}
}
