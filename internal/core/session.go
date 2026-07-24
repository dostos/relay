package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// TransportFactory creates a Transport for a host id.
type TransportFactory func(hostID string) (ports.Transport, error)

// SessionService manages durable sessions via Transport + Persistence adapters.
type SessionService struct {
	Reg       *Registry
	Profiles  *ProfileService
	NewTransport TransportFactory
	Persist   ports.Persistence
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// CreateOpts configures session creation.
type CreateOpts struct {
	HostID    string
	Name      string // optional persist name; default derived
	RepoRef   string // local git root
	RemoteCWD string // optional override (skips path_map)
	Command   string // optional initial command; default interactive shell
	Labels    map[string]string
}

// Create creates a remote persistent session and registers it locally.
func (s *SessionService) Create(ctx context.Context, opts CreateOpts) (*Session, error) {
	if opts.HostID == "" {
		return nil, fmt.Errorf("host_id required")
	}
	profile, err := s.Profiles.Get(ctx, opts.HostID, false)
	if err != nil {
		return nil, err
	}
	cwd := opts.RemoteCWD
	if cwd == "" {
		if opts.RepoRef == "" {
			return nil, fmt.Errorf("repo_ref or remote_cwd required")
		}
		cwd, err = profile.ResolveRemoteCWD(opts.RepoRef)
		if err != nil {
			return nil, err
		}
	}
	name := opts.Name
	if name == "" {
		base := "sess"
		if opts.RepoRef != "" {
			base = filepath.Base(opts.RepoRef)
		}
		name = base + "-" + newID("s")[2:8]
	}
	safe, err := shellquote.SanitizeSessionName(name)
	if err != nil {
		return nil, err
	}
	name = safe
	t, err := s.NewTransport(opts.HostID)
	if err != nil {
		return nil, err
	}
	cmd := opts.Command
	if cmd == "" {
		cmd = "bash -l"
	}
	h, err := s.Persist.Create(ctx, t, name, cwd, cmd)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sess := &Session{
		ID:        newID("sess"),
		HostID:    opts.HostID,
		RemoteCWD: cwd,
		Persist:   h,
		RepoRef:   opts.RepoRef,
		Labels:    opts.Labels,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	RememberResume(sess)
	return sess, nil
}

func (s *SessionService) transportFor(sess *Session) (ports.Transport, error) {
	return s.NewTransport(sess.HostID)
}

func (s *SessionService) Get(id string) (*Session, error) {
	return s.Reg.GetSession(id)
}

func (s *SessionService) List() ([]*Session, error) {
	return s.Reg.ListSessions()
}

func (s *SessionService) Capture(ctx context.Context, id string, lines int) (string, error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return "", err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 50
	}
	return s.Persist.Capture(ctx, t, sess.Persist, lines)
}

func (s *SessionService) Send(ctx context.Context, id, text string, enter bool) error {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return err
	}
	return s.Persist.Send(ctx, t, sess.Persist, text, enter)
}

func (s *SessionService) Exec(ctx context.Context, id, command string) (stdout, stderr string, err error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return "", "", err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return "", "", err
	}
	return t.Run(ctx, sess.RemoteCWD, command)
}

func (s *SessionService) Resize(ctx context.Context, id string) error {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return err
	}
	return s.Persist.Resize(ctx, t, sess.Persist)
}

func (s *SessionService) Attach(ctx context.Context, id string) error {
	sess, err := s.Reg.GetSession(id)
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

func (s *SessionService) AttachCommand(id string) (string, error) {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return "", err
	}
	return s.Persist.AttachCommand(sess.Persist, sess.RemoteCWD), nil
}

func (s *SessionService) Destroy(ctx context.Context, id string, keepRemote bool) error {
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return err
	}
	if !keepRemote {
		t, err := s.transportFor(sess)
		if err != nil {
			return err
		}
		_ = s.Persist.Destroy(ctx, t, sess.Persist)
		// Intentional teardown — cmux must not treat this as a reconnectable drop.
		MarkResumeCleaned(sess.Persist.Name, "destroyed")
	} else {
		// Local unbound; remote kept → disconnected/resumable.
		RememberResume(sess)
	}
	return s.Reg.DeleteSession(id)
}
