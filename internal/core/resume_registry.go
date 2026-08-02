package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// ResumeState distinguishes intentional teardown from transport/cmux disconnect.
type ResumeState string

const (
	// ResumeStateResumable: remote work may still be alive; cmux/SSH drop — OK to resume.
	ResumeStateResumable ResumeState = "resumable"
	// ResumeStateCleaned: intentional destroy/finalize — do NOT resume or recreate.
	ResumeStateCleaned ResumeState = "cleaned"
)

// ErrResumeCleaned means the session was intentionally torn down.
var ErrResumeCleaned = errors.New("session was cleaned (finalized/destroyed); not a disconnect")

// ResumeEntry maps a tmux persist name → host + lifecycle for cmux Vault resume.
type ResumeEntry struct {
	HostID    string      `json:"host_id"`
	SessionID string      `json:"session_id,omitempty"`
	RemoteCWD string      `json:"remote_cwd,omitempty"`
	RepoRef   string      `json:"repo_ref,omitempty"`
	State     ResumeState `json:"state"`
	Reason    string      `json:"reason,omitempty"`
	UpdatedAt time.Time   `json:"updated_at"`
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
	// Migrate legacy entries (no state) → resumable.
	for k, e := range f.Entries {
		if e.State == "" {
			e.State = ResumeStateResumable
			f.Entries[k] = e
		}
	}
	return &f, nil
}

func saveResumeRegistry(f *resumeRegistryFile) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
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

// RememberResume marks a session resumable (create/present/disconnect path).
func RememberResume(sess *Session) {
	upsertResume(sess, ResumeStateResumable, "active_or_disconnect")
}

// RenameResumePersist moves the durable resume key after a persistence rename.
func RenameResumePersist(oldName string, sess *Session) error {
	if sess == nil {
		return fmt.Errorf("session required")
	}
	if err := shellquote.ValidateSessionName(oldName); err != nil {
		return err
	}
	if err := shellquote.ValidateSessionName(sess.Persist.Name); err != nil {
		return err
	}
	f, err := loadResumeRegistry()
	if err != nil {
		return err
	}
	delete(f.Entries, oldName)
	f.Entries[sess.Persist.Name] = ResumeEntry{
		HostID: sess.HostID, SessionID: sess.ID, RemoteCWD: sess.RemoteCWD,
		RepoRef: sess.RepoRef, State: ResumeStateResumable,
		Reason: "renamed", UpdatedAt: time.Now().UTC(),
	}
	return saveResumeRegistry(f)
}

// MarkResumeCleaned records intentional teardown — resume must refuse.
func MarkResumeCleaned(persistName, reason string) {
	if err := shellquote.ValidateSessionName(persistName); err != nil {
		return
	}
	f, err := loadResumeRegistry()
	if err != nil {
		return
	}
	e := f.Entries[persistName]
	e.State = ResumeStateCleaned
	if reason == "" {
		reason = "destroyed"
	}
	e.Reason = reason
	e.UpdatedAt = time.Now().UTC()
	// keep host/cwd for diagnostics
	f.Entries[persistName] = e
	_ = saveResumeRegistry(f)
}

func upsertResume(sess *Session, state ResumeState, reason string) {
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
		State:     state,
		Reason:    reason,
		UpdatedAt: time.Now().UTC(),
	}
	_ = saveResumeRegistry(f)
}

// PruneResume drops registry entries. When cleanedOnly is set only cleaned
// tombstones are considered; otherwise every state is eligible. When olderThan
// > 0 only entries last updated before now-olderThan are dropped. Returns the
// removed persist names. Bounds the tombstone growth that otherwise only clears
// on the 60-day sweep in saveResumeRegistry.
func PruneResume(cleanedOnly bool, olderThan time.Duration) ([]string, error) {
	f, err := loadResumeRegistry()
	if err != nil {
		return nil, err
	}
	// A negative duration would put the cutoff in the future and match every
	// entry — treat it as "no age filter" instead.
	if olderThan < 0 {
		olderThan = 0
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	var removed []string
	for k, e := range f.Entries {
		if cleanedOnly && e.State != ResumeStateCleaned {
			continue
		}
		if olderThan > 0 && e.UpdatedAt.After(cutoff) {
			continue
		}
		removed = append(removed, k)
		delete(f.Entries, k)
	}
	if len(removed) > 0 {
		if err := saveResumeRegistry(f); err != nil {
			return nil, err
		}
	}
	return removed, nil
}

// LookupResume finds a registry entry (any state).
func LookupResume(persistName string) (*ResumeEntry, error) {
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
	cp := e
	return &cp, nil
}

// ResumePresence is the agent-facing classification.
type ResumePresence string

const (
	// PresenceLive: local session record exists (likely still connected from this machine).
	PresenceLive ResumePresence = "live"
	// PresenceDisconnected: no live local record, but resumable — cmux/SSH drop; remote may be up.
	PresenceDisconnected ResumePresence = "disconnected"
	// PresenceCleaned: intentional teardown; do not resume.
	PresenceCleaned ResumePresence = "cleaned"
	// PresenceUnknown: never seen on this machine.
	PresenceUnknown ResumePresence = "unknown"
)

// ClassifyResume returns live | disconnected | cleaned | unknown for a persist name.
func (r *Registry) ClassifyResume(persistName string) (ResumePresence, *ResumeEntry, *Session) {
	var live *Session
	if s, err := r.FindByPersistName(persistName, ""); err == nil {
		live = s
	}
	e, _ := LookupResume(persistName)
	// Live local record wins (e.g. name reused after a prior clean).
	if live != nil {
		return PresenceLive, e, live
	}
	if e != nil && e.State == ResumeStateCleaned {
		return PresenceCleaned, e, nil
	}
	if e != nil && e.State == ResumeStateResumable {
		return PresenceDisconnected, e, nil
	}
	return PresenceUnknown, e, nil
}

// ResolveResumeTarget returns host + persist handle for attach.
// Cleaned sessions error with ErrResumeCleaned (do not recreate remote).
func (r *Registry) ResolveResumeTarget(persistName, cwd string) (hostID, remoteCWD string, h ports.PersistHandle, presence ResumePresence, err error) {
	presence, e, live := r.ClassifyResume(persistName)
	switch presence {
	case PresenceCleaned:
		reason := "cleaned"
		if e != nil && e.Reason != "" {
			reason = e.Reason
		}
		return "", "", ports.PersistHandle{}, presence, fmt.Errorf("%w (%s)", ErrResumeCleaned, reason)
	case PresenceLive:
		return live.HostID, live.RemoteCWD, live.Persist, presence, nil
	case PresenceDisconnected:
		return e.HostID, e.RemoteCWD, ports.PersistHandle{Kind: "tmux", Name: persistName}, presence, nil
	default:
		return "", "", ports.PersistHandle{}, presence, fmt.Errorf(
			"unknown session %q — not live and not in resume registry (%s)",
			persistName, ResumeRegistryPath(),
		)
	}
}
