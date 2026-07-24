package core

import (
	"context"
	"errors"
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
func ResumeLaunchCmd(persistName string) string {
	return fmt.Sprintf("%s resume --session %s", RelayBin(), persistName)
}

// FindByPersistName returns the best matching local session for a tmux persist name.
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

// Resume re-attaches after cmux/SSH disconnect. Refuses cleaned sessions.
// Like sst: on transport drop (laptop sleep, wifi, Shared connection closed)
// it waits and retries until a clean attach exit or Ctrl+C — it does not fall
// through to a local shell while a binding exists.
func (s *SessionService) Resume(ctx context.Context, persistName, cwd string) error {
	return s.ResumeOpts(ctx, persistName, cwd, ResumeOpts{})
}

// ResumeOpts controls attach reconnect behavior.
type ResumeOpts struct {
	NoReconnect bool // disable retry loop (also RELAY_AUTO_RECONNECT=0)
}

// ResumeOpts attaches with optional auto-reconnect (default on).
func (s *SessionService) ResumeOpts(ctx context.Context, persistName, cwd string, opts ResumeOpts) error {
	if cwd != "" {
		_ = os.Chdir(cwd)
	}
	hostID, remoteCWD, handle, presence, err := s.Reg.ResolveResumeTarget(persistName, cwd)
	if err != nil {
		return err
	}
	if presence == PresenceCleaned {
		return err
	}
	t, err := s.NewTransport(hostID)
	if err != nil {
		return err
	}
	// Rehydrate live local record when reconnecting a disconnect.
	if presence == PresenceDisconnected {
		now := time.Now().UTC()
		sess := &Session{
			ID:        newID("sess"),
			HostID:    hostID,
			RemoteCWD: remoteCWD,
			Persist:   handle,
			RepoRef:   cwd,
			Labels:    map[string]string{"role": "resumed"},
			CreatedAt: now,
			UpdatedAt: now,
		}
		_ = s.Reg.PutSession(sess)
		RememberResume(sess)
	}
	cmd := s.Persist.AttachCommand(handle, remoteCWD)
	reconnect := autoReconnectEnabled(opts.NoReconnect)
	delay := reconnectDelay()
	if reconnect {
		fmt.Fprintf(os.Stderr, "relay resume: auto-reconnect on (delay %s; RELAY_AUTO_RECONNECT=0 or --no-reconnect to disable)\n", delay)
	}
	attempt := 0
	for {
		attempt++
		err := t.Interactive(ctx, cmd)
		code := exitCode(err)
		if err == nil || !shouldRetryAttach(code) || !reconnect {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "relay resume: disconnected from %s (exit %d); reconnecting in %s (attempt %d, Ctrl+C to stop)\n",
			hostID, code, delay, attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func autoReconnectEnabled(noReconnect bool) bool {
	if noReconnect {
		return false
	}
	v := strings.TrimSpace(os.Getenv("RELAY_AUTO_RECONNECT"))
	if v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
		return false
	}
	return true
}

func reconnectDelay() time.Duration {
	v := strings.TrimSpace(os.Getenv("RELAY_RECONNECT_DELAY"))
	if v == "" {
		return 3 * time.Second
	}
	if sec, err := time.ParseDuration(v); err == nil {
		return sec
	}
	// plain seconds
	if n, err := time.ParseDuration(v + "s"); err == nil {
		return n
	}
	return 3 * time.Second
}

// shouldRetryAttach mirrors sst: retry after unexpected drop; not after clean exit or Ctrl+C.
func shouldRetryAttach(code int) bool {
	switch code {
	case 0, 130: // success / SIGINT
		return false
	default:
		return true
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	// context cancel / other — treat as non-retry terminal
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 130
	}
	// unknown error (e.g. ssh binary missing): retry once-ish via non-zero
	return 1
}

// ResumeInfo is one row for `relay resume list`.
type ResumeInfo struct {
	PersistName string         `json:"persist_name"`
	Presence    ResumePresence `json:"presence"` // live|disconnected|cleaned|unknown
	HostID      string         `json:"host_id,omitempty"`
	SessionID   string         `json:"session_id,omitempty"`
	RemoteCWD   string         `json:"remote_cwd,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	UpdatedAt   time.Time      `json:"updated_at,omitempty"`
}

// ListResumeStatus merges live sessions + registry for operators/agents.
func (s *SessionService) ListResumeStatus() ([]ResumeInfo, error) {
	f, err := loadResumeRegistry()
	if err != nil {
		return nil, err
	}
	live, err := s.Reg.ListSessions()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []ResumeInfo
	for _, sess := range live {
		name := sess.Persist.Name
		seen[name] = true
		presence, e, _ := s.Reg.ClassifyResume(name)
		info := ResumeInfo{
			PersistName: name,
			Presence:    presence,
			HostID:      sess.HostID,
			SessionID:   sess.ID,
			RemoteCWD:   sess.RemoteCWD,
		}
		if e != nil {
			info.Reason = e.Reason
			info.UpdatedAt = e.UpdatedAt
		}
		out = append(out, info)
	}
	for name, e := range f.Entries {
		if seen[name] {
			continue
		}
		presence, _, _ := s.Reg.ClassifyResume(name)
		out = append(out, ResumeInfo{
			PersistName: name,
			Presence:    presence,
			HostID:      e.HostID,
			SessionID:   e.SessionID,
			RemoteCWD:   e.RemoteCWD,
			Reason:      e.Reason,
			UpdatedAt:   e.UpdatedAt,
		})
	}
	return out, nil
}

// FormatResumeError makes cleaned vs unknown clear for CLI (no shell fallback on cleaned).
func FormatResumeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrResumeCleaned) {
		return err.Error() + " — close the cmux pane; do not treat this as a disconnect"
	}
	return err.Error()
}
