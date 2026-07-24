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
	// Best-effort: stamp cmux resume binding so restart can re-run the same command.
	sessName := extractSessionFlag(attachCmd)
	if sessName != "" {
		_, _ = v.run(ctx, "surface", "resume", "set",
			"--surface", surface,
			"--kind", "relay",
			"--name", "relay: "+sessName,
			"--checkpoint", sessName,
			"--", attachCmd,
		)
	}
	b := binding{Surface: surface, Pane: pane, Attach: attachCmd}
	v.mu.Lock()
	v.bindings[sessionID] = b
	v.mu.Unlock()
	_ = v.persistBinding(sessionID, b)
	return surface, nil
}

func extractSessionFlag(cmd string) string {
	fields := strings.Fields(cmd)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "--session" || fields[i] == "-s" {
			return fields[i+1]
		}
	}
	return ""
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

// SaveRestorable stamps resume bindings on every live relay pane (manual snapshot).
func (v *Viz) SaveRestorable(ctx context.Context) (int, error) {
	tsv, err := v.run(ctx, "top", "--all", "--processes", "--format", "tsv")
	if err != nil {
		return 0, err
	}
	type row struct{ ref, cmd string }
	var rows []row
	var curRef, curCmd string
	for _, line := range strings.Split(tsv, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		typ, key, cmd := f[3], f[4], f[6]
		if typ == "surface" {
			if curRef != "" && strings.Contains(curCmd, "relay") && strings.Contains(curCmd, "--session") {
				rows = append(rows, row{curRef, curCmd})
			}
			curRef, curCmd = key, cmd
		}
	}
	if curRef != "" && strings.Contains(curCmd, "relay") && strings.Contains(curCmd, "--session") {
		rows = append(rows, row{curRef, curCmd})
	}
	saved := 0
	for _, r := range rows {
		name := extractSessionFlag(r.cmd)
		if name == "" {
			continue
		}
		if _, err := v.run(ctx, "surface", "resume", "set",
			"--surface", r.ref,
			"--kind", "relay",
			"--name", "relay: "+name,
			"--checkpoint", name,
			"--", r.cmd,
		); err == nil {
			saved++
		}
	}
	return saved, nil
}

// RestoreSaved re-sends resume commands into idle surfaces that have relay bindings.
func (v *Viz) RestoreSaved(ctx context.Context) (int, error) {
	tsv, err := v.run(ctx, "top", "--all", "--processes", "--format", "tsv")
	if err != nil {
		return 0, err
	}
	live := liveRelaySurfaces(tsv)
	out, err := v.run(ctx, "list-panes", "--json")
	if err != nil {
		return 0, err
	}
	var pj panesJSON
	_ = json.Unmarshal([]byte(out), &pj)
	restored := 0
	seen := map[string]bool{}
	for _, p := range pj.Panes {
		for _, ref := range p.SurfaceRefs {
			if seen[ref] || live[ref] {
				continue
			}
			seen[ref] = true
			raw, err := v.run(ctx, "surface", "resume", "get", "--surface", ref, "--json")
			if err != nil {
				continue
			}
			var wrap struct {
				ResumeBinding struct {
					Kind    string `json:"kind"`
					Command string `json:"command"`
				} `json:"resume_binding"`
			}
			if json.Unmarshal([]byte(raw), &wrap) != nil {
				continue
			}
			if wrap.ResumeBinding.Kind != "relay" || wrap.ResumeBinding.Command == "" {
				continue
			}
			if _, err := v.run(ctx, "send", "--surface", ref, "--", wrap.ResumeBinding.Command+"\n"); err == nil {
				restored++
			}
		}
	}
	return restored, nil
}

func liveRelaySurfaces(tsv string) map[string]bool {
	children := map[string][]struct{ key, cmd string }{}
	var surfaces []string
	for _, line := range strings.Split(tsv, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		typ, key, parent, cmd := f[3], f[4], f[5], f[6]
		if typ == "surface" {
			surfaces = append(surfaces, key)
			continue
		}
		if typ == "process" {
			children[parent] = append(children[parent], struct{ key, cmd string }{key, cmd})
		}
	}
	live := map[string]bool{}
	var hasRelayOrSSH func(string) bool
	hasRelayOrSSH = func(parent string) bool {
		for _, c := range children[parent] {
			cmd := c.cmd
			base := cmd
			if i := strings.IndexByte(cmd, ' '); i >= 0 {
				base = filepath.Base(cmd[:i])
			} else {
				base = filepath.Base(cmd)
			}
			if base == "relay" || base == "ssh" || strings.Contains(cmd, "relay resume") {
				return true
			}
			if hasRelayOrSSH(c.key) {
				return true
			}
		}
		return false
	}
	for _, ref := range surfaces {
		if hasRelayOrSSH(ref) {
			live[ref] = true
		}
	}
	return live
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
