// Package controlbridge keeps every relay session connected to the control
// bridge on the always-on host. Display clients are not part of this path.
package controlbridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dostos/relay/internal/core"
)

const reconcileInterval = 5 * time.Second

type Service struct {
	Registry     *core.Registry
	BridgeSocket string
	Stderr       *os.File

	mu      sync.Mutex
	tunnels map[string]context.CancelFunc
}

func (s *Service) Run(ctx context.Context) error {
	if s.Registry == nil || s.BridgeSocket == "" {
		return fmt.Errorf("control bridge registry and socket required")
	}
	s.tunnels = make(map[string]context.CancelFunc)
	defer s.stopAll()
	for {
		if err := s.reconcile(ctx); err != nil && s.Stderr != nil {
			fmt.Fprintf(s.Stderr, "relayd control bridge reconcile: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(reconcileInterval):
		}
	}
}

func (s *Service) reconcile(ctx context.Context) error {
	sessions, err := s.Registry.ListSessions()
	if err != nil {
		return err
	}
	localHost, err := localHostID()
	if err != nil {
		return err
	}
	wanted := make(map[string]bool)
	var errs []string
	for _, session := range sessions {
		if !hasToken(session.ID) || session.HostID == core.LocalHostID {
			continue
		}
		wanted[session.ID] = true
		if session.HostID == localHost || session.HostID == "self" {
			if err := linkLocalSocket(core.BridgeRemoteSocket(session.ID), s.BridgeSocket); err != nil {
				errs = append(errs, err.Error())
			}
			continue
		}
		s.ensureTunnel(ctx, session.ID, session.HostID)
	}
	s.mu.Lock()
	for id, cancel := range s.tunnels {
		if !wanted[id] {
			cancel()
			delete(s.tunnels, id)
		}
	}
	s.mu.Unlock()
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func localHostID() (string, error) {
	raw, err := os.ReadFile(filepath.Join(core.ConfigRoot(), "host.yaml"))
	if err != nil {
		return "", fmt.Errorf("read local host profile: %w", err)
	}
	profile, err := core.ParseHostProfileYAML(raw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(profile.HostID) == "" {
		return "", fmt.Errorf("local host profile has no host_id")
	}
	return profile.HostID, nil
}

func hasToken(sessionID string) bool {
	raw, err := os.ReadFile(filepath.Join(core.BridgeTokensDir(), sessionID+".token"))
	return err == nil && strings.TrimSpace(string(raw)) != ""
}

func linkLocalSocket(path, target string) error {
	if current, err := os.Readlink(path); err == nil && current == target {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace local bridge socket %s: %w", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("link local bridge socket %s: %w", path, err)
	}
	return nil
}

func (s *Service) ensureTunnel(parent context.Context, sessionID, host string) {
	if strings.HasPrefix(host, "-") || strings.ContainsAny(host, "\r\n\x00") {
		if s.Stderr != nil {
			fmt.Fprintf(s.Stderr, "relayd control bridge: invalid host for %s\n", sessionID)
		}
		return
	}
	s.mu.Lock()
	if _, ok := s.tunnels[sessionID]; ok {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.tunnels[sessionID] = cancel
	s.mu.Unlock()
	go s.runTunnel(ctx, sessionID, host)
}

func (s *Service) runTunnel(ctx context.Context, sessionID, host string) {
	defer func() {
		s.mu.Lock()
		delete(s.tunnels, sessionID)
		s.mu.Unlock()
	}()
	remote := core.BridgeRemoteSocket(sessionID)
	cleanup := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", host, "rm -f -- "+remote)
	cleanup.Stderr = s.Stderr
	if err := cleanup.Run(); err != nil || ctx.Err() != nil {
		return
	}
	args := tunnelArgs(remote, s.BridgeSocket, host)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stderr = s.Stderr
	_ = cmd.Run()
}

func tunnelArgs(remote, local, host string) []string {
	return []string{
		"-N", "-o", "BatchMode=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=4",
		"-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes",
		"-o", "StreamLocalBindMask=0177", "-R", remote + ":" + local, host,
	}
}

func (s *Service) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cancel := range s.tunnels {
		cancel()
		delete(s.tunnels, id)
	}
}
