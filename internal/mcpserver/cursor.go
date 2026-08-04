package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type cursorServer struct {
	Type    string   `json:"type,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// InstallCursorConfig atomically adds Relay to Cursor's user MCP inventory.
// Existing unrelated fields and servers are preserved byte-semantically after
// JSON decoding. A conflicting server named relay is a human-owned choice and
// is never overwritten.
func InstallCursorConfig(path, executable string) (bool, error) {
	if path == "" || executable == "" || !filepath.IsAbs(path) || !filepath.IsAbs(executable) {
		return false, fmt.Errorf("cursor MCP path and relay executable must be absolute")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, fmt.Errorf("refusing non-regular Cursor MCP config %s", path)
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	root := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &root); err != nil {
			return false, fmt.Errorf("parse Cursor MCP config: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	servers := map[string]json.RawMessage{}
	if raw := root["mcpServers"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return false, fmt.Errorf("parse Cursor MCP servers: %w", err)
		}
	}
	desired := cursorServer{Type: "stdio", Command: executable, Args: []string{"mcp", "serve"}}
	if raw := servers["relay"]; len(raw) > 0 {
		var existing cursorServer
		if err := json.Unmarshal(raw, &existing); err != nil || existing.Command != desired.Command || len(existing.Args) != 2 || existing.Args[0] != "mcp" || existing.Args[1] != "serve" || (existing.Type != "" && existing.Type != "stdio") {
			return false, fmt.Errorf("Cursor MCP server name relay already has a different definition")
		}
		return false, nil
	}
	desiredRaw, _ := json.Marshal(desired)
	servers["relay"] = desiredRaw
	serversRaw, err := json.Marshal(servers)
	if err != nil {
		return false, err
	}
	root["mcpServers"] = serversRaw
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".relay-mcp-*.tmp")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, err
	}
	return true, nil
}

func installCursor() int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	path := filepath.Join(home, ".cursor", "mcp.json")
	changed, err := InstallCursorConfig(path, executable)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if changed {
		fmt.Printf("relay: installed Cursor MCP definition in %s\n", path)
	} else {
		fmt.Printf("relay: Cursor MCP definition already current in %s\n", path)
	}
	fmt.Println("relay: approve only this server with: cursor-agent mcp enable relay")
	return 0
}
