package cmux

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// VaultAgent is the cmux vault.agents[] entry for relay resume.
var VaultAgent = map[string]any{
	"id":   "relay",
	"name": "relay remote",
	"detect": map[string]any{
		"processName":  "relay",
		"argvContains": []string{"--session"},
	},
	"sessionIdSource": map[string]any{
		"type":       "argvOption",
		"argvOption": "--session",
	},
	"resumeCommand": "relay resume --session {{sessionId}} --cwd {{cwd}}",
}

// DefaultCmuxJSONPath returns ~/Library/Application Support/cmux/cmux.json on macOS.
func DefaultCmuxJSONPath() string {
	if v := os.Getenv("CMUX_CONFIG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "cmux", "cmux.json")
}

// InstallVaultAgent merges the relay vault agent into cmux.json (JSONC-tolerant).
func InstallVaultAgent(cfgPath string) error {
	if cfgPath == "" {
		cfgPath = DefaultCmuxJSONPath()
	}
	raw := ""
	if b, err := os.ReadFile(cfgPath); err == nil {
		raw = string(b)
	}
	var data map[string]any
	if strings.TrimSpace(raw) == "" {
		data = map[string]any{}
	} else {
		if err := json.Unmarshal([]byte(stripJSONC(raw)), &data); err != nil {
			return fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	}
	vault, _ := data["vault"].(map[string]any)
	if vault == nil {
		vault = map[string]any{}
		data["vault"] = vault
	}
	agents, _ := vault["agents"].([]any)
	filtered := make([]any, 0, len(agents)+1)
	for _, a := range agents {
		if m, ok := a.(map[string]any); ok && m["id"] == "relay" {
			continue
		}
		filtered = append(filtered, a)
	}
	filtered = append(filtered, VaultAgent)
	vault["agents"] = filtered

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	if raw != "" {
		bak := cfgPath + ".bak-" + time.Now().Format("20060102-150405")
		_ = os.WriteFile(bak, []byte(raw), 0o644)
	}
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		return err
	}
	// Best-effort reload
	bin := os.Getenv("RELAY_CMUX_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("cmux")
	}
	if bin != "" {
		_ = exec.Command(bin, "reload-config").Run()
	}
	return nil
}

func stripJSONC(s string) string {
	var out strings.Builder
	instr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if instr {
			out.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				instr = false
			}
			continue
		}
		if c == '"' {
			instr = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
			if i < len(s) {
				out.WriteByte(s[i])
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}
