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
	info, err := os.Stat(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sessions mode=%o, want 600", info.Mode().Perm())
	}
	root, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if root.Mode().Perm() != 0o700 {
		t.Fatalf("state root mode=%o, want 700", root.Mode().Perm())
	}
}

func TestRegistryRejectsInvalidPersistedTmuxTarget(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	err := (&Registry{}).PutSession(&Session{
		ID: "sess-bad", HostID: "home",
		Persist: ports.PersistHandle{Name: "safe:other"},
	})
	if err == nil {
		t.Fatal("registry accepted tmux target delimiters")
	}
}

func TestAppendLedger(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "handoffs"), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "handoffs", "ledger.jsonl")
	if err := os.WriteFile(ledger, []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ledger, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendLedger(map[string]any{"type": "start", "v": 1}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("empty ledger")
	}
	info, err := os.Stat(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode=%o, want 600", info.Mode().Perm())
	}
}
