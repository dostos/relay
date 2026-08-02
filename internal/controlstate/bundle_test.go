package controlstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func TestBundleMovesControlStateButNotVizBindings(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	t.Setenv("RELAY_STATE_DIR", source)
	reg := &core.Registry{}
	now := time.Now().UTC()
	sess := &core.Session{ID: "sess-1", HostID: "home-relay", Persist: ports.PersistHandle{Kind: "tmux", Name: "agent-1"}, CreatedAt: now, UpdatedAt: now}
	ho := &core.Handoff{ID: "ho-1", SessionID: sess.ID, HostID: sess.HostID, Kind: core.KindAgent, Status: core.StatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(core.BridgeTokensDir(), "sess-1.token"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(core.ParentInboxDir(), "sess-parent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(core.ParentInboxDir(), "sess-parent", "pm-1.json"), []byte(`{"id":"pm-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "viz"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "viz", "sess-1.json"), []byte(`{"surface":"surface:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	bundle, err := Export(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("RELAY_STATE_DIR", destination); err != nil {
		t.Fatal(err)
	}
	summary, err := Import(&core.Registry{}, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sessions != 1 || summary.Handoffs != 1 || summary.Tokens != 1 || summary.Files != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	if _, err := (&core.Registry{}).GetSession(sess.ID); err != nil {
		t.Fatal(err)
	}
	if token, err := os.ReadFile(filepath.Join(core.BridgeTokensDir(), "sess-1.token")); err != nil || string(token) != "secret" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "viz", "sess-1.json")); !os.IsNotExist(err) {
		t.Fatalf("viz binding crossed control boundary: %v", err)
	}
}

func TestImportRefusesBridgeTokenConflict(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	if err := core.EnsureStateDirs(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(core.BridgeTokensDir(), "sess-1.token")
	if err := os.WriteFile(path, []byte("destination-authority"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Import(&core.Registry{}, &Bundle{V: 1, Tokens: map[string][]byte{"sess-1.token": []byte("stale-authority")}})
	if err == nil {
		t.Fatal("conflicting token import succeeded")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil || string(raw) != "destination-authority" {
		t.Fatalf("destination token changed: %q, %v", raw, readErr)
	}
}
