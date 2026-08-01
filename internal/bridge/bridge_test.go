package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerInvoke(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bridge.sock")
	srv := &Server{SockPath: sock, RelayBin: "/bin/echo"}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := Client{SockPath: sock}
	for client.Ping(ctx) != nil {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	resp, err := client.Invoke(ctx, []string{"c1", "named"}, Source{SessionID: "sess-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ExitCode != 0 || strings.TrimSpace(resp.Stdout) != "c1 named" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRejectInteractiveForward(t *testing.T) {
	if err := validateArgv([]string{"resume", "--session", "x"}); err == nil {
		t.Fatal("expected resume to be rejected")
	}
	if err := validateArgv([]string{"session", "attach", "x"}); err == nil {
		t.Fatal("expected session attach to be rejected")
	}
}

func TestBridgeAllowlist(t *testing.T) {
	for _, argv := range [][]string{
		{"c1", "named"}, {"agent", "start", "c1", "codex", "--", "x"}, {"resolve", "pm-1", "yes"}, {"log", "0"}, {"--json", "history"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("expected %v to be allowed: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"host", "bootstrap", "-H", "c1"}, {"auth", "copy"}, {"session", "destroy", "sess-x"},
		{"parent", "register", "--surface", "surface:1"}, {"parent", "link", "sess-p", "ho-1"},
		{"parent", "retire", "sess-p"}, {"parent", "watch", "ho-1"}, {"policy", "list"},
	} {
		if err := validateArgv(argv); err == nil {
			t.Fatalf("expected %v to be rejected", argv)
		}
	}
}

func TestDesktopInvokeEnvDropsStaleCmuxCaller(t *testing.T) {
	got := strings.Join(desktopInvokeEnv([]string{
		"PATH=/bin", "CMUX_WORKSPACE_ID=workspace:old", "CMUX_SURFACE_REF=surface:old", "RELAY_CMUX_BIN=/cmux",
	}), "\n")
	if strings.Contains(got, "workspace:old") || strings.Contains(got, "surface:old") {
		t.Fatalf("stale caller context survived: %s", got)
	}
	if !strings.Contains(got, "PATH=/bin") || !strings.Contains(got, "RELAY_CMUX_BIN=/cmux") {
		t.Fatalf("unrelated environment was removed: %s", got)
	}
}

func TestSerializeInvocationDoesNotBlockWaits(t *testing.T) {
	for _, argv := range [][]string{{"c1", "named"}, {"agent", "start", "c1", "codex", "--", "x"}, {"agent", "done", "ho-1"}, {"resolve", "pm-1", "yes"}} {
		if !serializeInvocation(argv) {
			t.Fatalf("expected %v to serialize", argv)
		}
	}
	for _, argv := range [][]string{{"agent", "wait", "ho-1"}, {"agent", "capture", "ho-1"}, {"log", "0"}, {"history"}} {
		if serializeInvocation(argv) {
			t.Fatalf("expected %v not to serialize", argv)
		}
	}
}

// The apex digest is the human's decision queue; it must not be reachable by
// an arbitrary session through the two-token host shorthand.
func TestBridgeRefusesRootAndAllowsBoard(t *testing.T) {
	for _, argv := range [][]string{
		{"root", "digest"}, {"root", "status"}, {"--json", "root", "digest"},
		{"root", "adopt", "sess-x"}, {"root", "enroll", "sess-x"},
	} {
		if err := validateArgv(argv); err == nil {
			t.Fatalf("argv %v must be refused through the bridge", argv)
		}
	}
	// Boards are the peer coordination surface and must be reachable.
	for _, argv := range [][]string{
		{"board", "query", "-c", "status"},
		{"board", "post", "-c", "status", "-k", "phase", "--", "hi"},
		{"board", "watch", "-c", "status"},
		{"board", "query", "-c", "status", "--subtree"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("argv %v must be allowed: %v", argv, err)
		}
	}
	if err := validateArgv([]string{"board", "destroy"}); err == nil {
		t.Fatal("unknown board subcommand must be refused")
	}
}
