// Package controlstate moves authoritative Relay state between control hosts.
// It excludes visualization bindings and transient locks/process state.
package controlstate

import (
	"bufio"
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
	Handoffs []*core.Handoff   `json:"handoffs"`
	Tokens   map[string][]byte `json:"tokens,omitempty"`
	Files    []File            `json:"files,omitempty"`
	Ledger   []json.RawMessage `json:"ledger,omitempty"`
}

type Summary struct {
	Sessions int `json:"sessions"`
	Handoffs int `json:"handoffs"`
	Tokens   int `json:"tokens"`
	Files    int `json:"files"`
	Ledger   int `json:"ledger"`
}

func Export(reg *core.Registry) (*Bundle, error) {
	sessions, err := reg.ListSessions()
	if err != nil {
		return nil, err
	}
	handoffs, err := reg.ListHandoffs()
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{V: 1, Sessions: sessions, Handoffs: handoffs, Tokens: map[string][]byte{}}
	entries, _ := os.ReadDir(core.BridgeTokensDir())
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".token" {
			continue
		}
		if raw, readErr := os.ReadFile(filepath.Join(core.BridgeTokensDir(), entry.Name())); readErr == nil {
			bundle.Tokens[entry.Name()] = raw
		}
	}
	for _, root := range []string{"parent-inbox", "conductor"} {
		base := filepath.Join(core.StateRoot(), root)
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(core.StateRoot(), path)
			if relErr != nil {
				return nil
			}
			if raw, readErr := os.ReadFile(path); readErr == nil {
				bundle.Files = append(bundle.Files, File{Path: rel, Data: raw})
			}
			return nil
		})
	}
	if file, openErr := os.Open(core.LedgerPath()); openErr == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			if json.Valid(line) {
				bundle.Ledger = append(bundle.Ledger, json.RawMessage(line))
			}
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
	for _, incoming := range bundle.Handoffs {
		if incoming == nil || incoming.ID == "" {
			continue
		}
		current, err := reg.GetHandoff(incoming.ID)
		if err == nil && !incoming.UpdatedAt.After(current.UpdatedAt) {
			continue
		}
		if err := reg.PutHandoff(incoming); err != nil {
			return nil, err
		}
		summary.Handoffs++
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
		if err := os.WriteFile(filepath.Join(core.BridgeTokensDir(), name), token, 0o600); err != nil {
			return nil, err
		}
		summary.Tokens++
	}
	for _, file := range bundle.Files {
		clean := filepath.Clean(file.Path)
		if clean != file.Path || strings.HasPrefix(clean, "..") || (!strings.HasPrefix(clean, "parent-inbox"+string(filepath.Separator)) && !strings.HasPrefix(clean, "conductor"+string(filepath.Separator))) {
			return nil, fmt.Errorf("invalid control file %q", file.Path)
		}
		dst := filepath.Join(core.StateRoot(), clean)
		if filepath.Ext(dst) == ".jsonl" {
			if err := mergeJSONL(dst, file.Data); err != nil {
				return nil, err
			}
		} else if _, err := os.Stat(dst); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(dst, file.Data, 0o600); err != nil {
				return nil, err
			}
		}
		summary.Files++
	}
	ledgerRaw := make([]byte, 0)
	for _, line := range bundle.Ledger {
		ledgerRaw = append(ledgerRaw, append(line, '\n')...)
	}
	if err := mergeJSONL(core.LedgerPath(), ledgerRaw); err != nil {
		return nil, err
	}
	summary.Ledger = len(bundle.Ledger)
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
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" && json.Valid([]byte(line)) && !seen[line] {
				seen[line] = true
				lines = append(lines, line)
			}
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
