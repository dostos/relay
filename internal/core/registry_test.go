package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestRegistrySessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	r := &Registry{}
	now := time.Now().UTC()
	s := &Session{
		ID:        "sess-test",
		HostID:    "c1",
		RemoteCWD: "~/gh/relay",
		Persist:   ports.PersistHandle{Kind: "tmux", Name: "relay-1"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.PutSession(s); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetSession("sess-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.HostID != "c1" || got.Persist.Name != "relay-1" {
		t.Fatalf("mismatch: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err != nil {
		t.Fatal(err)
	}
}

func TestAppendLedger(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	if err := AppendLedger(map[string]any{"type": "start", "v": 1}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "handoffs", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("empty ledger")
	}
}
