// Package controlstate moves authoritative Relay state between control hosts.
// It excludes visualization bindings and transient locks/process state.
package controlstate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dostos/relay/internal/core"
)

type File struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

type Bundle struct {
	V        int               `json:"v"`
	Sessions []*core.Session   `json:"sessions"`
	Tokens   map[string][]byte `json:"tokens,omitempty"`
	Files    []File            `json:"files,omitempty"`
}

type Summary struct {
	Sessions int `json:"sessions"`
	Tokens   int `json:"tokens"`
	Files    int `json:"files"`
}

func Export(reg *core.Registry) (*Bundle, error) {
	sessions, err := reg.ListSessions()
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{V: 1, Sessions: sessions, Tokens: map[string][]byte{}}
	entries, _ := os.ReadDir(core.BridgeTokensDir())
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".token" {
			continue
		}
		if raw, readErr := os.ReadFile(filepath.Join(core.BridgeTokensDir(), entry.Name())); readErr == nil {
			bundle.Tokens[entry.Name()] = raw
		}
	}
	return bundle, nil
}

func Import(reg *core.Registry, bundle *Bundle) (*Summary, error) {
	if bundle == nil || bundle.V != 1 {
		return nil, fmt.Errorf("unsupported control bundle")
	}
	if err := core.EnsureStateDirs(); err != nil {
		return nil, err
	}
	summary := &Summary{}
	for _, incoming := range bundle.Sessions {
		if incoming == nil || incoming.ID == "" {
			continue
		}
		current, err := reg.GetSession(incoming.ID)
		if err == nil && !incoming.UpdatedAt.After(current.UpdatedAt) {
			continue
		}
		if err := reg.PutSession(incoming); err != nil {
			return nil, err
		}
		summary.Sessions++
	}
	for name, token := range bundle.Tokens {
		if len(token) == 0 {
			// Revoked/partially cleaned identities sometimes leave an empty token
			// placeholder. It grants no authority and must not block migration.
			continue
		}
		if filepath.Base(name) != name || filepath.Ext(name) != ".token" || len(token) > 4096 {
			return nil, fmt.Errorf("invalid bridge token entry %q", name)
		}
		destination := filepath.Join(core.BridgeTokensDir(), name)
		if current, err := os.ReadFile(destination); err == nil {
			if !bytes.Equal(current, token) {
				return nil, fmt.Errorf("refuse to overwrite existing bridge token %q", name)
			}
			continue
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.WriteFile(destination, token, 0o600); err != nil {
			return nil, err
		}
		summary.Tokens++
	}
	return summary, nil
}

func mergeJSONL(path string, incoming []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	seen := map[string]bool{}
	var lines []string
	for _, source := range [][]byte{readFile(path), incoming} {
		scanner := bufio.NewScanner(strings.NewReader(string(source)))
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" && json.Valid([]byte(line)) && !seen[line] {
				seen[line] = true
				lines = append(lines, line)
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("merge JSONL: %w", err)
		}
	}
	data := []byte(strings.Join(lines, "\n"))
	if len(data) > 0 {
		data = append(data, '\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readFile(path string) []byte { raw, _ := os.ReadFile(path); return raw }
