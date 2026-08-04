package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dostos/relay/internal/shellquote"
)

var ErrSessionNotFound = errors.New("session not found")
var ErrProjectionOnlyAuthority = errors.New("local relay is visualization-only; authoritative registry is unavailable")

// Registry is the local durable store for sessions and handoffs.
type Registry struct {
	mu   sync.Mutex
	txMu sync.RWMutex
}

type sessionStore struct {
	Sessions map[string]*Session `json:"sessions"`
}

// EnsureAuthorityWritable is the single role boundary for durable control-plane
// stores. Projection code writes only under viz/ and must never call it.
func EnsureAuthorityWritable() error {
	if _, err := os.Lstat(ProjectionOnlyMarkerPath()); err == nil {
		return fmt.Errorf("local relay is visualization-only; authoritative registry mutation refused")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func EnsureAuthorityReadable() error {
	if _, err := os.Lstat(ProjectionOnlyMarkerPath()); err == nil {
		return ErrProjectionOnlyAuthority
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (r *Registry) loadSessions() (*sessionStore, error) {
	if err := EnsureStateDirs(); err != nil {
		return nil, err
	}
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(SessionsPath())
	if err != nil {
		if os.IsNotExist(err) {
			// Retirement may have moved sessions.json after the first marker
			// check. Never turn that transition into an authoritative empty
			// registry.
			if readableErr := EnsureAuthorityReadable(); readableErr != nil {
				return nil, readableErr
			}
			return &sessionStore{Sessions: map[string]*Session{}}, nil
		}
		return nil, err
	}
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	var s sessionStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Sessions == nil {
		s.Sessions = map[string]*Session{}
	}
	return &s, nil
}

func (r *Registry) saveSessions(s *sessionStore) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := SessionsPath() + ".tmp"
	if err := writeOwnerFile(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, SessionsPath())
}

func (r *Registry) PutSession(sess *Session) error {
	r.txMu.RLock()
	defer r.txMu.RUnlock()
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	return r.putSessionLocked(sess)
}

func (r *Registry) putSessionLocked(sess *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if managerDeletionReserved(sess.SourceSessionID) {
		return fmt.Errorf("manager %s is reserved for deletion", sess.SourceSessionID)
	}
	if sess.Persist.Name != "" && sess.Persist.Kind != LocalPersistKind {
		if err := shellquote.ValidateSessionName(sess.Persist.Name); err != nil {
			return fmt.Errorf("invalid persisted tmux name: %w", err)
		}
	}
	s, err := r.loadSessions()
	if err != nil {
		return err
	}
	sess.UpdatedAt = time.Now().UTC()
	s.Sessions[sess.ID] = sess
	return r.saveSessions(s)
}

func (r *Registry) GetSession(id string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.loadSessions()
	if err != nil {
		return nil, err
	}
	sess, ok := s.Sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	cp := *sess
	return &cp, nil
}

func (r *Registry) ListSessions() ([]*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.loadSessions()
	if err != nil {
		return nil, err
	}
	out := make([]*Session, 0, len(s.Sessions))
	for _, sess := range s.Sessions {
		cp := *sess
		out = append(out, &cp)
	}
	return out, nil
}

func (r *Registry) DeleteSession(id string) error {
	r.txMu.RLock()
	defer r.txMu.RUnlock()
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	return r.deleteSessionLocked(id)
}

func (r *Registry) deleteSessionLocked(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.loadSessions()
	if err != nil {
		return err
	}
	delete(s.Sessions, id)
	return r.saveSessions(s)
}

func handoffPath(id string) string {
	return HandoffsDir() + "/" + sanitizeID(id) + ".json"
}

func (r *Registry) PutHandoff(h *Handoff) error {
	r.txMu.RLock()
	defer r.txMu.RUnlock()
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	return r.putHandoffLocked(h)
}

func (r *Registry) putHandoffLocked(h *Handoff) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if managerDeletionReserved(h.SourceSessionID) {
		return fmt.Errorf("manager %s is reserved for deletion", h.SourceSessionID)
	}
	if err := EnsureStateDirs(); err != nil {
		return err
	}
	h.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	tmp := handoffPath(h.ID) + ".tmp"
	if err := writeOwnerFile(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, handoffPath(h.ID))
}

func (r *Registry) GetHandoff(id string) (*Handoff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(handoffPath(id))
	if err != nil {
		return nil, fmt.Errorf("handoff %q not found: %w", id, err)
	}
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	var h Handoff
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (r *Registry) ListHandoffs() ([]*Handoff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := EnsureStateDirs(); err != nil {
		return nil, err
	}
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(HandoffsDir())
	if err != nil {
		return nil, err
	}
	var out []*Handoff
	for _, e := range entries {
		if e.IsDir() || !stringsHasSuffix(e.Name(), ".json") || e.Name() == "ledger.jsonl" {
			continue
		}
		b, err := os.ReadFile(HandoffsDir() + "/" + e.Name())
		if err != nil {
			continue
		}
		var h Handoff
		if json.Unmarshal(b, &h) == nil {
			out = append(out, &h)
		}
	}
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	return out, nil
}

func stringsHasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

// AppendLedger writes one JSON line to the durable start/end ledger.
func AppendLedger(record map[string]any) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	return appendLedgerLocked(record)
}

func appendLedgerLocked(record map[string]any) error {
	if err := EnsureStateDirs(); err != nil {
		return err
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(LedgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
