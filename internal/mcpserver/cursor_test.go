package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallCursorConfigPreservesExistingServersAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"custom":{"keep":true},"mcpServers":{"existing":{"type":"http","url":"https://example.invalid"}}}`
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := InstallCursorConfig(path, "/opt/relay/bin/relay")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || len(root["custom"]) == 0 {
		t.Fatalf("root=%s err=%v", raw, err)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(root["mcpServers"], &servers); err != nil || len(servers) != 2 {
		t.Fatalf("servers=%v err=%v", servers, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
	changed, err = InstallCursorConfig(path, "/opt/relay/bin/relay")
	if err != nil || changed {
		t.Fatalf("idempotent changed=%v err=%v", changed, err)
	}
}

func TestInstallCursorConfigRefusesConflictingRelayServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"relay":{"command":"other"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallCursorConfig(path, "/opt/relay/bin/relay"); err == nil {
		t.Fatal("conflicting relay server was overwritten")
	}
}
