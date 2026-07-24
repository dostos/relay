// Package cmux implements ports.Viz using the cmux CLI (optional visualization).
// Bindings are keyed by session_id — never by pane title heuristics.
package cmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dostos/relay/internal/ports"
)

// Viz presents sessions in cmux panes. Lifecycle is never owned here.
type Viz struct {
	Bin string

	mu       sync.Mutex
	bindings map[string]binding // sessionID -> refs
}

type binding struct {
	Surface string `json:"surface"`
	Pane    string `json:"pane,omitempty"`
	Attach  string `json:"attach_cmd,omitempty"`
}

func New() *Viz {
	bin := os.Getenv("RELAY_CMUX_BIN")
	if bin == "" {
		if p, err := exec.LookPath("cmux"); err == nil {
			bin = p
		} else if _, err := os.Stat("/Applications/cmux.app/Contents/Resources/bin/cmux"); err == nil {
			bin = "/Applications/cmux.app/Contents/Resources/bin/cmux"
		} else {
			bin = "cmux"
		}
	}
	return &Viz{Bin: bin, bindings: map[string]binding{}}
}

func (v *Viz) Kind() string { return "cmux" }

func (v *Viz) Available(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, v.Bin, "ping")
	return cmd.Run() == nil
}

func (v *Viz) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, v.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cmux %v: %w (%s)", args, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (v *Viz) Present(ctx context.Context, sessionID, attachCmd string, layout ports.Layout) (string, error) {
	v.mu.Lock()
	if b, ok := v.bindings[sessionID]; ok && b.Surface != "" {
		v.mu.Unlock()
		_ = v.focusSurface(ctx, b)
		return b.Surface, nil
	}
	v.mu.Unlock()

	before, _ := v.listSurfaces(ctx)
	dir := "right"
	if layout.Mode == "pair" {
		dir = "right"
	}
	if _, err := v.run(ctx, "new-split", dir, "--focus", "true"); err != nil {
		return "", err
	}
	after, err := v.listSurfaces(ctx)
	if err != nil {
		return "", err
	}
	surface, pane := diffNew(before, after)
	if surface == "" {
		// Fallback: focused surface from identify
		surface, pane, _ = v.focused(ctx)
	}
	if surface == "" {
		return "", fmt.Errorf("could not determine new cmux surface after split")
	}
	if _, err := v.run(ctx, "send", "--surface", surface, "--", attachCmd+"\n"); err != nil {
		return "", err
	}
	b := binding{Surface: surface, Pane: pane, Attach: attachCmd}
	v.mu.Lock()
	v.bindings[sessionID] = b
	v.mu.Unlock()
	_ = v.persistBinding(sessionID, b)
	return surface, nil
}

func (v *Viz) Focus(ctx context.Context, sessionID string) error {
	b, err := v.lookup(sessionID)
	if err != nil {
		return err
	}
	return v.focusSurface(ctx, b)
}

func (v *Viz) focusSurface(ctx context.Context, b binding) error {
	if b.Pane != "" {
		if _, err := v.run(ctx, "focus-pane", "--pane", b.Pane); err == nil {
			return nil
		}
	}
	// Best-effort: focus-panel accepts surface aliases on some builds.
	if _, err := v.run(ctx, "focus-panel", "--panel", b.Surface); err != nil {
		return fmt.Errorf("focus session surface %s: %w", b.Surface, err)
	}
	return nil
}

func (v *Viz) Close(ctx context.Context, sessionID string) error {
	b, err := v.lookup(sessionID)
	if err != nil {
		return nil // already gone
	}
	v.mu.Lock()
	delete(v.bindings, sessionID)
	v.mu.Unlock()
	_, err = v.run(ctx, "close-surface", "--surface", b.Surface)
	_ = os.Remove(bindPath(sessionID))
	return err
}

func (v *Viz) Layout(ctx context.Context) (string, error) {
	out, err := v.run(ctx, "list-panes", "--json")
	if err != nil {
		return v.run(ctx, "tree", "--json")
	}
	return out, nil
}

func (v *Viz) lookup(sessionID string) (binding, error) {
	v.mu.Lock()
	b, ok := v.bindings[sessionID]
	v.mu.Unlock()
	if ok && b.Surface != "" {
		return b, nil
	}
	b, err := v.loadBinding(sessionID)
	if err != nil {
		return binding{}, fmt.Errorf("no viz surface for session %s", sessionID)
	}
	v.mu.Lock()
	v.bindings[sessionID] = b
	v.mu.Unlock()
	return b, nil
}

type panesJSON struct {
	Panes []struct {
		Ref               string   `json:"ref"`
		SelectedSurfaceRef string  `json:"selected_surface_ref"`
		SurfaceRefs       []string `json:"surface_refs"`
	} `json:"panes"`
}

func (v *Viz) listSurfaces(ctx context.Context) (map[string]string, error) {
	out, err := v.run(ctx, "list-panes", "--json")
	if err != nil {
		return nil, err
	}
	var pj panesJSON
	if err := json.Unmarshal([]byte(out), &pj); err != nil {
		return nil, err
	}
	m := map[string]string{} // surface -> pane
	for _, p := range pj.Panes {
		for _, s := range p.SurfaceRefs {
			m[s] = p.Ref
		}
		if p.SelectedSurfaceRef != "" {
			m[p.SelectedSurfaceRef] = p.Ref
		}
	}
	return m, nil
}

func diffNew(before, after map[string]string) (surface, pane string) {
	for s, p := range after {
		if _, ok := before[s]; !ok {
			return s, p
		}
	}
	return "", ""
}

func (v *Viz) focused(ctx context.Context) (surface, pane string, err error) {
	out, err := v.run(ctx, "identify", "--json")
	if err != nil {
		return "", "", err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return "", "", err
	}
	for _, key := range []string{"focused", "caller"} {
		if block, ok := m[key].(map[string]any); ok {
			if s, _ := block["surface_ref"].(string); s != "" {
				surface = s
			}
			if p, _ := block["pane_ref"].(string); p != "" {
				pane = p
			}
			if surface != "" {
				return surface, pane, nil
			}
		}
	}
	return "", "", fmt.Errorf("no focused surface")
}

func bindPath(sessionID string) string {
	xdg := os.Getenv("XDG_STATE_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(xdg, "relay", "viz")
	_ = os.MkdirAll(dir, 0o755)
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, sessionID)
	return filepath.Join(dir, safe+".json")
}

func (v *Viz) persistBinding(sessionID string, b binding) error {
	raw, _ := json.MarshalIndent(b, "", "  ")
	return os.WriteFile(bindPath(sessionID), raw, 0o644)
}

func (v *Viz) loadBinding(sessionID string) (binding, error) {
	raw, err := os.ReadFile(bindPath(sessionID))
	if err != nil {
		return binding{}, err
	}
	var b binding
	if err := json.Unmarshal(raw, &b); err != nil {
		return binding{}, err
	}
	return b, nil
}
