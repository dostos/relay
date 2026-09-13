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

// Registry is the local durable store for sessions and handoffs.
type Registry struct {
	mu   sync.Mutex
	txMu sync.RWMutex
}

type sessionStore struct {
	Sessions map[string]*Session `json:"sessions"`
}

func (r *Registry) loadSessions() (*sessionStore, error) {
	if err := EnsureStateDirs(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(SessionsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &sessionStore{Sessions: map[string]*Session{}}, nil
		}
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
	if sess.Persist.Name != "" {
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

func stringsHasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

// DirectChildren lists the sessions a session launched. This is the launch
// edge, not an ownership edge: it survived the hierarchy retirement because
// teardown still has to know whether anything it started is still running.
func (r *Registry) DirectChildren(sessionID string) ([]*Session, error) {
	sessions, err := r.ListSessions()
	if err != nil {
		return nil, err
	}
	var children []*Session
	for _, sess := range sessions {
		if sess.SourceSessionID == sessionID {
			children = append(children, sess)
		}
	}
	return children, nil
}
