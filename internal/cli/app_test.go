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
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

type projectedPaneViz struct {
	ports.Viz
	panes []ports.ProjectedSession
	err   error
}

func (v projectedPaneViz) ProjectionSessions(context.Context) ([]ports.ProjectedSession, error) {
	return v.panes, v.err
}

func TestProjectionOnlyCLIUsesSnapshotOnlyForSessionList(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, ".viz-projection-only"), []byte("projection only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.Viz = projectedPaneViz{panes: []ports.ProjectedSession{{SessionID: "sess-live", TmuxName: "apex", Surface: "surface:1"}}}
	out := captureStdout(t, func() {
		if code := a.Run([]string{"--json", "session", "list"}); code != 0 {
			t.Fatalf("session list code=%d", code)
		}
	})
	if !strings.Contains(out, `"sess-live"`) {
		t.Fatalf("session projection missing from %q", out)
	}
	for _, args := range [][]string{{"--json", "handoff", "list"}, {"--json", "parent", "inbox", "sess-parent"}, {"--json", "history"}} {
		out := captureStdout(t, func() {
			if code := a.Run(args); code == 0 {
				t.Fatalf("%v unexpectedly succeeded", args)
			}
		})
		if !strings.Contains(out, core.ErrProjectionOnlyAuthority.Error()) {
			t.Fatalf("%v did not fail closed: %q", args, out)
		}
	}
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

func TestMain(m *testing.M) {
	// Tests run inside relay-managed tmux panes too. Keep App.Run calls local so
	// test commands cannot leak through the pane's authenticated desktop bridge
	// into the live control plane.
	_ = os.Setenv(bridge.LocalInvokeEnv, "1")
	os.Exit(m.Run())
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

func TestParentCallerScope(t *testing.T) {
	t.Setenv(bridge.SourceSessionEnv, "sess-parent")
	if err := authorizeParentCaller("sess-parent"); err != nil {
		t.Fatalf("own parent rejected: %v", err)
	}
	if err := authorizeParentCaller("sess-other"); err == nil {
		t.Fatal("cross-parent access should be rejected")
	}
	t.Setenv(bridge.SourceSessionEnv, "")
	if err := authorizeParentCaller("sess-local"); err != nil {
		t.Fatalf("local desktop invocation rejected: %v", err)
	}
}

func TestCurrentParentIDUsesRelaySessionIdentity(t *testing.T) {
	t.Setenv(bridge.SourceSessionEnv, "")
	t.Setenv("RELAY_SESSION_ID", "sess-apex")
	a := New()
	got, err := a.currentParentID()
	if err != nil || got != "sess-apex" {
		t.Fatalf("parent=%q err=%v", got, err)
	}
}

func TestAuthenticatedManagerCannotBypassHierarchy(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv(bridge.SourceSessionEnv, "sess-manager")
	now := time.Now().UTC()
	a := New()
	for _, sess := range []*core.Session{
		{ID: "sess-manager", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "manager"}, CreatedAt: now},
		{ID: "sess-root", HostID: core.LocalHostID, Persist: ports.PersistHandle{Kind: core.LocalPersistKind, Name: "root"}, Labels: map[string]string{"role": core.ParentRole}, CreatedAt: now},
	} {
		if err := a.Reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	opts := core.HandoffOpts{SourceSessionID: "sess-root", Workspace: "workspace:1", Pane: "surface:1"}
	if _, err := a.applyHandoffSource(context.Background(), &opts); err == nil || !bytes.Contains([]byte(err.Error()), []byte("bypasses authenticated manager")) {
		t.Fatalf("hierarchy bypass error = %v", err)
	}

	opts.SourceSessionID = "sess-manager"
	if _, err := a.applyHandoffSource(context.Background(), &opts); err != nil {
		t.Fatalf("direct child rejected: %v", err)
	}
}

func TestParentSendRefusesSilentZeroEventChannel(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv(bridge.SourceSessionEnv, "sess-manager")
	now := time.Now().UTC()
	a := New()
	for _, sess := range []*core.Session{
		{ID: "sess-manager", HostID: core.LocalHostID, Persist: ports.PersistHandle{Kind: core.LocalPersistKind, Name: "manager"}, Labels: map[string]string{"role": core.ParentRole}, CreatedAt: now},
		{ID: "sess-child", HostID: "c3", SourceSessionID: "sess-manager", Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"}, Labels: map[string]string{"governed": "true"}, CreatedAt: now},
	} {
		if err := a.Reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdout(t, func() {
		if code := a.Run([]string{"--json", "parent", "send", "sess-child", "--", "continue"}); code == 0 {
			t.Fatal("zero-event send unexpectedly succeeded")
		}
	})
	if !strings.Contains(out, "no observable handoff event channel") || !strings.Contains(out, "--delivery-only") {
		t.Fatalf("missing fail-loud route: %q", out)
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

func TestAgentProtocolIsCompactAndSelfDescribing(t *testing.T) {
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"agent"}); code != 0 {
			t.Fatalf("agent protocol exit=%d", code)
		}
	})
	if bytes.Count([]byte(out), []byte("\n")) != 1 || len(out) > 600 {
		t.Fatalf("protocol is not compact: %d bytes: %q", len(out), out)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["purpose"] != "long-lived goal handoff and orchestration" {
		t.Fatalf("unexpected protocol: %v", resp)
	}
}

func TestAgentStatusRejectsRemovedHandoffFlag(t *testing.T) {
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"agent", "status", "--handoff", "ho-1"}); code == 0 {
			t.Fatal("removed --handoff syntax was accepted")
		}
	})
	if !bytes.Contains([]byte(out), []byte("agent status HANDOFF")) {
		t.Fatalf("missing positional usage: %q", out)
	}
}

func TestAgentStartRejectsRemovedAgentAndGoalFlags(t *testing.T) {
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"agent", "start", "c1", "--agent", "codex", "--goal", "x"}); code == 0 {
			t.Fatal("removed --agent/--goal syntax was accepted")
		}
	})
	if !bytes.Contains([]byte(out), []byte("unknown flag")) {
		t.Fatalf("missing removed-flag error: %q", out)
	}
}

func TestAgentRestartRejectsUnknownFlagsBeforeLaunch(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	a := New()
	if err := a.Reg.PutHandoff(&core.Handoff{ID: "ho-old", HostID: "c3", Kind: core.KindAgent, Status: core.StatusDone, Outcome: "done", Goal: "goal", Agent: "codex", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if code := a.Run([]string{"agent", "restart", "ho-old", "--bogus"}); code == 0 {
			t.Fatal("unknown restart flag accepted")
		}
	})
	if !bytes.Contains([]byte(out), []byte("unknown flag")) {
		t.Fatalf("restart error=%q", out)
	}
}

func TestParentMessageArgsArePositional(t *testing.T) {
	id, text := parentMessageArgs([]string{"pm-1", "--", "approve", "once"})
	if id != "pm-1" || text != "approve once" {
		t.Fatalf("got id=%q text=%q", id, text)
	}
	if id, _ := parentMessageArgs([]string{"--message", "pm-1"}); id != "" {
		t.Fatalf("removed --message syntax was accepted: %q", id)
	}
}

func TestParentLogReturnsCursorDelta(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	msg := &core.ParentMessage{
		ID: "pm-1", CorrelationID: "corr-1", ParentSessionID: "sess-parent",
		ChildSessionID: "sess-child", HandoffID: "ho-1", Kind: "result", Text: "checkpoint complete",
	}
	if err := core.AppendCommunication(msg, "request", ""); err != nil {
		t.Fatal(err)
	}
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"parent", "log", "sess-parent", "--after", "0", "--limit", "1"}); code != 0 {
			t.Fatalf("parent log exit=%d", code)
		}
	})
	for _, want := range []string{`"next_after":1`, `"summary":"checkpoint complete"`, `"message_id":"pm-1"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("parent log missing %s: %s", want, out)
		}
	}
}

func TestCommunicationLogInfersAuthenticatedManager(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv(bridge.SourceSessionEnv, "sess-parent")
	msg := &core.ParentMessage{
		ID: "pm-1", CorrelationID: "pm-1", ParentSessionID: "sess-parent",
		ChildSessionID: "sess-child", HandoffID: "ho-1", Kind: "result", Text: "checkpoint complete",
	}
	if err := core.AppendCommunication(msg, "event", ""); err != nil {
		t.Fatal(err)
	}
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"log", "0"}); code != 0 {
			t.Fatalf("log exit=%d", code)
		}
	})
	for _, want := range []string{`"next":1`, `"action":"event"`, `"summary":"checkpoint complete"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("compact log missing %s: %s", want, out)
		}
	}
	for _, redundant := range []string{"parent_session_id", "correlation_id"} {
		if strings.Contains(out, redundant) {
			t.Fatalf("compact log leaked %s: %s", redundant, out)
		}
	}
}

func TestPolicyCLIAddCheckRemove(t *testing.T) {
	t.Setenv("RELAY_CONFIG_DIR", t.TempDir())
	a := New()
	add := captureStdout(t, func() {
		if code := a.Run([]string{"policy", "add", "cursor-read", "--kind", "ask", "--agent", "cursor-agent", "--contains", "Run this command?", "--contains", "git status", "--reply", "y"}); code != 0 {
			t.Fatalf("add exit=%d", code)
		}
	})
	if !bytes.Contains([]byte(add), []byte(`"ok":true`)) {
		t.Fatalf("add=%q", add)
	}
	check := captureStdout(t, func() {
		if code := a.Run([]string{"policy", "check", "--kind", "ask", "--source", "idle", "--agent", "cursor-agent", "--text", "Run this command?", "--command", "git status"}); code != 0 {
			t.Fatalf("check exit=%d", code)
		}
	})
	if !bytes.Contains([]byte(check), []byte(`"rule_id":"cursor-read"`)) {
		t.Fatalf("check=%q", check)
	}
	remove := captureStdout(t, func() {
		if code := a.Run([]string{"policy", "remove", "cursor-read"}); code != 0 {
			t.Fatalf("remove exit=%d", code)
		}
	})
	if !bytes.Contains([]byte(remove), []byte(`"removed":"cursor-read"`)) {
		t.Fatalf("remove=%q", remove)
	}
}

func TestPolicyCLIRejectsUnguardedReply(t *testing.T) {
	t.Setenv("RELAY_CONFIG_DIR", t.TempDir())
	a := New()
	out := captureStdout(t, func() {
		if code := a.Run([]string{"policy", "add", "unsafe", "--kind", "permission_required", "--reply", "y"}); code == 0 {
			t.Fatal("unguarded reply policy accepted")
		}
	})
	if !bytes.Contains([]byte(out), []byte("permission decisions require explicit relay resolve")) {
		t.Fatalf("unexpected error=%q", out)
	}
}

func TestCompactHookFieldBoundsProviderPayloads(t *testing.T) {
	got := compactHookField("  Run\n this\tcommand?  ", 12)
	if got != "Run this co…" {
		t.Fatalf("compact field=%q", got)
	}
}

func TestBoardCallerRefusesToActAsAnotherSession(t *testing.T) {
	t.Setenv(bridge.SourceSessionEnv, "sess-me")
	// A bridge-authenticated agent always acts as itself.
	got, err := boardCaller("")
	if err != nil || got != "sess-me" {
		t.Fatalf("want the authenticated session, got %q (%v)", got, err)
	}
	// Naming itself explicitly is fine.
	if got, err := boardCaller("sess-me"); err != nil || got != "sess-me" {
		t.Fatalf("self-reference must be allowed, got %q (%v)", got, err)
	}
	// Naming a peer must be refused.
	if _, err := boardCaller("sess-someone-else"); err == nil {
		t.Fatal("an agent must not be able to act as another session")
	}
}

func TestBoardCallerRequiresSessionOutsideBridge(t *testing.T) {
	t.Setenv(bridge.SourceSessionEnv, "")
	if _, err := boardCaller(""); err == nil {
		t.Fatal("outside a relay pane the caller must be named explicitly")
	}
	if got, err := boardCaller("sess-local"); err != nil || got != "sess-local" {
		t.Fatalf("local operator use must work, got %q (%v)", got, err)
	}
}
