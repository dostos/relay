package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dostos/relay/internal/shellquote"
)

// RelayBin returns an absolute path to the relay binary when possible.
func RelayBin() string {
	if v := os.Getenv("RELAY_BIN"); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			return abs
		}
		return exe
	}
	if p, err := exec.LookPath("relay"); err == nil {
		return p
	}
	return "relay"
}

// ResumeLaunchCmd is the restorable pane command for cmux Vault / viz save.
// Argv always carries --session <persistName> so cmux can extract a session id.
func ResumeLaunchCmd(persistName string) string {
	return fmt.Sprintf("%s resume --session %s", RelayBin(), persistName)
}

// FindByPersistName returns the best matching local session for a tmux persist name.
// When cwd is set, prefers a session whose RepoRef contains that cwd.
func (r *Registry) FindByPersistName(persistName, cwd string) (*Session, error) {
	if err := shellquote.ValidateSessionName(persistName); err != nil {
		return nil, err
	}
	list, err := r.ListSessions()
	if err != nil {
		return nil, err
	}
	var matches []*Session
	for _, s := range list {
		if s.Persist.Name == persistName {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no local session with persist name %q — was it created on this machine?", persistName)
	}
	if cwd != "" {
		cwdAbs, _ := filepath.Abs(cwd)
		for _, s := range matches {
			if s.RepoRef != "" && (strings.HasPrefix(cwdAbs, s.RepoRef) || strings.Contains(s.RepoRef, cwdAbs)) {
				cp := *s
				return &cp, nil
			}
		}
	}
	// most recently updated
	best := matches[0]
	for _, s := range matches[1:] {
		if s.UpdatedAt.After(best.UpdatedAt) {
			best = s
		}
	}
	cp := *best
	return &cp, nil
}

// Resume re-attaches to a still-alive remote tmux session (cmux Vault target).
func (s *SessionService) Resume(ctx context.Context, persistName, cwd string) error {
	if cwd != "" {
		_ = os.Chdir(cwd)
	}
	sess, err := s.Reg.FindByPersistName(persistName, cwd)
	if err != nil {
		return err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return err
	}
	cmd := s.Persist.AttachCommand(sess.Persist, sess.RemoteCWD)
	return t.Interactive(ctx, cmd)
}
