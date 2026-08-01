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
	"strconv"
	"strings"
	"sync"
	"time"

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
	V               int       `json:"v"`
	SessionID       string    `json:"session_id"`
	SourceSessionID string    `json:"source_session_id,omitempty"`
	Surface         string    `json:"surface"`
	Pane            string    `json:"pane,omitempty"`
	Workspace       string    `json:"workspace,omitempty"`
	Attach          string    `json:"attach_cmd,omitempty"`
	Mode            string    `json:"mode,omitempty"` // current | split | tab
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ManagedPane is the inspectable desktop-owned pane record returned by
// `relay pane list`.
type ManagedPane struct {
	SessionID       string    `json:"session_id"`
	SourceSessionID string    `json:"source_session_id,omitempty"`
	PersistName     string    `json:"persist_name,omitempty"`
	Surface         string    `json:"surface"`
	Pane            string    `json:"pane,omitempty"`
	Workspace       string    `json:"workspace,omitempty"`
	Mode            string    `json:"mode,omitempty"`
	State           string    `json:"state"` // live | disconnected
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type surfaceLocation struct {
	Workspace string
	Pane      string
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
	sessName := extractSessionFlag(attachCmd)
	// Idempotent across processes. Each `relay` CLI call is a fresh process, so
	// the in-memory bindings map is always empty here; consult the persisted
	// binding too (via lookup) and, when its surface is still open in cmux,
	// reuse it instead of leaking a duplicate split. This lets an agent call
	// `viz present` repeatedly (e.g. to keep a handoff visible) without piling
	// up panes.
	if b, err := v.lookup(sessionID); err == nil && b.Surface != "" {
		if loc := v.locationOfSurface(ctx, b.Surface); loc.Workspace != "" {
			b.SessionID = sessionID
			b.Workspace = loc.Workspace
			b.Pane = loc.Pane
			b.Attach = attachCmd
			if b.SourceSessionID == "" {
				b.SourceSessionID = layout.SourceSessionID
			}
			b.UpdatedAt = time.Now().UTC()
			_ = v.persistBinding(sessionID, b)
			_ = v.focusSurface(ctx, b)
			_ = v.brandSurface(ctx, b.Surface, sessName)
			if sessName != "" {
				v.rememberPane(sessionID, b.Surface, sessName)
			}
			return b.Surface, nil
		}
		// Bound surface is gone — drop the stale binding and open a fresh one.
		v.mu.Lock()
		delete(v.bindings, sessionID)
		v.mu.Unlock()
		_ = os.Remove(bindPath(sessionID))
	}

	// Blind-usage default: when the caller omits --workspace, land the split in
	// whatever workspace cmux currently has focused. Without this, cmux
	// new-split fails "Surface not found" and present is unusable unless the
	// caller first discovers the active workspace ref itself.
	if layout.Workspace == "" {
		layout.Workspace = v.activeWorkspace(ctx)
	}
	if layout.SourceSessionID != "" {
		layout = childLayout(layout, v.latestLiveChild(ctx, layout.SourceSessionID))
	}

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
		v.rememberPane(sessionID, surface, sessName)
	}
	loc := v.locationOfSurface(ctx, surface)
	if loc.Pane != "" {
		pane = loc.Pane
	}
	b := binding{
		V:               2,
		SessionID:       sessionID,
		SourceSessionID: layout.SourceSessionID,
		Surface:         surface,
		Pane:            pane,
		Workspace:       firstNonEmpty(loc.Workspace, layout.Workspace),
		Attach:          attachCmd,
		Mode:            bindingMode(layout),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	v.mu.Lock()
	v.bindings[sessionID] = b
	v.mu.Unlock()
	_ = v.persistBinding(sessionID, b)
	return surface, nil
}

// BindCurrent turns the caller's existing cmux surface into the visual home
// for a relay session. Unlike Present it does not create a split.
func (v *Viz) BindCurrent(ctx context.Context, sessionID, attachCmd string) (string, error) {
	surface, err := core.CurrentSurface()
	if err != nil {
		return "", err
	}
	return v.BindSurface(ctx, sessionID, attachCmd, surface)
}

// BindLocalParent records an existing cmux surface as a Relay parent without
// changing its title, command, or resume checkpoint. The local agent remains
// the owner of its pane lifecycle until the guarded parent-retire path closes it.
func (v *Viz) BindLocalParent(ctx context.Context, sessionID, surface string) (string, error) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		return "", fmt.Errorf("surface required")
	}
	loc := v.locationOfSurface(ctx, surface)
	if loc.Workspace == "" || loc.Pane == "" {
		return "", fmt.Errorf("cmux surface %s is not live", surface)
	}
	now := time.Now().UTC()
	b := binding{V: 2, SessionID: sessionID, Surface: surface, Pane: loc.Pane, Workspace: loc.Workspace, Mode: "current", CreatedAt: now, UpdatedAt: now}
	if old, err := v.lookup(sessionID); err == nil && !old.CreatedAt.IsZero() {
		b.CreatedAt = old.CreatedAt
	}
	v.mu.Lock()
	v.bindings[sessionID] = b
	v.mu.Unlock()
	if err := v.persistBinding(sessionID, b); err != nil {
		return "", err
	}
	return surface, nil
}

// ReparentBinding keeps persisted pane lineage aligned with an explicit
// parent-edge repair. It does not move the pane; layout remains stable.
func (v *Viz) ReparentBinding(childSessionID, parentSessionID string) error {
	b, err := v.lookup(childSessionID)
	if err != nil {
		return err
	}
	b.SourceSessionID = parentSessionID
	b.UpdatedAt = time.Now().UTC()
	v.mu.Lock()
	v.bindings[childSessionID] = b
	v.mu.Unlock()
	return v.persistBinding(childSessionID, b)
}

// ForgetBinding removes obsolete ownership metadata without closing its
// surface. Named-session replacement uses this after a dead tmux is recreated
// in the same current pane.
func (v *Viz) ForgetBinding(sessionID string) error {
	v.mu.Lock()
	delete(v.bindings, sessionID)
	v.mu.Unlock()
	err := os.Remove(bindPath(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// NotifyParent emits a desktop notification/flash and, for agent parents,
// injects one compact actionable envelope. The durable inbox deduplicates the
// event before this method is called, so a replay cannot spam the pane.
func (v *Viz) NotifyParent(ctx context.Context, sessionID string, notice core.ParentNotice) error {
	b, err := v.lookup(sessionID)
	if err != nil {
		return err
	}
	if v.locationOfSurface(ctx, b.Surface).Workspace == "" {
		return fmt.Errorf("parent surface %s is not live", b.Surface)
	}
	body := compactNotice(notice)
	args := []string{"notify", "--title", "Relay: " + notice.Kind, "--body", body, "--surface", b.Surface}
	if b.Workspace != "" {
		args = append(args, "--workspace", b.Workspace)
	}
	_, _ = v.run(ctx, args...)
	flash := []string{"trigger-flash", "--surface", b.Surface}
	if b.Workspace != "" {
		flash = append(flash, "--workspace", b.Workspace)
	}
	_, _ = v.run(ctx, flash...)
	sess, _ := (&core.Registry{}).GetSession(sessionID)
	if sess == nil || sess.Labels["wake_mode"] != "inject" {
		return nil
	}
	if _, err := v.run(ctx, surfaceCommand("send", b.Surface, b.Workspace, "--", body)...); err != nil {
		return err
	}
	return v.submitInjected(ctx, sessionID, b, notice.MessageID)
}

// surfaceCommand keeps multi-step input directed at the same pane even when
// its workspace is not focused. Short surface refs alone are not sufficient
// for a follow-up send-key in a background cmux workspace.
func surfaceCommand(command, surface, workspace string, tail ...string) []string {
	args := []string{command, "--surface", surface}
	if workspace != "" {
		args = append(args, "--workspace", workspace)
	}
	return append(args, tail...)
}

func compactNotice(n core.ParentNotice) string {
	text := strings.Join(strings.Fields(n.Text), " ")
	if len(text) > 320 {
		text = text[:319] + "…"
	}
	n.Text = text
	return core.FormatParentNotice(n)
}

// BindSurface makes an existing cmux surface authoritative for a relay
// session. It is used both when a named session claims the caller's pane and
// when cmux restores a pane under a new surface reference.
func (v *Viz) BindSurface(ctx context.Context, sessionID, attachCmd, surface string) (string, error) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		return "", fmt.Errorf("surface required")
	}
	sessName := extractSessionFlag(attachCmd)
	if sessName == "" {
		return "", fmt.Errorf("attach command has no --session")
	}
	title := brandTitle(sessName)
	if _, err := v.run(ctx, "surface", "resume", "set",
		"--surface", surface,
		"--kind", "relay",
		"--name", title,
		"--checkpoint", sessName,
		"--", attachCmd,
	); err != nil {
		return "", err
	}
	_ = v.brandSurface(ctx, surface, sessName)
	loc := v.locationOfSurface(ctx, surface)
	b := binding{
		V:         2,
		SessionID: sessionID,
		Surface:   surface,
		Pane:      loc.Pane,
		Workspace: loc.Workspace,
		Attach:    attachCmd,
		Mode:      "current",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if old, err := v.lookup(sessionID); err == nil {
		b.SourceSessionID = old.SourceSessionID
		if !old.CreatedAt.IsZero() {
			b.CreatedAt = old.CreatedAt
		}
		if old.Mode != "" {
			b.Mode = old.Mode
		}
	}
	v.mu.Lock()
	v.bindings[sessionID] = b
	v.mu.Unlock()
	if err := v.persistBinding(sessionID, b); err != nil {
		return "", err
	}
	v.rememberPane(sessionID, surface, sessName)
	return surface, nil
}

// RebindRenamedSession updates the existing surface checkpoint and restarts
// only its attach process. The remote tmux session itself remains alive.
func (v *Viz) RebindRenamedSession(ctx context.Context, sess *core.Session, attachCmd string) error {
	if sess == nil {
		return fmt.Errorf("session required")
	}
	b, err := v.lookup(sess.ID)
	if err != nil {
		return err
	}
	if b.Surface == "" || v.locationOfSurface(ctx, b.Surface).Workspace == "" {
		return fmt.Errorf("bound cmux surface for session %s is not live", sess.ID)
	}
	displayName := core.SessionDisplayName(sess)
	if _, err := v.run(ctx, "surface", "resume", "set",
		"--surface", b.Surface,
		"--kind", "relay",
		"--name", brandTitle(displayName),
		"--checkpoint", sess.Persist.Name,
		"--", attachCmd,
	); err != nil {
		return err
	}
	b.Attach = attachCmd
	b.UpdatedAt = time.Now().UTC()
	if err := v.persistBinding(sess.ID, b); err != nil {
		return err
	}
	core.RememberPane(b.Surface, sess, true)
	if _, err := v.run(ctx, "respawn-pane", "--surface", b.Surface, "--command", attachCmd); err != nil {
		return err
	}
	return v.brandSurface(ctx, b.Surface, displayName)
}

// WorkspaceForSurface resolves the cmux workspace that owns a recorded relay
// surface. It lets desktop-bridge requests route beside their true origin
// rather than whichever workspace the long-lived daemon last inherited.
func (v *Viz) WorkspaceForSurface(ctx context.Context, surface string) string {
	return v.workspaceOfSurface(ctx, surface)
}

// LocationForSurface resolves the workspace and pane owning a surface.
func (v *Viz) LocationForSurface(ctx context.Context, surface string) (string, string) {
	loc := v.locationOfSurface(ctx, surface)
	return loc.Workspace, loc.Pane
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
	dir := layout.SplitDirection
	if dir == "" {
		dir = "right"
	}
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
	args := []string{"rename-tab", "--surface", surface, "--title", title}
	if workspace := v.workspaceOfSurface(ctx, surface); workspace != "" {
		args = append(args, "--workspace", workspace)
	}
	_, err := v.run(ctx, args...)
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
		displayName := strings.TrimSpace(project)
		if displayName == "" {
			displayName = extractSessionFlag(b.Attach)
		}
		_ = v.brandSurface(ctx, b.Surface, displayName)
		ws := v.workspaceOfSurface(ctx, b.Surface)
		if ws == "" {
			continue
		}
		byWS[ws] = append(byWS[ws], hit{project: projectLabel(displayName)})
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
	out, err := v.run(ctx, "tree", "--all", "--json")
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
	return v.locationOfSurface(ctx, surface).Workspace
}

func (v *Viz) locationOfSurface(ctx context.Context, surface string) surfaceLocation {
	return v.surfaceLocations(ctx)[surface]
}

func (v *Viz) surfaceLocations(ctx context.Context) map[string]surfaceLocation {
	// --all so a surface is resolvable regardless of which window is focused;
	// plain `tree --json` only reports the current window.
	out, err := v.run(ctx, "tree", "--all", "--json")
	if err != nil {
		return map[string]surfaceLocation{}
	}
	return parseSurfaceLocations([]byte(out))
}

func parseSurfaceLocation(out []byte, surface string) surfaceLocation {
	return parseSurfaceLocations(out)[surface]
}

func parseSurfaceLocations(out []byte) map[string]surfaceLocation {
	locations := map[string]surfaceLocation{}
	var root struct {
		Windows []struct {
			Workspaces []struct {
				Ref   string `json:"ref"`
				Panes []struct {
					Ref      string `json:"ref"`
					Surfaces []struct {
						Ref string `json:"ref"`
					} `json:"surfaces"`
				} `json:"panes"`
			} `json:"workspaces"`
		} `json:"windows"`
	}
	if json.Unmarshal(out, &root) != nil {
		return locations
	}
	for _, w := range root.Windows {
		for _, ws := range w.Workspaces {
			for _, p := range ws.Panes {
				for _, s := range p.Surfaces {
					if s.Ref != "" {
						locations[s.Ref] = surfaceLocation{Workspace: ws.Ref, Pane: p.Ref}
					}
				}
			}
		}
	}
	return locations
}

func bindingMode(layout ports.Layout) string {
	if layout.Tab {
		return "tab"
	}
	return "split"
}

func childLayout(layout ports.Layout, sibling binding) ports.Layout {
	if layout.SourceSessionID == "" || layout.ExplicitPlace || layout.Tab {
		return layout
	}
	layout.SplitDirection = "right"
	if sibling.Pane != "" {
		layout.Workspace = sibling.Workspace
		layout.Pane = sibling.Pane
		layout.SplitDirection = "down"
	}
	return layout
}

// latestLiveChild returns the newest live sibling in a parent's child stack.
// The first child splits right from the parent; each later child splits down
// from this anchor, producing a stable right-hand column.
func (v *Viz) latestLiveChild(ctx context.Context, sourceSessionID string) binding {
	dir := filepath.Join(core.StateRoot(), "viz")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return binding{}
	}
	locations := v.surfaceLocations(ctx)
	var latest binding
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var candidate binding
		if json.Unmarshal(raw, &candidate) != nil || candidate.SourceSessionID != sourceSessionID {
			continue
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			if candidate.CreatedAt.IsZero() {
				candidate.CreatedAt = info.ModTime().UTC()
			}
			if candidate.UpdatedAt.IsZero() {
				candidate.UpdatedAt = candidate.CreatedAt
			}
		}
		loc := locations[candidate.Surface]
		if loc.Pane == "" {
			continue
		}
		candidate.Workspace, candidate.Pane = loc.Workspace, loc.Pane
		candidateTime := candidate.CreatedAt
		if candidateTime.IsZero() {
			candidateTime = candidate.UpdatedAt
		}
		latestTime := latest.CreatedAt
		if latestTime.IsZero() {
			latestTime = latest.UpdatedAt
		}
		if latest.Pane == "" || candidateTime.After(latestTime) {
			latest = candidate
		}
	}
	return latest
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (v *Viz) rememberPane(sessionID, surface, persistName string) {
	if sess, err := (&core.Registry{}).GetSession(sessionID); err == nil {
		core.RememberPane(surface, sess, true)
		return
	}
	cwd, _ := os.Getwd()
	core.RememberPanePersist(surface, persistName, "", "", cwd, true)
}

func extractSessionFlag(cmd string) string {
	fields := strings.Fields(cmd)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "--session" || fields[i] == "-s" {
			return strings.Trim(fields[i+1], "'\"\\")
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

	persist := extractSessionFlag(b.Attach)
	// Close the exact bound surface, then mop up any DUPLICATE panes bound to
	// the same session. A re-present from a fresh process used to leak a new
	// split each call, and destroy only knew the last binding — orphaning the
	// earlier panes with a dead `relay resume`. Collecting by relay checkpoint
	// (never by unrelated titles) closes them all.
	closed := map[string]bool{}
	v.closeSurface(ctx, b.Surface)
	closed[b.Surface] = true
	if persist != "" {
		for ref := range v.surfacesForPersist(ctx, persist) {
			if closed[ref] {
				continue
			}
			v.closeSurface(ctx, ref)
			closed[ref] = true
		}
		// Drop lingering local pane-state files for this session so a later
		// cmux restore won't try to resurrect the destroyed remote.
		core.RemovePaneBindingsForPersist(persist)
	}
	return nil
}

// ClosePersist retires every session binding whose attach command names the
// cleaned tmux session. It works even when cmux is stopped, so local ownership
// state cannot resurrect an intentionally removed remote later.
func (v *Viz) ClosePersist(ctx context.Context, persistName string) int {
	if persistName == "" {
		return 0
	}
	dir := filepath.Join(core.StateRoot(), "viz")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var b binding
		if json.Unmarshal(raw, &b) != nil || extractSessionFlag(b.Attach) != persistName {
			continue
		}
		if b.SessionID == "" {
			b.SessionID = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}
		_ = v.Close(ctx, b.SessionID)
		removed++
	}
	return removed
}

// closeSurface closes one surface scoped to its own workspace (cmux short refs
// resolve per-window, so a bare --surface fails not_found when the surface is
// not in the focused window) and drops its local pane-state file. Best-effort:
// a surface already gone is not an error.
func (v *Viz) closeSurface(ctx context.Context, surface string) {
	if surface == "" {
		return
	}
	if ws := v.workspaceOfSurface(ctx, surface); ws != "" {
		_, _ = v.run(ctx, "close-surface", "--surface", surface, "--workspace", ws)
	}
	_ = core.RemovePaneBinding(surface)
}

// surfacesForPersist returns every cmux surface whose relay checkpoint targets
// persistName — i.e. duplicate panes presented for the same session.
func (v *Viz) surfacesForPersist(ctx context.Context, persistName string) map[string]bool {
	out := map[string]bool{}
	if persistName == "" {
		return out
	}
	for ref := range v.relayBoundSurfaces(ctx) {
		if name, _, _ := v.relayCheckpoint(ctx, ref); name == persistName {
			out[ref] = true
		}
	}
	return out
}

func (v *Viz) Layout(ctx context.Context) (string, error) {
	out, err := v.run(ctx, "list-panes", "--json")
	if err != nil {
		return v.run(ctx, "tree", "--json")
	}
	return out, nil
}

// activeWorkspace returns the caller's cmux workspace when Relay was launched
// from a cmux terminal. This must win over the globally focused workspace:
// another window may be focused by the time Relay asks cmux, and explicitly
// passing that workspace to new-split would otherwise override cmux's normal
// caller-workspace routing. Outside cmux, fall back to the focused workspace
// so blind use remains supported.
func (v *Viz) activeWorkspace(ctx context.Context) string {
	if workspace := strings.TrimSpace(os.Getenv("CMUX_WORKSPACE_ID")); workspace != "" {
		return workspace
	}
	out, err := v.run(ctx, "list-panes", "--json")
	if err != nil {
		return ""
	}
	return parseWorkspaceRef([]byte(out))
}

func parseWorkspaceRef(out []byte) string {
	var doc struct {
		WorkspaceRef string `json:"workspace_ref"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return ""
	}
	return doc.WorkspaceRef
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
		name, _, cwd := v.relayCheckpoint(ctx, ref)
		if name == "" {
			continue
		}
		sess, findErr := (&core.Registry{}).FindByPersistName(name, cwd)
		displayName := name
		if findErr == nil {
			displayName = core.SessionDisplayName(sess)
		}
		// cmux returns a shell-rendered command from resume get. Reusing that
		// value compounds quoting on every save, so always regenerate the
		// canonical Relay argv from the checkpoint identity.
		cmd := core.ResumeLaunchCmd(name)
		if _, err := v.run(ctx, "surface", "resume", "set",
			"--surface", ref,
			"--kind", "relay",
			"--name", brandTitle(displayName),
			"--checkpoint", name,
			"--", cmd,
		); err == nil {
			_ = v.brandSurface(ctx, ref, displayName)
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			if findErr == nil {
				core.RememberPane(ref, sess, true)
				loc := v.locationOfSurface(ctx, ref)
				b := binding{
					V: 2, SessionID: sess.ID, Surface: ref, Pane: loc.Pane,
					Workspace: loc.Workspace, Attach: cmd, Mode: "current",
					CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
				}
				if old, lookupErr := v.lookup(sess.ID); lookupErr == nil {
					b.SourceSessionID = old.SourceSessionID
					if !old.CreatedAt.IsZero() {
						b.CreatedAt = old.CreatedAt
					}
					if old.Mode != "" {
						b.Mode = old.Mode
					}
				}
				_ = v.persistBinding(sess.ID, b)
			} else {
				core.RememberPanePersist(ref, name, "", "", cwd, true)
			}
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
	surfaces, err := v.listSurfaces(ctx)
	if err != nil {
		return 0, err
	}
	restored := 0
	for ref := range surfaces {
		if live[ref] {
			continue
		}
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
		Ref                string   `json:"ref"`
		SelectedSurfaceRef string   `json:"selected_surface_ref"`
		SurfaceRefs        []string `json:"surface_refs"`
	} `json:"panes"`
}

func (v *Viz) listSurfaces(ctx context.Context) (map[string]string, error) {
	// Prefer tree: list-panes often returns only the focused workspace's
	// selected pane, so before/after diffs miss newly split surfaces.
	out, err := v.run(ctx, "tree", "--all", "--json")
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
	dir := filepath.Join(core.StateRoot(), "viz")
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
	b.V = 2
	b.SessionID = sessionID
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now().UTC()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = b.UpdatedAt
	}
	raw, _ := json.MarshalIndent(b, "", "  ")
	return os.WriteFile(bindPath(sessionID), raw, 0o600)
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
	if b.SessionID == "" {
		b.SessionID = sessionID
	}
	return b, nil
}

// ManagedPanes reports every desktop-owned session binding and whether its
// exact surface still exists. This is intentionally session-keyed; titles are
// only used by legacy recovery in SaveRestorable.
func (v *Viz) ManagedPanes(ctx context.Context) ([]ManagedPane, error) {
	dir := filepath.Join(core.StateRoot(), "viz")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ManagedPane{}, nil
		}
		return nil, err
	}
	locations := v.surfaceLocations(ctx)
	panes := make([]ManagedPane, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var b binding
		if json.Unmarshal(raw, &b) != nil || b.Surface == "" {
			continue
		}
		legacySessionID := b.SessionID == ""
		if legacySessionID {
			b.SessionID = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}
		migrated := legacySessionID
		if info, infoErr := entry.Info(); infoErr == nil {
			if b.CreatedAt.IsZero() {
				b.CreatedAt = info.ModTime().UTC()
				migrated = true
			}
			if b.UpdatedAt.IsZero() {
				b.UpdatedAt = b.CreatedAt
				migrated = true
			}
		}
		state := "disconnected"
		if loc := locations[b.Surface]; loc.Workspace != "" {
			state = "live"
			if b.Workspace != loc.Workspace || b.Pane != loc.Pane {
				b.Workspace, b.Pane, b.UpdatedAt = loc.Workspace, loc.Pane, time.Now().UTC()
				migrated = true
			}
		}
		if migrated {
			_ = v.persistBinding(b.SessionID, b)
		}
		panes = append(panes, ManagedPane{
			SessionID: b.SessionID, SourceSessionID: b.SourceSessionID,
			PersistName: extractSessionFlag(b.Attach), Surface: b.Surface,
			Pane: b.Pane, Workspace: b.Workspace, Mode: b.Mode,
			State: state, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
		})
	}
	sort.Slice(panes, func(i, j int) bool {
		if panes[i].Workspace != panes[j].Workspace {
			return panes[i].Workspace < panes[j].Workspace
		}
		if panes[i].Pane != panes[j].Pane {
			return panes[i].Pane < panes[j].Pane
		}
		return panes[i].SessionID < panes[j].SessionID
	})
	return panes, nil
}

// CaptureScreen reads the visible text of the pane bound to a session,
// implementing core.ScreenCapturer.
//
// A cmux-backed session has no tmux server behind it, so the persistence
// adapter cannot read it — capture used to fail with "no server running",
// naming a subsystem that was never involved. That left every local pane,
// including root manager panes, unreadable.
func (v *Viz) CaptureScreen(ctx context.Context, sessionID string, lines int) (string, error) {
	b, err := v.lookup(sessionID)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 50
	}
	args := surfaceCommand("read-screen", b.Surface, b.Workspace,
		"--lines", strconv.Itoa(lines))
	out, err := v.run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("read pane for %s: %w", sessionID, err)
	}
	return out, nil
}
