package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// PaneBinding pins a remote session to a cmux surface so bare `relay resume`
// (and reconnect) recover the same session — not whichever repo was last used.
type PaneBinding struct {
	Surface     string    `json:"surface"`
	PersistName string    `json:"persist_name"`
	HostID      string    `json:"host_id,omitempty"`
	RemoteCWD   string    `json:"remote_cwd,omitempty"`
	CWD         string    `json:"cwd,omitempty"`
	Pinned      bool      `json:"pinned"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PanesDir holds per-surface resume history.
func PanesDir() string { return filepath.Join(StateRoot(), "panes") }

func paneBindingPath(surface string) string {
	safe := sanitizeID(normalizeSurface(surface))
	return filepath.Join(PanesDir(), safe+".json")
}

func normalizeSurface(surface string) string {
	s := strings.TrimSpace(surface)
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "surface:") {
		return s
	}
	// numeric id / bare token → surface:N
	if !strings.Contains(s, ":") {
		return "surface:" + s
	}
	return s
}

// WritePaneBinding stores pane-specific resume history.
func WritePaneBinding(b PaneBinding) error {
	if strings.TrimSpace(b.Surface) == "" || strings.TrimSpace(b.PersistName) == "" {
		return nil
	}
	b.Surface = normalizeSurface(b.Surface)
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(PanesDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(paneBindingPath(b.Surface), raw, 0o644)
}

// RememberPane writes a pinned binding from a live session + surface.
func RememberPane(surface string, sess *Session, pinned bool) {
	if sess == nil || surface == "" {
		return
	}
	cwd, _ := os.Getwd()
	_ = WritePaneBinding(PaneBinding{
		Surface:     surface,
		PersistName: sess.Persist.Name,
		HostID:      sess.HostID,
		RemoteCWD:   sess.RemoteCWD,
		CWD:         firstNonEmpty(sess.RepoRef, cwd),
		Pinned:      pinned,
	})
}

// RememberPanePersist writes a minimal pinned binding (present/save paths).
func RememberPanePersist(surface, persistName, hostID, remoteCWD, cwd string, pinned bool) {
	_ = WritePaneBinding(PaneBinding{
		Surface:     surface,
		PersistName: persistName,
		HostID:      hostID,
		RemoteCWD:   remoteCWD,
		CWD:         cwd,
		Pinned:      pinned,
	})
}

// RemovePaneBinding deletes the local resume history for one surface.
// Missing files are not an error.
func RemovePaneBinding(surface string) error {
	err := os.Remove(paneBindingPath(surface))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RemovePaneBindingsForPersist deletes every local pane-history file that pins
// the given persist name. Used on intentional teardown so stale surface_*.json
// don't try to resurrect a destroyed session on the next cmux restore. Returns
// the number removed.
func RemovePaneBindingsForPersist(persistName string) int {
	if strings.TrimSpace(persistName) == "" {
		return 0
	}
	entries, err := os.ReadDir(PanesDir())
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(PanesDir(), e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var b PaneBinding
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		if b.PersistName == persistName {
			if os.Remove(p) == nil {
				removed++
			}
		}
	}
	return removed
}

// ReadPaneBinding loads history for a surface.
func ReadPaneBinding(surface string) (*PaneBinding, error) {
	surface = normalizeSurface(surface)
	raw, err := os.ReadFile(paneBindingPath(surface))
	if err != nil {
		return nil, err
	}
	var b PaneBinding
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	if b.PersistName == "" {
		return nil, fmt.Errorf("empty pane binding for %s", surface)
	}
	b.Surface = surface
	return &b, nil
}

// CurrentSurface returns the cmux surface running this process, if any.
func CurrentSurface() (string, error) {
	for _, k := range []string{"CMUX_SURFACE_REF", "CMUX_SURFACE"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return normalizeSurface(v), nil
		}
	}
	if identifySurface != nil {
		if s, err := identifySurface(); err == nil && s != "" {
			return normalizeSurface(s), nil
		}
	}
	return "", fmt.Errorf("not inside a cmux surface (set CMUX_SURFACE_REF or pass --session)")
}

// identifySurface is swappable for tests; default shells out to cmux identify.
var identifySurface = defaultIdentifySurface

func defaultIdentifySurface() (string, error) {
	bin := os.Getenv("RELAY_CMUX_BIN")
	if bin == "" {
		bin = "cmux"
	}
	out, err := exec.Command(bin, "identify", "--json").Output()
	if err != nil {
		return "", err
	}
	var root struct {
		Caller struct {
			SurfaceRef string `json:"surface_ref"`
		} `json:"caller"`
		Focused struct {
			SurfaceRef string `json:"surface_ref"`
		} `json:"focused"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		return "", err
	}
	if root.Caller.SurfaceRef != "" {
		return root.Caller.SurfaceRef, nil
	}
	if root.Focused.SurfaceRef != "" {
		return root.Focused.SurfaceRef, nil
	}
	return "", fmt.Errorf("cmux identify: no surface")
}

// cmuxResumeCheckpoint reads kind=relay binding from cmux (fallback when local history missing).
var cmuxResumeCheckpoint = defaultCmuxResumeCheckpoint

func defaultCmuxResumeCheckpoint(surface string) (persistName, cwd string, err error) {
	bin := os.Getenv("RELAY_CMUX_BIN")
	if bin == "" {
		bin = "cmux"
	}
	out, err := exec.Command(bin, "surface", "resume", "get", "--surface", normalizeSurface(surface), "--json").Output()
	if err != nil {
		return "", "", err
	}
	var wrap struct {
		ResumeBinding struct {
			Kind         string `json:"kind"`
			CheckpointID string `json:"checkpoint_id"`
			Command      string `json:"command"`
			CWD          string `json:"cwd"`
		} `json:"resume_binding"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return "", "", err
	}
	rb := wrap.ResumeBinding
	if rb.Kind != "" && rb.Kind != "relay" {
		return "", "", fmt.Errorf("surface %s binding kind %q is not relay", surface, rb.Kind)
	}
	name := strings.TrimSpace(rb.CheckpointID)
	if name == "" {
		name = extractSessionFlag(rb.Command)
	}
	if name == "" {
		return "", "", fmt.Errorf("no relay checkpoint on %s", surface)
	}
	return name, rb.CWD, nil
}

func extractSessionFlag(cmd string) string {
	fields := strings.Fields(cmd)
	for i := 0; i < len(fields); i++ {
		f := strings.Trim(fields[i], "'\"")
		if f == "--session" || f == "-s" {
			if i+1 < len(fields) {
				return strings.Trim(fields[i+1], "'\"")
			}
		}
		if strings.HasPrefix(f, "--session=") {
			return strings.TrimPrefix(f, "--session=")
		}
	}
	return ""
}

// ResolveResumeFromPane picks persist name for bare `relay resume` from this surface.
// Prefers pinned local pane history; falls back to cmux surface resume get.
func ResolveResumeFromPane() (persistName, cwd, surface string, err error) {
	surface, err = CurrentSurface()
	if err != nil {
		return "", "", "", err
	}
	if b, e := ReadPaneBinding(surface); e == nil && b.Pinned && b.PersistName != "" {
		return b.PersistName, b.CWD, surface, nil
	}
	name, c, e := cmuxResumeCheckpoint(surface)
	if e != nil || name == "" {
		if b, e2 := ReadPaneBinding(surface); e2 == nil && b.PersistName != "" {
			// Unpinned local history still beats nothing.
			return b.PersistName, b.CWD, surface, nil
		}
		if e != nil {
			return "", "", surface, fmt.Errorf("no pane history for %s: %w", surface, e)
		}
		return "", "", surface, fmt.Errorf("no pane history for %s; pass --session NAME", surface)
	}
	// Cache cmux binding locally so reconnect stays pane-stable.
	_ = WritePaneBinding(PaneBinding{
		Surface:     surface,
		PersistName: name,
		CWD:         c,
		Pinned:      true,
		UpdatedAt:   time.Now().UTC(),
	})
	return name, c, surface, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
