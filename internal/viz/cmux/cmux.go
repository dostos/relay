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
	"sort"
	"strings"
	"sync"

	"github.com/dostos/relay/internal/core"
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
		sessName := extractSessionFlag(attachCmd)
		_ = v.brandSurface(ctx, b.Surface, sessName)
		if sessName != "" {
			cwd, _ := os.Getwd()
			core.RememberPanePersist(b.Surface, sessName, "", "", cwd, true)
		}
		return b.Surface, nil
	}
	v.mu.Unlock()

	before, _ := v.listSurfaces(ctx)
	surface, pane, err := v.openSurface(ctx, layout)
	if err != nil {
		return "", err
	}
	if surface == "" {
		after, err := v.listSurfaces(ctx)
		if err != nil {
			return "", err
		}
		surface, pane = diffNew(before, after)
	}
	// Never fall back to the focused surface — that can inject into an agent tab.
	if surface == "" {
		return "", fmt.Errorf("could not determine new cmux surface after present (split/tab created no detectable surface)")
	}
	if _, err := v.run(ctx, "send", "--surface", surface, "--", attachCmd+"\n"); err != nil {
		return "", err
	}
	// Best-effort: stamp cmux resume binding so restart can re-run the same command.
	sessName := extractSessionFlag(attachCmd)
	if sessName != "" {
		title := brandTitle(sessName)
		_, _ = v.run(ctx, "surface", "resume", "set",
			"--surface", surface,
			"--kind", "relay",
			"--name", title,
			"--checkpoint", sessName,
			"--", attachCmd,
		)
		_ = v.brandSurface(ctx, surface, sessName)
		cwd, _ := os.Getwd()
		core.RememberPanePersist(surface, sessName, "", "", cwd, true)
	}
	b := binding{Surface: surface, Pane: pane, Attach: attachCmd}
	v.mu.Lock()
	v.bindings[sessionID] = b
	v.mu.Unlock()
	_ = v.persistBinding(sessionID, b)
	return surface, nil
}

func (v *Viz) openSurface(ctx context.Context, layout ports.Layout) (surface, pane string, err error) {
	// Stacked tabs only when explicitly requested (--tab). Default: side-by-side split.
	if layout.Tab && layout.Pane != "" {
		args := []string{"new-surface", "--type", "terminal", "--pane", layout.Pane, "--focus", "true"}
		if layout.Workspace != "" {
			args = append(args, "--workspace", layout.Workspace)
		}
		out, err := v.run(ctx, args...)
		if err != nil {
			return "", "", err
		}
		surface, pane = parseNewSurfaceRefs(out, layout.Pane)
		return surface, pane, nil
	}
	dir := "right"
	args := []string{"new-split", dir, "--focus", "true"}
	if layout.Workspace != "" {
		args = append(args, "--workspace", layout.Workspace)
	}
	if layout.Pane != "" {
		if surf := v.selectedSurfaceInPane(ctx, layout.Workspace, layout.Pane); surf != "" {
			args = append(args, "--surface", surf)
		}
	}
	out, err := v.run(ctx, args...)
	if err != nil {
		return "", "", err
	}
	// cmux prints: OK surface:N workspace:M
	surface, _ = parseNewSurfaceRefs(out, "")
	return surface, "", nil
}

func (v *Viz) selectedSurfaceInPane(ctx context.Context, workspace, pane string) string {
	args := []string{"list-panes", "--json"}
	if workspace != "" {
		args = append(args, "--workspace", workspace)
	}
	out, err := v.run(ctx, args...)
	if err != nil {
		return ""
	}
	var pj panesJSON
	if json.Unmarshal([]byte(out), &pj) != nil {
		return ""
	}
	for _, p := range pj.Panes {
		if p.Ref != pane {
			continue
		}
		if p.SelectedSurfaceRef != "" {
			return p.SelectedSurfaceRef
		}
		if len(p.SurfaceRefs) > 0 {
			return p.SurfaceRefs[0]
		}
	}
	return ""
}

func parseNewSurfaceRefs(out, fallbackPane string) (surface, pane string) {
	pane = fallbackPane
	for _, field := range strings.Fields(out) {
		switch {
		case strings.HasPrefix(field, "surface:"):
			surface = field
		case strings.HasPrefix(field, "pane:"):
			pane = field
		}
	}
	return surface, pane
}

func brandTitle(persistName string) string {
	name := strings.TrimPrefix(strings.TrimSpace(persistName), "dostos-workspace-")
	if name == "" {
		name = persistName
	}
	return "◆ RELAY · " + name
}

func projectLabel(persistName string) string {
	name := strings.TrimPrefix(strings.TrimSpace(persistName), "dostos-workspace-")
	if name == "" {
		return persistName
	}
	return name
}

func (v *Viz) brandSurface(ctx context.Context, surface, persistName string) error {
	if surface == "" || persistName == "" {
		return nil
	}
	title := brandTitle(persistName)
	// Title only — do not pin; pinning is a user choice.
	_, err := v.run(ctx, "rename-tab", "--surface", surface, "--title", title)
	return err
}

// BrandLabels renames bound tabs to ◆ RELAY · <project> and refreshes workspace
// status pills to ◆ RELAY · a, b (comma-separated projects). Does not touch
// workspace descriptions — those are user-owned.
func (v *Viz) BrandLabels(ctx context.Context, labels map[string]string) error {
	type hit struct {
		project string
	}
	byWS := map[string][]hit{}
	for sessionID, project := range labels {
		b, err := v.lookup(sessionID)
		if err != nil || b.Surface == "" {
			continue
		}
		persist := extractSessionFlag(b.Attach)
		if persist == "" {
			persist = project
		}
		_ = v.brandSurface(ctx, b.Surface, persist)
		ws := v.workspaceOfSurface(ctx, b.Surface)
		if ws == "" {
			continue
		}
		byWS[ws] = append(byWS[ws], hit{project: projectLabel(persist)})
	}
	for ws, hits := range byWS {
		seen := map[string]bool{}
		var projects []string
		add := func(p string) {
			if p == "" || seen[p] {
				return
			}
			seen[p] = true
			projects = append(projects, p)
		}
		for _, h := range hits {
			add(h.project)
		}
		// Merge any other ◆ RELAY tabs already in this workspace (partial updates).
		for _, p := range v.relayProjectsInWorkspace(ctx, ws) {
			add(p)
		}
		if len(projects) == 0 {
			continue
		}
		sort.Strings(projects)
		status := "◆ RELAY · " + strings.Join(projects, ", ")
		_, _ = v.run(ctx, "set-status", "relay", status, "--color", "#14b8a6", "--priority", "90", "--workspace", ws)
	}
	return nil
}

func (v *Viz) relayProjectsInWorkspace(ctx context.Context, workspace string) []string {
	out, err := v.run(ctx, "tree", "--json")
	if err != nil {
		return nil
	}
	var root struct {
		Windows []struct {
			Workspaces []struct {
				Ref   string `json:"ref"`
				Panes []struct {
					Surfaces []struct {
						Ref   string `json:"ref"`
						Title string `json:"title"`
					} `json:"surfaces"`
				} `json:"panes"`
			} `json:"workspaces"`
		} `json:"windows"`
	}
	if json.Unmarshal([]byte(out), &root) != nil {
		return nil
	}
	const prefix = "◆ RELAY · "
	var projects []string
	for _, w := range root.Windows {
		for _, ws := range w.Workspaces {
			if ws.Ref != workspace {
				continue
			}
			for _, p := range ws.Panes {
				for _, s := range p.Surfaces {
					title := strings.TrimSpace(s.Title)
					if strings.HasPrefix(title, prefix) {
						projects = append(projects, strings.TrimSpace(strings.TrimPrefix(title, prefix)))
					}
				}
			}
		}
	}
	return projects
}

func (v *Viz) workspaceOfSurface(ctx context.Context, surface string) string {
	// --all so a surface is resolvable regardless of which window is focused;
	// plain `tree --json` only reports the current window.
	out, err := v.run(ctx, "tree", "--all", "--json")
	if err != nil {
		return ""
	}
	var root struct {
		Windows []struct {
			Workspaces []struct {
				Ref   string `json:"ref"`
				Panes []struct {
					Surfaces []struct {
						Ref string `json:"ref"`
					} `json:"surfaces"`
				} `json:"panes"`
			} `json:"workspaces"`
		} `json:"windows"`
	}
	if json.Unmarshal([]byte(out), &root) != nil {
		return ""
	}
	for _, w := range root.Windows {
		for _, ws := range w.Workspaces {
			for _, p := range ws.Panes {
				for _, s := range p.Surfaces {
					if s.Ref == surface {
						return ws.Ref
					}
				}
			}
		}
	}
	return ""
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
	defer os.Remove(bindPath(sessionID))
	// cmux short refs (surface:N) resolve within a window/workspace context, so
	// scope close-surface to the bound surface's own workspace. A bare
	// --surface fails not_found whenever that surface is not in the focused
	// window, silently orphaning the pane. An empty workspace means the surface
	// is no longer in any window — already gone, nothing to close.
	ws := v.workspaceOfSurface(ctx, b.Surface)
	if ws == "" {
		return nil
	}
	_, err = v.run(ctx, "close-surface", "--surface", b.Surface, "--workspace", ws)
	return err
}

func (v *Viz) Layout(ctx context.Context) (string, error) {
	out, err := v.run(ctx, "list-panes", "--json")
	if err != nil {
		return v.run(ctx, "tree", "--json")
	}
	return out, nil
}

// SaveRestorable stamps resume bindings + pane history on every live relay pane.
// cmux top often shows only the binary name ("relay"), so we recover the session
// from `surface resume get` (checkpoint / command) rather than argv in the TSV.
func (v *Viz) SaveRestorable(ctx context.Context) (int, error) {
	tsv, err := v.run(ctx, "top", "--all", "--processes", "--format", "tsv")
	if err != nil {
		return 0, err
	}
	live := liveRelaySurfaces(tsv)
	// Also include surfaces that already have a kind=relay resume binding.
	for ref := range v.relayBoundSurfaces(ctx) {
		live[ref] = true
	}
	saved := 0
	for ref := range live {
		name, cmd, cwd := v.relayCheckpoint(ctx, ref)
		if name == "" {
			continue
		}
		if cmd == "" {
			cmd = core.ResumeLaunchCmd(name)
		}
		if _, err := v.run(ctx, "surface", "resume", "set",
			"--surface", ref,
			"--kind", "relay",
			"--name", brandTitle(name),
			"--checkpoint", name,
			"--", cmd,
		); err == nil {
			_ = v.brandSurface(ctx, ref, name)
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			core.RememberPanePersist(ref, name, "", "", cwd, true)
			saved++
		}
	}
	return saved, nil
}

func (v *Viz) relayBoundSurfaces(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	surfs, err := v.listSurfaces(ctx)
	if err != nil {
		return out
	}
	for ref := range surfs {
		name, _, _ := v.relayCheckpoint(ctx, ref)
		if name != "" {
			out[ref] = true
		}
	}
	return out
}

func (v *Viz) relayCheckpoint(ctx context.Context, surface string) (persistName, command, cwd string) {
	raw, err := v.run(ctx, "surface", "resume", "get", "--surface", surface, "--json")
	if err != nil {
		return "", "", ""
	}
	var wrap struct {
		ResumeBinding struct {
			Kind         string `json:"kind"`
			CheckpointID string `json:"checkpoint_id"`
			Command      string `json:"command"`
			CWD          string `json:"cwd"`
		} `json:"resume_binding"`
	}
	if json.Unmarshal([]byte(raw), &wrap) != nil {
		return "", "", ""
	}
	rb := wrap.ResumeBinding
	if rb.Kind != "" && rb.Kind != "relay" {
		return "", "", ""
	}
	name := strings.TrimSpace(rb.CheckpointID)
	if name == "" {
		name = extractSessionFlag(rb.Command)
	}
	if name == "" {
		return "", "", ""
	}
	return name, rb.Command, rb.CWD
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
	// Prefer tree: list-panes often returns only the focused workspace's
	// selected pane, so before/after diffs miss newly split surfaces.
	out, err := v.run(ctx, "tree", "--json")
	if err != nil {
		return v.listSurfacesFromPanes(ctx)
	}
	var root struct {
		Windows []struct {
			Workspaces []struct {
				Panes []struct {
					Ref      string `json:"ref"`
					Surfaces []struct {
						Ref string `json:"ref"`
					} `json:"surfaces"`
				} `json:"panes"`
			} `json:"workspaces"`
		} `json:"windows"`
	}
	if err := json.Unmarshal([]byte(out), &root); err != nil {
		return v.listSurfacesFromPanes(ctx)
	}
	m := map[string]string{}
	for _, w := range root.Windows {
		for _, ws := range w.Workspaces {
			for _, p := range ws.Panes {
				for _, s := range p.Surfaces {
					if s.Ref != "" {
						m[s.Ref] = p.Ref
					}
				}
			}
		}
	}
	if len(m) == 0 {
		return v.listSurfacesFromPanes(ctx)
	}
	return m, nil
}

func (v *Viz) listSurfacesFromPanes(ctx context.Context) (map[string]string, error) {
	out, err := v.run(ctx, "list-panes", "--json")
	if err != nil {
		return nil, err
	}
	var pj panesJSON
	if err := json.Unmarshal([]byte(out), &pj); err != nil {
		return nil, err
	}
	m := map[string]string{}
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
