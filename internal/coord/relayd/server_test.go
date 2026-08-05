package relayd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "relay-coord-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSecondServerCannotUnlinkLiveSocket(t *testing.T) {
	dir := shortSocketDir(t)
	sock := filepath.Join(dir, "relayd.sock")
	store, err := NewStore(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	first := &Server{SockPath: sock, Store: store}
	done := make(chan error, 1)
	go func() { done <- first.Serve() }()
	t.Cleanup(func() { _ = first.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("unix", sock, 20*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first server did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := &Server{SockPath: sock, Store: store}
	if err := second.Serve(); err == nil {
		t.Fatal("second server replaced the live listener")
	}
	conn, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first listener was detached: %v", err)
	}
	_ = conn.Close()
}

func TestCloseIsSafeWhileServerIsAccepting(t *testing.T) {
	dir := shortSocketDir(t)
	sock := filepath.Join(dir, "relayd.sock")
	store, err := NewStore(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{SockPath: sock, Store: store}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("unix", sock, 20*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("Serve returned nil after listener close")
	}
}
