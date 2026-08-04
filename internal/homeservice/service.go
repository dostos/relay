// Package homeservice owns Relay's authoritative always-on home process.
package homeservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/cli"
	"github.com/dostos/relay/internal/controlbridge"
	"github.com/dostos/relay/internal/coord"
	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/core"
	cmuxviz "github.com/dostos/relay/internal/viz/cmux"
)

const restartDelay = time.Second

type ComponentHealth struct {
	Name           string `json:"name"`
	Build          string `json:"build"`
	Ready          bool   `json:"ready"`
	Live           bool   `json:"live"`
	DurableEffects bool   `json:"durable_effects"`
	RestartCount   int    `json:"restart_count"`
	LastSuccess    string `json:"last_success,omitempty"`
	LastFailure    string `json:"last_failure,omitempty"`
	Error          string `json:"error,omitempty"`
}

type Health struct {
	V          int                        `json:"v"`
	Build      string                     `json:"build"`
	PID        int                        `json:"pid"`
	StartedAt  string                     `json:"started_at"`
	UpdatedAt  string                     `json:"updated_at"`
	Ready      bool                       `json:"ready"`
	Live       bool                       `json:"live"`
	Stopping   bool                       `json:"stopping,omitempty"`
	Components map[string]ComponentHealth `json:"components"`
}

// Component is independently supervised inside the shared home process.
// Run must block until it fails or ctx is cancelled and call ready only after
// its durable state and externally visible endpoint are usable.
type Component struct {
	Name string
	Run  func(ctx context.Context, ready func(durable bool)) error
}

type Service struct {
	Components   []Component
	HealthPath   string
	LockPath     string
	RestartDelay time.Duration

	mu     sync.Mutex
	health Health
}

func New() *Service {
	s := &Service{HealthPath: core.HomeServiceHealthPath(), LockPath: core.HomeServiceLockPath(), RestartDelay: restartDelay}
	s.Components = []Component{
		{Name: "event_coordinator", Run: runEventCoordinator},
		{Name: "command_boundary", Run: runCommandBoundary},
		{Name: "watcher_reconciler", Run: runWatcherReconciler},
	}
	return s
}

func (s *Service) Run(ctx context.Context) error {
	if len(s.Components) == 0 {
		return fmt.Errorf("home service requires components")
	}
	if err := core.EnsureStateDirs(); err != nil {
		return err
	}
	if err := core.EnsureAuthorityWritable(); err != nil {
		return fmt.Errorf("home authority unavailable: %w", err)
	}
	lockPath := s.LockPath
	if lockPath == "" {
		lockPath = core.HomeServiceLockPath()
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another authoritative relay service owns %s", lockPath)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	s.health = Health{V: 1, Build: coord.Build, PID: os.Getpid(), StartedAt: now, UpdatedAt: now, Live: true, Components: map[string]ComponentHealth{}}
	for _, component := range s.Components {
		s.health.Components[component.Name] = ComponentHealth{Name: component.Name, Build: coord.Build}
	}
	s.recomputeLocked()
	if err := s.writeHealthLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	// Component contexts deliberately do not inherit the caller cancellation.
	// Shutdown is ordered in reverse dependency order: watchers stop first,
	// command/forwarding drains next, and the event coordinator remains available
	// until every producer has stopped.
	serviceCtx, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-serviceCtx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				s.health.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				_ = s.writeHealthLocked()
				s.mu.Unlock()
			}
		}
	}()
	type runningComponent struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	running := make([]runningComponent, 0, len(s.Components))
	for _, component := range s.Components {
		component := component
		componentCtx, componentCancel := context.WithCancel(serviceCtx)
		done := make(chan struct{})
		running = append(running, runningComponent{cancel: componentCancel, done: done})
		go func() {
			defer close(done)
			s.supervise(componentCtx, component)
		}()
	}
	<-ctx.Done()
	s.mu.Lock()
	s.health.Stopping = true
	s.health.Ready = false
	s.health.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.writeHealthLocked()
	s.mu.Unlock()
	for i := len(running) - 1; i >= 0; i-- {
		running[i].cancel()
		select {
		case <-running[i].done:
		case <-time.After(5 * time.Second):
			cancelAll()
		}
	}
	cancelAll()
	<-heartbeatDone
	s.mu.Lock()
	s.health.Live = false
	s.health.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.recomputeLocked()
	_ = s.writeHealthLocked()
	s.mu.Unlock()
	return nil
}

func (s *Service) supervise(ctx context.Context, component Component) {
	delay := s.RestartDelay
	if delay <= 0 {
		delay = restartDelay
	}
	first := true
	for ctx.Err() == nil {
		s.updateComponent(component.Name, func(state *ComponentHealth) {
			state.Live = true
			state.Ready = false
			state.DurableEffects = false
			state.Error = ""
			if !first {
				state.RestartCount++
			}
		})
		first = false
		ready := func(durable bool) {
			s.updateComponent(component.Name, func(state *ComponentHealth) {
				state.Ready = true
				state.Live = true
				state.DurableEffects = durable
				state.LastSuccess = time.Now().UTC().Format(time.RFC3339Nano)
				state.Error = ""
			})
		}
		err := component.Run(ctx, ready)
		if ctx.Err() != nil {
			s.updateComponent(component.Name, func(state *ComponentHealth) {
				state.Ready = false
				state.Live = false
			})
			return
		}
		s.updateComponent(component.Name, func(state *ComponentHealth) {
			state.Ready = false
			state.Live = false
			state.LastFailure = time.Now().UTC().Format(time.RFC3339Nano)
			if err == nil {
				state.Error = "component exited unexpectedly"
			} else {
				state.Error = err.Error()
			}
		})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) updateComponent(name string, update func(*ComponentHealth)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.health.Components[name]
	update(&state)
	s.health.Components[name] = state
	s.health.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.recomputeLocked()
	_ = s.writeHealthLocked()
}

func (s *Service) recomputeLocked() {
	ready := len(s.health.Components) > 0 && !s.health.Stopping
	for _, state := range s.health.Components {
		if !state.Ready || !state.Live || !state.DurableEffects || state.Build != s.health.Build {
			ready = false
			break
		}
	}
	s.health.Ready = ready
}

func (s *Service) writeHealthLocked() error {
	path := s.HealthPath
	if path == "" {
		path = core.HomeServiceHealthPath()
	}
	raw, err := json.MarshalIndent(s.health, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".service-health-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if tmp.Chmod(0o600) != nil || func() error { _, err := tmp.Write(raw); return err }() != nil || tmp.Sync() != nil || tmp.Close() != nil {
		_ = tmp.Close()
		return fmt.Errorf("write relay service health receipt")
	}
	return os.Rename(tmpPath, path)
}

func ReadHealth() (*Health, error) {
	raw, err := os.ReadFile(core.HomeServiceHealthPath())
	if err != nil {
		return nil, err
	}
	var health Health
	if err := json.Unmarshal(raw, &health); err != nil {
		return nil, err
	}
	if health.V != 1 || health.Build == "" || len(health.Components) == 0 {
		return nil, fmt.Errorf("invalid relay service health receipt")
	}
	return &health, nil
}

func eventSocketPath() (string, error) {
	if value := os.Getenv("RELAYD_SOCK"); value != "" {
		return value, nil
	}
	sock, _, err := coordrelayd.DefaultPaths()
	return sock, err
}

func runEventCoordinator(ctx context.Context, ready func(bool)) error {
	sock, err := eventSocketPath()
	if err != nil {
		return err
	}
	store, err := coordrelayd.NewStore(filepath.Join(core.StateRoot(), "events"))
	if err != nil {
		return err
	}
	server := &coordrelayd.Server{SockPath: sock, Store: store}
	return serveSocketComponent(ctx, func() error { return server.Serve() }, server.Close, func() error {
		_, err := coordrelayd.PingLocal(sock)
		return err
	}, ready)
}

func runCommandBoundary(ctx context.Context, ready func(bool)) error {
	sock := core.DesktopBridgeSocketPath()
	if _, err := core.EnsureHomeClientIdentity(); err != nil {
		return err
	}
	server := &bridge.Server{SockPath: sock, RelayBin: relayBinary(), Build: coord.Build, Authorize: core.AuthorizeBridgeSource, AuthorizeRequest: core.AuthorizeBridgeRequest, ReceiptDir: core.CommandReceiptDir()}
	componentCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bridgeErr := make(chan error, 1)
	go func() { bridgeErr <- server.Serve() }()
	if err := waitReady(componentCtx, func() error { return bridge.Client{SockPath: sock}.Ping(componentCtx) }); err != nil {
		_ = server.Close()
		return err
	}
	controlReady := make(chan struct{}, 1)
	app := cli.New()
	viz := cmuxviz.New()
	control := &controlbridge.Service{
		Registry: app.Reg, BridgeSocket: sock, Stderr: os.Stderr,
		AckSync: func(ctx context.Context) error { return viz.SyncAcks(ctx, app.Reg) },
		Ready: func() {
			select {
			case controlReady <- struct{}{}:
			default:
			}
		},
	}
	controlErr := make(chan error, 1)
	go func() { controlErr <- control.Run(componentCtx) }()
	select {
	case <-componentCtx.Done():
		_ = server.Close()
		return nil
	case err := <-bridgeErr:
		return err
	case err := <-controlErr:
		_ = server.Close()
		return err
	case <-controlReady:
		ready(true)
	}
	select {
	case <-componentCtx.Done():
		_ = server.Close()
		return nil
	case err := <-bridgeErr:
		cancel()
		return err
	case err := <-controlErr:
		_ = server.Close()
		return err
	}
}

func runWatcherReconciler(ctx context.Context, ready func(bool)) error {
	app := cli.New()
	supervisor := &core.SupervisorService{
		Reg: app.Reg, Parents: app.Parents, Interval: time.Second,
		ReconcileHandoffs: func(ctx context.Context) (int, error) {
			return app.Handoffs.Reconcile(ctx)
		},
		RepairSensors: func(ctx context.Context, sessionID string) error {
			return app.Handoffs.ReinstallSensors(ctx, sessionID, 0)
		},
	}
	if _, err := supervisor.Reconcile(ctx); err != nil {
		return err
	}
	ready(true)
	err := supervisor.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func serveSocketComponent(ctx context.Context, serve func() error, closeServer func() error, probe func() error, ready func(bool)) error {
	errCh := make(chan error, 1)
	go func() { errCh <- serve() }()
	if err := waitReady(ctx, probe); err != nil {
		_ = closeServer()
		return err
	}
	ready(true)
	select {
	case <-ctx.Done():
		_ = closeServer()
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

func waitReady(ctx context.Context, probe func() error) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if err := probe(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("component readiness timeout")
		case <-ticker.C:
		}
	}
}

func relayBinary() string {
	if value := os.Getenv("RELAY_BIN"); value != "" {
		return value
	}
	if executable, err := os.Executable(); err == nil {
		if filepath.Base(executable) == "relay" {
			return executable
		}
		candidate := filepath.Join(filepath.Dir(executable), "relay")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if path, err := exec.LookPath("relay"); err == nil {
		return path
	}
	return "relay"
}

func SocketOwner(path string) bool {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
