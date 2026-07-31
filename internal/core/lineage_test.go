package core

import (
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
