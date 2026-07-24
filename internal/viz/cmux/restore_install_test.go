package cmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallVaultAgentIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cmux.json")
	_ = os.WriteFile(cfg, []byte(`{"vault":{"agents":[{"id":"other"}]}}`), 0o644)
	if err := InstallVaultAgent(cfg); err != nil {
		t.Fatal(err)
	}
	if err := InstallVaultAgent(cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfg)
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	agents := data["vault"].(map[string]any)["agents"].([]any)
	n := 0
	for _, a := range agents {
		if a.(map[string]any)["id"] == "relay" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want 1 relay agent, got %d in %s", n, raw)
	}
}
