package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

type projectedPaneViz struct {
	ports.Viz
	panes []ports.ProjectedSession
	err   error
}

type localFocusViz struct {
	projectedPaneViz
	focused string
}

func (v *localFocusViz) Available(context.Context) bool { return true }

func (v *localFocusViz) Focus(_ context.Context, sessionID string) error {
	v.focused = sessionID
	return nil
}

type authorityForwardViz struct{ ports.Viz }

func (authorityForwardViz) ForwardAuthorityCommand(context.Context, []string) (int, string, string, error) {
	return 0, "forwarded-from-home\n", "", nil
}

type projectionDoctorViz struct{ projectedPaneViz }

func (projectionDoctorViz) Available(context.Context) bool { return true }

func (projectionDoctorViz) ForwardAuthorityCommand(_ context.Context, args []string) (int, string, string, error) {
	if len(args) == 2 && args[0] == "service" && args[1] == "status" {
		raw, _ := json.Marshal(map[string]any{
			"build": coord.Build, "pid": 42, "ready": true, "live": true,
			"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
			"components": map[string]any{
				"command_boundary": map[string]any{"build": coord.Build, "ready": true, "live": true, "durable_effects": true},
			},
		})
		wrapped, _ := json.Marshal(map[string]any{"ok": true, "health": json.RawMessage(raw)})
		return 0, string(wrapped), "", nil
	}
	return 0, `{"ok":true}`, "", nil
}

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "relay-cli-tests-")
	if err != nil {
		panic(err)
	}
	state, config := filepath.Join(root, "state"), filepath.Join(root, "config")
	if err := os.MkdirAll(config, 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(config, "host.yaml"), []byte("version: 1\nhost_id: cli-test\n"), 0o600); err != nil {
		panic(err)
	}
	_ = os.Setenv("RELAY_STATE_DIR", state)
	_ = os.Setenv("RELAY_CONFIG_DIR", config)
	for _, name := range []string{bridge.SocketEnv, bridge.LocalInvokeEnv, bridge.SourceSessionEnv, bridge.SourceHostEnv, bridge.SourcePersistEnv, bridge.SourceTokenEnv, "RELAY_SESSION_ID", "RELAY_SESSION_HOST", "RELAY_SESSION_NAME"} {
		_ = os.Unsetenv(name)
	}
	// Individual forwarding tests temporarily clear this with t.Setenv.
	_ = os.Setenv(bridge.LocalInvokeEnv, "1")
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func (v projectedPaneViz) ProjectionSessions(context.Context) ([]ports.ProjectedSession, error) {
	return v.panes, v.err
}

func TestStaleQueuedPresentationsDistinguishesPendingFromInert(t *testing.T) {
	now := time.Now().UTC()
	sessions := []*core.Session{
		{ID: "sess-stale", VizSurfaceRef: "viz:queued:39", UpdatedAt: now.Add(-10 * time.Minute)},
		{ID: "sess-recent", VizSurfaceRef: "viz:queued:40", UpdatedAt: now.Add(-time.Minute)},
		{ID: "sess-done", VizSurfaceRef: "surface:289", UpdatedAt: now.Add(-time.Hour)},
	}
	got := staleQueuedPresentations(sessions, now, 5*time.Minute)
	if len(got) != 1 || !strings.Contains(got[0], "sess-stale") || !strings.Contains(got[0], "viz:queued:39") {
		t.Fatalf("stale presentations=%v", got)
	}
}

func TestProjectionSessionListDoesNotCollapseInventoryFailureToEmpty(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.Viz = projectedPaneViz{err: errors.New("cmux unavailable")}
	out := captureStdout(t, func() {
		if code := a.Run([]string{"--json", "session", "list"}); code == 0 {
			t.Fatal("session list unexpectedly succeeded")
		}
	})
	if !strings.Contains(out, "cmux unavailable") {
		t.Fatalf("inventory error lost: %q", out)
	}
}

func TestVizHelpDoesNotExecuteUpdate(t *testing.T) {
	a := New()
	a.Viz = projectedPaneViz{}
	out := captureStdout(t, func() {
		if code := a.Run([]string{"viz", "update", "--help"}); code != 0 {
			t.Fatalf("help code=%d", code)
		}
	})
	if !strings.Contains(out, "usage: relay viz") {
		t.Fatalf("help output=%q", out)
	}
}

func TestProjectedSessionListUsesLiveVizBindings(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.Viz = projectedPaneViz{panes: []ports.ProjectedSession{
		{SessionID: "sess-engram", ParentSessionID: "sess-apex", TmuxName: "engram", Target: "c3", Surface: "surface:2"},
	}}
	list, err := a.Sessions.List()
	if !errors.Is(err, core.ErrProjectionOnlyAuthority) || list != nil {
		t.Fatalf("authority read did not fail closed: list=%v err=%v", list, err)
	}
	projected, err := a.projectedSessions(context.Background())
	if err != nil || len(projected) != 1 {
		t.Fatalf("projection=%v err=%v", projected, err)
	}
	got := projected[0]
	if got.ID != "sess-engram" || got.SourceSessionID != "sess-apex" || got.HostID != "c3" || got.Persist.Name != "engram" || got.VizSurfaceRef != "surface:2" {
		t.Fatalf("projection fields lost: %+v", got)
	}
}

func TestProjectionOnlyVizFocusUsesProjectedIdentity(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	viz := &localFocusViz{projectedPaneViz: projectedPaneViz{panes: []ports.ProjectedSession{
		{SessionID: "sess-hamburg", Target: "hamburg", TmuxName: "beholder", Surface: "surface:1"},
	}}}
	a := New()
	a.Viz = viz
	if code := a.Run([]string{"viz", "focus", "hamburg"}); code != 0 {
		t.Fatalf("projection-only viz focus code=%d", code)
	}
	if viz.focused != "sess-hamburg" {
		t.Fatalf("focused session=%q, want sess-hamburg", viz.focused)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

func TestSourceEnvironmentUsesAuthenticatedRegistryIdentity(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv(bridge.SourceSessionEnv, "sess-real")
	t.Setenv(bridge.SourceHostEnv, "spoofed-host")
	t.Setenv(bridge.SourcePersistEnv, "spoofed-name")
	now := time.Now().UTC()
	reg := &core.Registry{}
	if err := reg.PutSession(&core.Session{
		ID: "sess-real", HostID: "c3", RepoRef: "/local/repo",
		Persist: ports.PersistHandle{Name: "research"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	id, host, persist, repo := sourceFromEnvironment(reg)
	if id != "sess-real" || host != "c3" || persist != "research" || repo != "/local/repo" {
		t.Fatalf("unexpected source: %q %q %q %q", id, host, persist, repo)
	}
}

func TestLocalCLIForwardsAuthenticatedRequestAndConfirmsResponse(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "relay-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("RELAY_STATE_DIR", root)
	t.Setenv(bridge.SocketEnv, "")
	t.Setenv(bridge.LocalInvokeEnv, "")
	t.Setenv("RELAY_SESSION_ID", "")
	t.Setenv(bridge.SourceTokenEnv, "")
	if _, err := core.EnsureHomeClientIdentity(); err != nil {
		t.Fatal(err)
	}
	relayBin := filepath.Join(root, "relay-effect")
	if err := os.WriteFile(relayBin, []byte("#!/bin/sh\nprintf 'forwarded:%s\\n' \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := &bridge.Server{
		SockPath: core.DesktopBridgeSocketPath(), RelayBin: relayBin, Build: coord.Build,
		Authorize:  core.AuthorizeBridgeSource,
		ReceiptDir: core.CommandReceiptDir(),
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := bridge.Client{SockPath: core.DesktopBridgeSocketPath()}
	for client.Ping(ctx) != nil {
		select {
		case serveErr := <-done:
			t.Fatalf("bridge server: %v", serveErr)
		default:
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	var code int
	out := captureStdout(t, func() { code = New().Run([]string{"version"}) })
	if code != 0 || strings.TrimSpace(out) != "forwarded:version" {
		t.Fatalf("code=%d output=%q", code, out)
	}
	entries, err := os.ReadDir(core.CommandReceiptDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("command receipts=%d err=%v", len(entries), err)
	}
}

func TestProjectionClientDoctorChecksHomeWithoutLocalAuthority(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.Viz = projectionDoctorViz{}
	a.CompactJSON = true
	out := captureStdout(t, func() {
		if code := a.Run([]string{"doctor"}); code != 0 {
			t.Fatalf("doctor code=%d", code)
		}
	})
	var result struct {
		Failed int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Failed != 0 || !strings.Contains(out, `"authority_command"`) {
		t.Fatalf("projection doctor output=%q", out)
	}
}

func TestLegacyAuthorityProcessDetectionIgnoresDiagnosticCommands(t *testing.T) {
	tests := []struct {
		command string
		argv    []string
		want    bool
	}{
		{"relayd", []string{"/home/me/.local/bin/relayd", "serve"}, true},
		{"relayd", []string{"relayd", "control", "serve"}, true},
		{"relay", []string{"relay", "supervise"}, true},
		{"pgrep", []string{"pgrep", "-af", "relayd serve|relay supervise"}, false},
		{"relay", []string{"relay", "doctor", "-H", "c1"}, false},
	}
	for _, test := range tests {
		if got := isLegacyAuthorityProcess(test.command, test.argv); got != test.want {
			t.Fatalf("command=%s argv=%v got=%v want=%v", test.command, test.argv, got, test.want)
		}
	}
}

func TestUnknownFlagRejected(t *testing.T) {
	a := New()
	if code := a.Run([]string{"session", "create", "--bogus", "x"}); code == 0 {
		t.Fatal("expected non-zero")
	}
}

func TestJSONErrorShape(t *testing.T) {
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"--json", "session", "create", "--bogus"}); code == 0 {
			t.Fatal("expected non-zero")
		}
	})
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("json: %v out=%q", err, out)
	}
	if resp["ok"] != false {
		t.Fatalf("ok=%v", resp["ok"])
	}
	errStr, _ := resp["error"].(string)
	if errStr == "" || !bytes.Contains([]byte(errStr), []byte("unknown flag")) {
		t.Fatalf("error=%q", errStr)
	}
}

func TestRetiredDelegationVerbsAreUnknownCommands(t *testing.T) {
	// The delegation handshake retired as one unit (workspace decision
	// 2026-09-13); none of its verbs may resolve to anything, and none may be
	// mistaken for `relay HOST NAME` (which needs exactly two words).
	for _, argv := range [][]string{{"agent", "protocol"}, {"parent", "list"}, {"handoff", "list"}, {"msg", "read", "x"}, {"resolve", "pm-1", "--", "yes"}, {"ask", "q"}, {"board", "query"}, {"root", "status"}, {"policy", "list"}, {"gc"}, {"events", "tail"}, {"history"}, {"supervise"}, {"log", "0"}} {
		a := New()
		a.JSON = true
		out := captureStdout(t, func() {
			if code := a.Run(argv); code == 0 {
				t.Fatalf("%v should not be a command any more", argv)
			}
		})
		if !strings.Contains(out, "unknown command") {
			t.Fatalf("%v: expected an unknown-command error, got %q", argv, out)
		}
	}
}
