package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// ResumeEntry maps a tmux persist name → host for cmux Vault resume.
// Survives session destroy so panes can re-attach after cmux restart.
type ResumeEntry struct {
	HostID    string    `json:"host_id"`
	SessionID string    `json:"session_id,omitempty"`
	RemoteCWD string    `json:"remote_cwd,omitempty"`
	RepoRef   string    `json:"repo_ref,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type resumeRegistryFile struct {
	Entries map[string]ResumeEntry `json:"entries"`
}

// ResumeRegistryPath is the durable persist-name → host map.
func ResumeRegistryPath() string {
	return filepath.Join(StateRoot(), "resume-registry.json")
}

func loadResumeRegistry() (*resumeRegistryFile, error) {
	if err := EnsureStateDirs(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(ResumeRegistryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &resumeRegistryFile{Entries: map[string]ResumeEntry{}}, nil
		}
		return nil, err
	}
	var f resumeRegistryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Entries == nil {
		f.Entries = map[string]ResumeEntry{}
	}
	return &f, nil
}

func saveResumeRegistry(f *resumeRegistryFile) error {
	// prune > 60 days
	cutoff := time.Now().UTC().Add(-60 * 24 * time.Hour)
	for k, e := range f.Entries {
		if e.UpdatedAt.Before(cutoff) {
			delete(f.Entries, k)
		}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := ResumeRegistryPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ResumeRegistryPath())
}

// RememberResume writes/updates the durable resume binding for a session.
// Best-effort: never fails the caller’s main path.
func RememberResume(sess *Session) {
	if sess == nil || sess.Persist.Name == "" || sess.HostID == "" {
		return
	}
	if err := shellquote.ValidateSessionName(sess.Persist.Name); err != nil {
		return
	}
	f, err := loadResumeRegistry()
	if err != nil {
		return
	}
	f.Entries[sess.Persist.Name] = ResumeEntry{
		HostID:    sess.HostID,
		SessionID: sess.ID,
		RemoteCWD: sess.RemoteCWD,
		RepoRef:   sess.RepoRef,
		UpdatedAt: time.Now().UTC(),
	}
	_ = saveResumeRegistry(f)
}

// LookupResume finds host binding for a persist name (cwd disambiguates collisions).
func LookupResume(persistName, cwd string) (*ResumeEntry, error) {
	if err := shellquote.ValidateSessionName(persistName); err != nil {
		return nil, err
	}
	f, err := loadResumeRegistry()
	if err != nil {
		return nil, err
	}
	e, ok := f.Entries[persistName]
	if !ok {
		return nil, fmt.Errorf("persist name %q not in resume registry", persistName)
	}
	if cwd != "" && e.RepoRef != "" {
		cwdAbs, _ := filepath.Abs(cwd)
		if !(strings.HasPrefix(cwdAbs, e.RepoRef) || strings.Contains(e.RepoRef, cwdAbs)) {
			// still accept — registry is 1:1 by persist name; cwd is soft preference only
		}
	}
	cp := e
	return &cp, nil
}

// ResolveResumeTarget returns host + persist handle for attach, preferring live
// session records then the durable resume registry.
func (r *Registry) ResolveResumeTarget(persistName, cwd string) (hostID, remoteCWD string, h ports.PersistHandle, err error) {
	if sess, err := r.FindByPersistName(persistName, cwd); err == nil {
		return sess.HostID, sess.RemoteCWD, sess.Persist, nil
	}
	e, err := LookupResume(persistName, cwd)
	if err != nil {
		return "", "", ports.PersistHandle{}, fmt.Errorf(
			"no resume binding for %q (not in live sessions or %s) — create/present a session on this machine first",
			persistName, ResumeRegistryPath(),
		)
	}
	return e.HostID, e.RemoteCWD, ports.PersistHandle{Kind: "tmux", Name: persistName}, nil
}
