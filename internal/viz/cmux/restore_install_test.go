package cmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultCmuxJSONPathPrefersXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir() resolves the Library fallback from $HOME
	t.Setenv("CMUX_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	xdg := filepath.Join(home, ".config", "cmux", "cmux.json")
	lib := filepath.Join(home, "Library", "Application Support", "cmux", "cmux.json")

	// Neither exists yet -> default to the XDG path (cmux reads it on macOS).
	if got := DefaultCmuxJSONPath(); got != xdg {
		t.Fatalf("no config present: want %s, got %s", xdg, got)
	}

	// Only Library exists -> fall back to it.
	if err := os.MkdirAll(filepath.Dir(lib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lib, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DefaultCmuxJSONPath(); got != lib {
		t.Fatalf("only Library present: want %s, got %s", lib, got)
	}

	// XDG exists -> prefer it over Library (the real bug: cmux reads only this).
	if err := os.MkdirAll(filepath.Dir(xdg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xdg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DefaultCmuxJSONPath(); got != xdg {
		t.Fatalf("both present: want %s, got %s", xdg, got)
	}

	// CMUX_CONFIG override always wins.
	t.Setenv("CMUX_CONFIG", "/explicit/path.json")
	if got := DefaultCmuxJSONPath(); got != "/explicit/path.json" {
		t.Fatalf("override: want /explicit/path.json, got %s", got)
	}
}

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
