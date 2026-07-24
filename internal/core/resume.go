package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
		return nil, fmt.Errorf("no live session with persist name %q", persistName)
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
// Uses live session records first, then the durable resume registry (survives destroy).
func (s *SessionService) Resume(ctx context.Context, persistName, cwd string) error {
	if cwd != "" {
		_ = os.Chdir(cwd)
	}
	hostID, remoteCWD, handle, err := s.Reg.ResolveResumeTarget(persistName, cwd)
	if err != nil {
		return err
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		return err
	}
	// Ensure registry stays warm for the next cmux restart.
	RememberResume(&Session{
		ID:        "",
		HostID:    hostID,
		RemoteCWD: remoteCWD,
		Persist:   handle,
		RepoRef:   cwd,
		UpdatedAt: time.Now().UTC(),
	})
	// Rehydrate a live local session record if missing (so agent/list still work).
	if _, err := s.Reg.FindByPersistName(persistName, cwd); err != nil {
		now := time.Now().UTC()
		_ = s.Reg.PutSession(&Session{
			ID:        newID("sess"),
			HostID:    hostID,
			RemoteCWD: remoteCWD,
			Persist:   handle,
			RepoRef:   firstNonEmpty(cwd, ""),
			Labels:    map[string]string{"role": "resumed"},
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	cmd := s.Persist.AttachCommand(handle, remoteCWD)
	return t.Interactive(ctx, cmd)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
