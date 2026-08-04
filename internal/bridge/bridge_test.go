package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestReceiptReturnsOneEffectAcrossRetries(t *testing.T) {
	root := t.TempDir()
	effects := filepath.Join(root, "effects")
	server := &Server{RelayBin: "/bin/sh", ReceiptDir: filepath.Join(root, "receipts")}
	request := Request{V: 2, Op: "invoke", RequestID: "0123456789abcdef0123456789abcdef", Argv: []string{"-c", "printf x >> \"$1\"; printf confirmed", "relay-test", effects}, Source: Source{SessionID: "sess-source"}}
	first := server.invoke(request)
	second := server.invoke(request)
	if !first.OK || !second.OK || first.Stdout != "confirmed" || second.Stdout != first.Stdout {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	raw, err := os.ReadFile(effects)
	if err != nil || string(raw) != "x" {
		t.Fatalf("effect=%q err=%v", raw, err)
	}
	request.Argv = []string{"-c", "printf different"}
	if got := server.invoke(request); got.OK || !strings.Contains(got.Error, "conflicts") {
		t.Fatalf("request id accepted different payload: %+v", got)
	}
}

func TestPendingRequestReceiptPreventsAmbiguousReplay(t *testing.T) {
	root := t.TempDir()
	server := &Server{RelayBin: "/bin/echo", ReceiptDir: root}
	request := Request{V: 2, Op: "invoke", RequestID: "fedcba9876543210fedcba9876543210", Argv: []string{"effect"}, Source: Source{SessionID: "sess-source"}}
	receipt := commandReceipt{V: 1, RequestID: request.RequestID, SourceID: request.Source.SessionID, ArgvDigest: requestArgvDigest(request.Argv), State: "pending"}
	raw, _ := json.Marshal(receipt)
	if err := os.WriteFile(filepath.Join(root, request.RequestID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := server.invoke(request); got.OK || !strings.Contains(got.Error, "intentionally not repeated") {
		t.Fatalf("ambiguous effect was replayed: %+v", got)
	}
}

func TestCancelledDeliveryResumesFromCompletedReceiptWithoutRepeatingEffect(t *testing.T) {
	root := t.TempDir()
	effects := filepath.Join(root, "effects")
	sock := filepath.Join(root, "bridge.sock")
	server := &Server{SockPath: sock, RelayBin: "/bin/sh", ReceiptDir: filepath.Join(root, "receipts")}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := Client{SockPath: sock}
	for client.Ping(ctx) != nil {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	request := Request{V: 2, Op: "invoke", RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Argv: []string{"-c", "sleep 0.05; printf x >> \"$1\"; printf confirmed", "relay-test", effects}, Source: Source{SessionID: "sess-source"}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	// Closing before the response models a cancelled/lost client. The boundary
	// must finish and receipt the claimed effect, never infer that it did not run.
	_ = conn.Close()
	for {
		response, callErr := client.call(ctx, request)
		if callErr == nil && response.OK {
			if response.Stdout != "confirmed" {
				t.Fatalf("cached response=%+v", response)
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("completed receipt unavailable: response=%+v err=%v", response, callErr)
		}
		time.Sleep(time.Millisecond)
	}
	effect, err := os.ReadFile(effects)
	if err != nil || string(effect) != "x" {
		t.Fatalf("effect=%q err=%v", effect, err)
	}
}

func TestPartiallyWrittenRequestReceiptFailsClosed(t *testing.T) {
	root := t.TempDir()
	effects := filepath.Join(root, "effects")
	server := &Server{RelayBin: "/bin/sh", ReceiptDir: root}
	request := Request{V: 2, Op: "invoke", RequestID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Argv: []string{"-c", "printf x >> \"$1\"", "relay-test", effects}, Source: Source{SessionID: "sess-source"}}
	if err := os.WriteFile(filepath.Join(root, request.RequestID+".json"), []byte(`{"v":1,"request_id":"bbbb`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := server.invoke(request); got.OK || !strings.Contains(got.Error, "conflicts") {
		t.Fatalf("partial receipt did not fail closed: %+v", got)
	}
	if _, err := os.Stat(effects); !os.IsNotExist(err) {
		t.Fatalf("effect ran despite partial receipt: %v", err)
	}
}

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

func TestServerPreventsConcurrentSocketOwnership(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bridge.sock")
	first := &Server{SockPath: sock, RelayBin: "/bin/echo"}
	done := make(chan error, 1)
	go func() { done <- first.Serve() }()
	t.Cleanup(func() { _ = first.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := Client{SockPath: sock}
	for client.Ping(ctx) != nil {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	second := &Server{SockPath: sock, RelayBin: "/bin/echo"}
	if err := second.Serve(); err == nil {
		t.Fatal("second bridge stole the authoritative socket")
	}
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("first bridge was disrupted: %v", err)
	}
}

func TestServerSurvivesMalformedPartialAndOversizedRequests(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bridge.sock")
	server := &Server{SockPath: sock, RelayBin: "/bin/echo"}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := Client{SockPath: sock}
	for client.Ping(ctx) != nil {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}

	partial, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = partial.Write([]byte(`{"v":1,"op":"invoke"`))
	_ = partial.(*net.UnixConn).CloseWrite()
	_ = partial.SetReadDeadline(time.Now().Add(time.Second))
	line, _ := bufio.NewReader(partial).ReadString('\n')
	_ = partial.Close()
	if !strings.Contains(line, "bad bridge request") {
		t.Fatalf("partial request response=%q", line)
	}

	oversized, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = oversized.Write([]byte(strings.Repeat("x", maxMessageBytes+1) + "\n"))
	_ = oversized.(*net.UnixConn).CloseWrite()
	_ = oversized.SetReadDeadline(time.Now().Add(time.Second))
	line, _ = bufio.NewReader(oversized).ReadString('\n')
	_ = oversized.Close()
	if !strings.Contains(line, "exceeds") {
		t.Fatalf("oversized request response=%q", line)
	}
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("malformed request killed boundary: %v", err)
	}
}

func TestServerBoundsCommandOutputAndReceipts(t *testing.T) {
	root := t.TempDir()
	server := &Server{RelayBin: "/bin/sh", ReceiptDir: filepath.Join(root, "receipts")}
	request := Request{V: 2, Op: "invoke", RequestID: "cccccccccccccccccccccccccccccccc", Argv: []string{"-c", "head -c 1100000 /dev/zero"}, Source: Source{SessionID: "sess-source"}}
	response := server.invoke(request)
	if response.OK || !strings.Contains(response.Error, "output exceeds") || len(response.Stdout) != maxCommandOutputBytes {
		t.Fatalf("unbounded response: ok=%t stdout=%d error=%q", response.OK, len(response.Stdout), response.Error)
	}
	path := filepath.Join(server.ReceiptDir, request.RequestID+".json")
	if info, err := os.Stat(path); err != nil || info.Size() > maxCommandReceiptBytes {
		t.Fatalf("receipt size=%v err=%v", info, err)
	}

	unsafeID := "dddddddddddddddddddddddddddddddd"
	unsafePath := filepath.Join(server.ReceiptDir, unsafeID+".json")
	if err := os.Symlink(path, unsafePath); err != nil {
		t.Fatal(err)
	}
	request.RequestID = unsafeID
	if got := server.invoke(request); got.OK || !strings.Contains(got.Error, "unsafe") {
		t.Fatalf("unsafe receipt accepted: %+v", got)
	}
}

func TestValidateArgvRejectsOversizedAndNULArguments(t *testing.T) {
	if err := validateArgv([]string{"agent", strings.Repeat("x", 256*1024+1)}); err == nil {
		t.Fatal("oversized argument accepted")
	}
	if err := validateArgv([]string{"agent", "bad\x00arg"}); err == nil {
		t.Fatal("NUL argument accepted")
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
