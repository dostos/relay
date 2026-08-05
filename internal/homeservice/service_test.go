package homeservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
)

func TestServiceRestartsFailedComponentWithoutCancellingHealthySibling(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var flakyRuns atomic.Int32
	var stableStopped atomic.Bool
	service := &Service{
		HealthPath: filepath.Join(root, "health.json"), LockPath: filepath.Join(root, "service.lock"), RestartDelay: time.Millisecond,
		Components: []Component{
			{Name: "stable", Run: func(ctx context.Context, ready func(bool)) error {
				ready(true)
				<-ctx.Done()
				stableStopped.Store(true)
				return nil
			}},
			{Name: "flaky", Run: func(ctx context.Context, ready func(bool)) error {
				if flakyRuns.Add(1) == 1 {
					return errors.New("transient failure")
				}
				ready(true)
				<-ctx.Done()
				return nil
			}},
		},
	}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		health := readTestHealth(service.HealthPath)
		return health != nil && health.Ready && health.Components["flaky"].RestartCount == 1
	})
	if stableStopped.Load() {
		t.Fatal("healthy sibling was cancelled during component restart")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServiceReportsDurableEffectFailureAsUnready(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", root)
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		HealthPath: filepath.Join(root, "health.json"), LockPath: filepath.Join(root, "service.lock"),
		Components: []Component{{Name: "inert", Run: func(ctx context.Context, ready func(bool)) error {
			ready(false)
			<-ctx.Done()
			return nil
		}}},
	}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		health := readTestHealth(service.HealthPath)
		return health != nil && health.Components["inert"].Ready
	})
	health := readTestHealth(service.HealthPath)
	if health.Ready || health.Components["inert"].DurableEffects {
		t.Fatalf("inert component left aggregate green: %+v", health)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServiceShutdownIsReverseDependencyOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", root)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var stopped []string
	components := make([]Component, 0, 3)
	for _, name := range []string{"event", "boundary", "watcher"} {
		name := name
		components = append(components, Component{Name: name, Run: func(ctx context.Context, ready func(bool)) error {
			ready(true)
			<-ctx.Done()
			mu.Lock()
			stopped = append(stopped, name)
			mu.Unlock()
			return nil
		}})
	}
	service := &Service{HealthPath: filepath.Join(root, "health.json"), LockPath: filepath.Join(root, "service.lock"), Components: components}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	waitFor(t, time.Second, func() bool {
		health := readTestHealth(service.HealthPath)
		return health != nil && health.Ready
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"watcher", "boundary", "event"}
	if len(stopped) != len(want) {
		t.Fatalf("shutdown order=%v", stopped)
	}
	for i := range want {
		if stopped[i] != want[i] {
			t.Fatalf("shutdown order=%v want=%v", stopped, want)
		}
	}
}

func TestServicePreventsConcurrentAuthorityOwner(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	component := Component{Name: "only", Run: func(ctx context.Context, ready func(bool)) error {
		ready(true)
		<-ctx.Done()
		return nil
	}}
	first := &Service{HealthPath: filepath.Join(root, "first-health.json"), LockPath: filepath.Join(root, "service.lock"), Components: []Component{component}}
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	waitFor(t, time.Second, func() bool { return readTestHealth(first.HealthPath) != nil })
	second := &Service{HealthPath: filepath.Join(root, "second-health.json"), LockPath: filepath.Join(root, "service.lock"), Components: []Component{component}}
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	if err := second.Run(secondCtx); err == nil {
		t.Fatal("second authoritative service acquired the same lock")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEventCoordinatorCursorSurvivesComponentRestart(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "relay-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	sock := filepath.Join(root, "relayd.sock")
	t.Setenv("RELAY_STATE_DIR", root)
	t.Setenv("RELAYD_SOCK", sock)
	runOnce := func(emitKind string, wantSeq int64) {
		ctx, cancel := context.WithCancel(context.Background())
		ready := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() { done <- runEventCoordinator(ctx, func(bool) { ready <- struct{}{} }) }()
		select {
		case <-ready:
		case <-time.After(time.Second):
			t.Fatal("event coordinator did not become ready")
		}
		response, err := coordrelayd.EmitLocal(sock, "worker", emitKind, nil)
		if err != nil || response.Seq != wantSeq {
			t.Fatalf("emit kind=%s response=%+v err=%v", emitKind, response, err)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	runOnce("started", 1)
	runOnce("result", 2)
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func readTestHealth(path string) *Health {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var health Health
	if json.Unmarshal(raw, &health) != nil {
		return nil
	}
	return &health
}
