package bridge

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerAuthorityBoundaryRunsAfterIdentity(t *testing.T) {
	srv := &Server{
		RelayBin: "/bin/echo",
		Authorize: func(source Source) error {
			if source.Token != "valid" {
				return fmt.Errorf("identity rejected")
			}
			return nil
		},
		AuthorizeRequest: func(_ Source, argv []string) error {
			if len(argv) > 0 && argv[0] == "root" {
				return fmt.Errorf("policy denied")
			}
			return nil
		},
	}
	if got := srv.invoke(Request{Argv: []string{"root", "status"}, Source: Source{Token: "bad"}}); got.Error != "identity rejected" {
		t.Fatalf("identity must fail first: %+v", got)
	}
	if got := srv.invoke(Request{Argv: []string{"root", "status"}, Source: Source{Token: "valid"}}); got.Error != "policy denied" {
		t.Fatalf("boundary denial missing: %+v", got)
	}
}

func TestClientLifecycleReachesAuthorityBoundary(t *testing.T) {
	for _, argv := range [][]string{{"client", "update"}, {"client", "list"}, {"client", "status"}} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("syntax rejected %v: %v", argv, err)
		}
	}
}

func TestServerInvoke(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bridge.sock")
	srv := &Server{SockPath: sock, RelayBin: "/bin/echo", Build: "test-build"}
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
	status, err := client.Status(ctx)
	if err != nil || status.Build != "test-build" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	resp, err := client.Invoke(ctx, []string{"c1", "named"}, Source{SessionID: "sess-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Build != "test-build" || resp.ExitCode != 0 || strings.TrimSpace(resp.Stdout) != "c1 named" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRejectInteractiveForward(t *testing.T) {
	for _, argv := range [][]string{{"resume", "--session", "x"}, {"resume", "list"}, {"resume", "reap", "--dry-run"}, {"session", "attach", "x"}} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("syntax rejected %v: %v", argv, err)
		}
	}
}

func TestBridgeAllowlist(t *testing.T) {
	for _, argv := range [][]string{
		{"c1", "named"}, {"agent", "start", "c1", "codex", "--", "x"}, {"resolve", "pm-1", "yes"}, {"log", "0"}, {"--json", "history"}, {"session", "list"}, {"session", "list", "--json"},
		{"resume", "list", "--probe"}, {"--json", "resume", "list", "--probe"}, {"parent", "send", "sess-child", "--", "review"}, {"doctor"}, {"doctor", "-H", "c1"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("expected %v to be allowed: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"host", "bootstrap", "-H", "c1"}, {"auth", "copy"},
		{"session", "send", "sess-x", "--", "review"},
		{"parent", "register", "--surface", "surface:1"}, {"parent", "link", "sess-p", "ho-1"},
		{"parent", "retire", "sess-p"}, {"parent", "watch", "ho-1"}, {"policy", "list"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("expected %v to reach policy: %v", argv, err)
		}
	}
}

func TestDesktopInvokeEnvDropsStaleCmuxCaller(t *testing.T) {
	got := strings.Join(desktopInvokeEnv([]string{
		"PATH=/bin", "CMUX_WORKSPACE_ID=workspace:old", "CMUX_SURFACE_REF=surface:old", "RELAY_CMUX_BIN=/cmux",
		SourceSessionEnv + "=sess-poison", SourceTokenEnv + "=token-poison",
	}), "\n")
	if strings.Contains(got, "workspace:old") || strings.Contains(got, "surface:old") {
		t.Fatalf("stale caller context survived: %s", got)
	}
	if !strings.Contains(got, "PATH=/bin") || !strings.Contains(got, "RELAY_CMUX_BIN=/cmux") {
		t.Fatalf("unrelated environment was removed: %s", got)
	}
	if strings.Contains(got, "sess-poison") || strings.Contains(got, "token-poison") {
		t.Fatalf("stale authenticated source survived: %s", got)
	}
}

func TestSerializeInvocationDoesNotBlockWaits(t *testing.T) {
	for _, argv := range [][]string{{"c1", "named"}, {"agent", "start", "c1", "codex", "--", "x"}, {"agent", "done", "ho-1"}, {"resolve", "pm-1", "yes"}, {"session", "cleanup", "sess-child"}} {
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

func TestBridgeAllowsOnlySessionDiscoveryAndScopedCleanup(t *testing.T) {
	for _, argv := range [][]string{
		{"session", "list"}, {"--json", "session", "list"}, {"session", "list", "--json"},
		{"session", "cleanup", "sess-child"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("argv %v rejected: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"session", "get", "sess-child"}, {"session", "list", "extra"},
		{"session", "destroy", "sess-child"}, {"session", "cleanup", "sess-child", "--unknown"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("argv %v must reach policy: %v", argv, err)
		}
	}
}

// The apex digest is the human's decision queue; it must not be reachable by
// an arbitrary session through the two-token host shorthand.
func TestBridgeDefersRootAndBoardToAuthorityBoundary(t *testing.T) {
	for _, argv := range [][]string{
		{"root", "digest"}, {"root", "status"}, {"--json", "root", "digest"},
		{"root", "adopt", "sess-x"}, {"root", "enroll", "sess-x"},
	} {
		if err := validateArgv(argv); err != nil {
			t.Fatalf("argv %v must reach policy: %v", argv, err)
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
	if err := validateArgv([]string{"board", "destroy"}); err != nil {
		t.Fatal("syntactically valid command must reach policy")
	}
}
