package cmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

func TestVizServiceUsesRelayManagedReconnect(t *testing.T) {
	v := &Viz{Targets: map[string]targetConfig{
		"home-relay": {Host: "100.108.118.32", User: "dostos", Port: 2222, Identity: "~/.ssh/viz"},
	}}
	command, err := v.attachCommand(context.Background(), ports.Presentation{SessionID: "sess-1", Target: "home-relay", TmuxName: "beholder-pdf-main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"relay", `resume`, `--session`, `beholder-pdf-main`, `--host`, `100.108.118.32`, `--user`, `dostos`, `--port`, `2222`, `--identity`, `~/.ssh/viz`} {
		if !strings.Contains(command, want) {
			t.Fatalf("attach command %q missing %q", command, want)
		}
	}
	if strings.Contains(command, "tmux attach") || strings.HasPrefix(command, "ssh ") {
		t.Fatalf("viz bypassed Relay persistence: %q", command)
	}
}

func TestAuthorityProjectionPreservesClientRoutedAlias(t *testing.T) {
	v := &Viz{}
	got, err := v.withAuthorityTarget(ports.Presentation{SessionID: "sess-1", Target: "hamburg", TmuxName: "worker"})
	if err != nil || got.SSHHost != "hamburg" || got.SSHUser != "" || got.SSHPort != 0 {
		t.Fatalf("projection target=%+v err=%v", got, err)
	}
}

func TestControlSSHArgsPinsDedicatedNonMultiplexedIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := &Viz{Control: &targetConfig{Host: "home.example", User: "viz", Port: 2222, Identity: "~/.ssh/viz"}}
	args, err := v.controlSSHArgs("viz-snapshot relay-viz-mac")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"IdentitiesOnly=yes", "ControlMaster=no", "ControlPath=none", "-i " + filepath.Join(os.Getenv("HOME"), ".ssh/viz")} {
		if !strings.Contains(joined, want) {
			t.Fatalf("control SSH args %q missing %q", joined, want)
		}
	}
	if _, err := (&Viz{Control: &targetConfig{Host: "home.example"}}).controlSSHArgs("viz-snapshot relay-viz-mac"); err == nil {
		t.Fatal("control connection without dedicated identity was accepted")
	}
}

func TestProjectedResumeUsesAuthoritySnapshotAndLocalTargetPolicy(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	t.Setenv("HOME", t.TempDir())
	v := &Viz{Targets: map[string]targetConfig{"home-relay": {Host: "home.example", User: "dostos", Port: 2222, Identity: "~/.ssh/viz"}}}
	raw, _ := json.Marshal([]ports.Presentation{{SessionID: "sess-apex", Target: "home-relay", TmuxName: "apex-v4"}})
	if err := saveBytes(v.authoritySnapshotPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := v.resolveOfflineSnapshot("apex-v4")
	if err != nil {
		t.Fatal(err)
	}
	target, err := v.localResumeTarget(item.Target)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "home.example" || target.User != "dostos" || target.Port != 2222 || target.Identity != filepath.Join(os.Getenv("HOME"), ".ssh/viz") {
		t.Fatalf("resume target=%+v", target)
	}
	if _, err := v.resolveOfflineSnapshot("missing"); err == nil {
		t.Fatal("missing authoritative session was accepted")
	}
}

func TestProjectedResumePrefersLiveAuthorityAndRequiresExplicitOffline(t *testing.T) {
	state := t.TempDir()
	home := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ssh := filepath.Join(binDir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' '{\"session_id\":\"sess-live\",\"target\":\"home-relay\",\"tmux_name\":\"apex-v4\",\"ssh_host\":\"home.example\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{
		ServiceID: "mac",
		Control:   &targetConfig{Host: "control.example", Identity: "~/.ssh/control"},
		Targets:   map[string]targetConfig{"home-relay": {Host: "home.example", Identity: "~/.ssh/attach"}},
	}
	target, err := v.ResolveProjectedResume(context.Background(), "apex-v4", ports.ResumeResolveOpts{})
	if err != nil || target.Host != "home.example" || target.Identity != filepath.Join(home, ".ssh/attach") {
		t.Fatalf("live target=%+v err=%v", target, err)
	}
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' '{\"session_id\":\"sess-live\",\"target\":\"home-relay\",\"tmux_name\":\"apex-v4\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ResolveProjectedResume(context.Background(), "apex-v4", ports.ResumeResolveOpts{}); err == nil {
		t.Fatal("live authority response without an SSH endpoint used local routing")
	}
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]ports.Presentation{{SessionID: "sess-offline", Target: "home-relay", TmuxName: "apex-v4"}})
	if err := saveBytes(v.authoritySnapshotPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ResolveProjectedResume(context.Background(), "apex-v4", ports.ResumeResolveOpts{}); err == nil {
		t.Fatal("authority failure silently used offline state")
	}
	if _, err := v.ResolveProjectedResume(context.Background(), "apex-v4", ports.ResumeResolveOpts{AllowOffline: true}); err != nil {
		t.Fatalf("explicit offline fallback failed: %v", err)
	}
}

func TestCoveredProjectionSkipsRetiredHistoricalSession(t *testing.T) {
	v := &Viz{}
	event := coord.Event{Seq: 50, Kind: "project", Meta: map[string]any{
		"op": "upsert", "session_id": "sess-retired", "target": "hamburg", "tmux_name": "old",
	}}
	result, handled, receipt, err := v.handleCoveredProjection(context.Background(), event, ports.AuthoritySnapshot{V: 1, Revision: 52})
	if err != nil || !handled || receipt || result != "" {
		t.Fatalf("retired replay result=%q handled=%v receipt=%v err=%v", result, handled, receipt, err)
	}
	_, handled, _, err = v.handleCoveredProjection(context.Background(), coord.Event{Seq: 51, Kind: "update_relayd"}, ports.AuthoritySnapshot{V: 1, Revision: 52})
	if err != nil || handled {
		t.Fatalf("lifecycle event was swallowed by snapshot watermark: handled=%v err=%v", handled, err)
	}
}

func TestCoveredProjectionSkipsMalformedHistoricalDelete(t *testing.T) {
	v := &Viz{}
	event := coord.Event{Seq: 50, Kind: "project", Meta: map[string]any{"op": "delete", "session_id": ""}}
	result, handled, receipt, err := v.handleCoveredProjection(context.Background(), event, ports.AuthoritySnapshot{V: 1, Revision: 52})
	if err != nil || !handled || receipt || result != "" {
		t.Fatalf("malformed covered delete result=%q handled=%v receipt=%v err=%v", result, handled, receipt, err)
	}
}

func TestCoveredHistoricalDeleteConvergesToCurrentSnapshot(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	bin := filepath.Join(t.TempDir(), "cmux")
	tree := `{"windows":[{"workspaces":[{"ref":"workspace:1","panes":[{"ref":"pane:1","surfaces":[{"ref":"surface:1"}]}]}]}]}`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+tree+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{Bin: bin}
	if err := v.persistBinding("sess-current", binding{V: 2, Revision: 52, SessionID: "sess-current", Surface: "surface:1"}); err != nil {
		t.Fatal(err)
	}
	event := coord.Event{Seq: 50, Kind: "project", Meta: map[string]any{"op": "delete", "session_id": "sess-current"}}
	snapshot := ports.AuthoritySnapshot{V: 1, Revision: 52, Items: []ports.Presentation{{
		SessionID: "sess-current", Target: "hamburg", TmuxName: "current", SSHHost: "host.example",
	}}}
	_, handled, receipt, err := v.handleCoveredProjection(context.Background(), event, snapshot)
	if err != nil || !handled || receipt {
		t.Fatalf("covered delete handled=%v receipt=%v err=%v", handled, receipt, err)
	}
	b, err := v.loadBinding("sess-current")
	if err != nil || b.Deleted {
		t.Fatalf("historical delete retired current binding: %+v err=%v", b, err)
	}
}

func TestSnapshotReconcileRecoversLostAckWithoutDuplicatePane(t *testing.T) {
	state := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmuxLog := filepath.Join(t.TempDir(), "cmux.log")
	sshLog := filepath.Join(t.TempDir(), "ssh.log")
	tree := `{"windows":[{"workspaces":[{"ref":"workspace:38","panes":[{"ref":"pane:219","surfaces":[{"ref":"surface:289"}]}]}]}]}`
	cmuxScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + cmuxLog + "'\nif [ \"$1 $2\" = 'tree --all' ]; then printf '%s\\n' '" + tree + "'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "cmux"), []byte(cmuxScript), 0o700); err != nil {
		t.Fatal(err)
	}
	sshScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + sshLog + "'\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(sshScript), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{Bin: filepath.Join(binDir, "cmux"), ServiceID: "client", Control: &targetConfig{Host: "authority", Identity: "/tmp/viz-key"}}
	if err := v.persistBinding("sess-apex", binding{V: 2, Revision: 39, SessionID: "sess-apex", Surface: "surface:289", Pane: "pane:219", Workspace: "workspace:38"}); err != nil {
		t.Fatal(err)
	}
	snapshot := ports.AuthoritySnapshot{V: 1, Revision: 50, Items: []ports.Presentation{{
		SessionID: "sess-apex", Target: "home", TmuxName: "apex-v4", ProjectionRevision: 39,
	}}}
	if err := v.reconcileManagedSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cmuxLog)
	if strings.Contains(string(raw), "new-split") || strings.Contains(string(raw), "new-pane") {
		t.Fatalf("lost ack recovery duplicated the live pane:\n%s", raw)
	}
	ack, _ := os.ReadFile(sshLog)
	if !strings.Contains(string(ack), "viz-ack relay-viz-client") {
		t.Fatalf("lost acknowledgement was not recovered:\n%s", ack)
	}
	if !strings.Contains(string(ack), "IdentitiesOnly=yes") || !strings.Contains(string(ack), "ControlPath=none") {
		t.Fatalf("recovery escaped the confined control connection:\n%s", ack)
	}
}

func TestPresentationMetadataKeepsIntegerSSHPort(t *testing.T) {
	got := presentationFromMeta(map[string]any{"ssh_user": "jingyulee", "ssh_port": 7777})
	if got.SSHUser != "jingyulee" || got.SSHPort != 7777 {
		t.Fatalf("presentation metadata lost SSH endpoint: %+v", got)
	}
}

func TestProjectionFocusReplaysAgainstLiveEqualRevision(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	bin, logPath := filepath.Join(dir, "cmux"), filepath.Join(dir, "calls")
	tree := `{"windows":[{"workspaces":[{"ref":"workspace:1","panes":[{"ref":"pane:1","surfaces":[{"ref":"surface:1"}]}]}]}]}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\nif [ \"$1\" = tree ]; then printf '%s\\n' '" + tree + "'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{Bin: bin}
	if err := v.persistBinding("sess-current", binding{V: 2, Revision: 9, SessionID: "sess-current", Surface: "surface:1", Pane: "pane:1"}); err != nil {
		t.Fatal(err)
	}
	_, err := v.ApplyProjection(context.Background(), ports.ProjectionEvent{V: 1, Revision: 9, Op: ports.ProjectionFocus, Item: ports.Presentation{SessionID: "sess-current"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "focus-pane --pane pane:1") {
		t.Fatalf("equal-revision focus was dropped:\n%s", raw)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.persistBinding("sess-current", binding{V: 2, Revision: 10, SessionID: "sess-current", Surface: "surface:1", Pane: "pane:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ApplyProjection(context.Background(), ports.ProjectionEvent{V: 1, Revision: 9, Op: ports.ProjectionFocus, Item: ports.Presentation{SessionID: "sess-current"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(logPath)
	if strings.Contains(string(raw), "focus-pane") || strings.Contains(string(raw), "focus-panel") {
		t.Fatalf("superseded focus stole focus:\n%s", raw)
	}
}

func TestProjectedResumeRejectsMalformedAuthorityResponse(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"session_id\":\"sess-live\",\"target\":\"home-relay\",\"tmux_name\":\"other\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{ServiceID: "mac", Control: &targetConfig{Host: "control.example", Identity: "/tmp/control"}}
	if _, err := v.ResolveProjectedResume(context.Background(), "apex-v4", ports.ResumeResolveOpts{}); err == nil {
		t.Fatal("mismatched authority identity accepted")
	}
}

func TestDuplicateSurfaceHasOneDeterministicLiveOwner(t *testing.T) {
	now := time.Now().UTC()
	old := binding{SessionID: "sess-old", Surface: "surface:1", Revision: 2, UpdatedAt: now}
	newer := binding{SessionID: "sess-new", Surface: "surface:1", Revision: 3, UpdatedAt: now.Add(-time.Hour)}
	if !preferBindingOwner(newer, old) {
		t.Fatal("higher stream revision must own a crash-left duplicate surface")
	}
	if preferBindingOwner(old, newer) {
		t.Fatal("older stream revision displaced current surface owner")
	}
	tieA := binding{SessionID: "sess-a", Surface: "surface:1", Revision: 3, UpdatedAt: now}
	tieB := binding{SessionID: "sess-b", Surface: "surface:1", Revision: 3, UpdatedAt: now}
	if !preferBindingOwner(tieB, tieA) || preferBindingOwner(tieA, tieB) {
		t.Fatal("equal bindings do not have deterministic ownership")
	}
}

func TestManagedPanesKeepsDurableOwnerAcrossLocationRefresh(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	bin := filepath.Join(t.TempDir(), "cmux")
	tree := `{"windows":[{"workspaces":[{"ref":"workspace:new","panes":[{"ref":"pane:new","surfaces":[{"ref":"surface:1"}]}]}]}]}`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+tree+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{Bin: bin}
	now := time.Now().UTC()
	for _, b := range []binding{
		{SessionID: "sess-current", Surface: "surface:1", Workspace: "workspace:old", Pane: "pane:old", UpdatedAt: now},
		{SessionID: "sess-stale", Surface: "surface:1", Workspace: "workspace:older", Pane: "pane:older", UpdatedAt: now.Add(-time.Hour)},
	} {
		if err := v.persistBinding(b.SessionID, b); err != nil {
			t.Fatal(err)
		}
	}
	panes, err := v.ManagedPanes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	live := ""
	for _, pane := range panes {
		if pane.State == "live" {
			if live != "" {
				t.Fatalf("multiple live owners: %+v", panes)
			}
			live = pane.SessionID
		}
	}
	if live != "sess-current" {
		t.Fatalf("durable current owner lost after location refresh: live=%q panes=%+v", live, panes)
	}
}

func TestLiveSurfaceInventoryRejectsValidWrongSchema(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `[]`, `{"error":"unsupported"}`, `{"windows":{}}`} {
		if locations, err := parseLiveSurfaceLocations([]byte(raw)); err == nil {
			t.Fatalf("schema %s returned healthy inventory: %+v", raw, locations)
		}
	}
	locations, err := parseLiveSurfaceLocations([]byte(`{"windows":[]}`))
	if err != nil || len(locations) != 0 {
		t.Fatalf("legitimate empty inventory rejected: %+v err=%v", locations, err)
	}
}

func TestProjectionSessionsUsesAuthoritySnapshotNotBindingLineage(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	bin := filepath.Join(t.TempDir(), "cmux")
	tree := `{"windows":[{"workspaces":[{"ref":"workspace:1","panes":[{"ref":"pane:1","surfaces":[{"ref":"surface:1"}]}]}]}]}`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+tree+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{Bin: bin}
	if err := v.persistBinding("sess-engram", binding{SessionID: "sess-engram", SourceSessionID: "sess-dead", Surface: "surface:1"}); err != nil {
		t.Fatal(err)
	}
	items := []ports.Presentation{
		{SessionID: "sess-engram", ParentSessionID: "sess-apex", Target: "c3", TmuxName: "engram"},
		{SessionID: "sess-apex", Target: "home", TmuxName: "apex-v4"},
	}
	raw, _ := json.Marshal(items)
	if err := saveBytes(v.authoritySnapshotPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	projected, err := v.ProjectionSessions(context.Background())
	if err != nil || len(projected) != 2 {
		t.Fatalf("projection=%+v err=%v", projected, err)
	}
	if got := projected[0]; got.SessionID != "sess-apex" || got.Surface != "" {
		t.Fatalf("authority session without a pane was erased or given a surface: %+v", got)
	}
	got := projected[1]
	if got.ParentSessionID != "sess-apex" || got.Target != "c3" || got.TmuxName != "engram" {
		t.Fatalf("stale binding leaked into projection: %+v", got)
	}
}

func TestProjectionSessionsSurvivesUnavailableVisualization(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := &Viz{Bin: filepath.Join(t.TempDir(), "missing-cmux")}
	items := []ports.Presentation{{SessionID: "sess-apex", Target: "home", TmuxName: "apex-v4"}}
	raw, _ := json.Marshal(items)
	if err := saveBytes(v.authoritySnapshotPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	projected, err := v.ProjectionSessions(context.Background())
	if err != nil || len(projected) != 1 || projected[0].SessionID != "sess-apex" || projected[0].Surface != "" {
		t.Fatalf("authority inventory depended on visualization: projection=%+v err=%v", projected, err)
	}
}

func TestAuthoritySnapshotTracksStreamReparentAndDelete(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := &Viz{}
	initial, _ := json.Marshal([]ports.Presentation{{SessionID: "sess-child", ParentSessionID: "sess-old", Target: "c3", TmuxName: "child"}})
	if err := saveBytes(v.authoritySnapshotPath(), initial, 0o600); err != nil {
		t.Fatal(err)
	}
	updated := ports.Presentation{SessionID: "sess-child", ParentSessionID: "sess-new", Target: "hamburg", TmuxName: "child-v2"}
	if err := v.updateAuthoritySnapshot(ports.ProjectionEvent{V: 1, Revision: 10, Op: ports.ProjectionUpsert, Item: updated}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(v.authoritySnapshotPath())
	snapshot, err := decodeAuthoritySnapshot(raw)
	items := snapshot.Items
	if err != nil || snapshot.Revision != 10 || len(items) != 1 || items[0] != updated {
		t.Fatalf("updated snapshot=%+v err=%v", items, err)
	}
	if err := v.updateAuthoritySnapshot(ports.ProjectionEvent{V: 1, Revision: 11, Op: ports.ProjectionDelete, Item: updated}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(v.authoritySnapshotPath())
	snapshot, err = decodeAuthoritySnapshot(raw)
	items = snapshot.Items
	if err != nil || snapshot.Revision != 11 || len(items) != 0 {
		t.Fatalf("deleted snapshot=%+v err=%v", items, err)
	}
}

func TestAuthoritySnapshotWatermarkRejectsRegressiveReplay(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := &Viz{}
	current, _ := json.Marshal(ports.AuthoritySnapshot{V: 1, Revision: 12, Items: []ports.Presentation{}})
	if err := saveBytes(v.authoritySnapshotPath(), current, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := ports.Presentation{SessionID: "sess-deleted", Target: "c3", TmuxName: "deleted"}
	if err := v.updateAuthoritySnapshot(ports.ProjectionEvent{V: 1, Revision: 11, Op: ports.ProjectionUpsert, Item: stale}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(v.authoritySnapshotPath())
	snapshot, err := decodeAuthoritySnapshot(raw)
	if err != nil || snapshot.Revision != 12 || len(snapshot.Items) != 0 {
		t.Fatalf("regressive replay changed snapshot: %+v err=%v", snapshot, err)
	}
}

func TestPresentTargetCarriesParentAnchorIntoLocalPolicy(t *testing.T) {
	req := ports.Presentation{SessionID: "sess-child", ParentSessionID: "sess-parent", Target: "home", TmuxName: "child"}
	if req.ParentSessionID != "sess-parent" {
		t.Fatalf("parent anchor lost: %+v", req)
	}
	layout := ports.Layout{Mode: "remote", SourceSessionID: req.ParentSessionID}
	got := childLayout(parentChildLayout(layout, binding{Workspace: "workspace:beholder", Pane: "pane:parent"}), binding{})
	if got.Workspace != "workspace:beholder" || got.Pane != "pane:parent" || got.SplitDirection != "right" {
		t.Fatalf("anchored layout = %+v", got)
	}
}

func TestPresentationRejectsShellTargets(t *testing.T) {
	err := validatePresentation(ports.Presentation{SessionID: "sess-1", Target: "home; touch /tmp/no", TmuxName: "safe"})
	if err == nil {
		t.Fatal("shell syntax in a visualization target must be rejected")
	}
}

func TestAttachCommandRejectsUnknownTargetPolicy(t *testing.T) {
	v := &Viz{Targets: map[string]targetConfig{"home": {Host: "home.example"}}}
	if _, err := v.attachCommand(context.Background(), ports.Presentation{SessionID: "sess-1", Target: "raw-host", TmuxName: "safe"}); err == nil {
		t.Fatal("unknown policy key must not become a raw ssh destination")
	}
}

func TestAttachCommandAcceptsAuthorityResolvedTarget(t *testing.T) {
	v := &Viz{}
	command, err := v.attachCommand(context.Background(), ports.Presentation{
		SessionID: "sess-1", Target: "hamburg", TmuxName: "safe",
		SSHHost: "host.example", SSHUser: "worker", SSHPort: 2222,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--host 'host.example'", "--user 'worker'", "--port 2222"} {
		if !strings.Contains(command, want) {
			t.Fatalf("authority target command %q missing %q", command, want)
		}
	}
}

func TestNewLoadsRemoteVizConfig(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", configDir)
	t.Setenv("RELAY_CMUX_BIN", "")
	raw, _ := json.Marshal(config{ServiceID: "mac", Control: &targetConfig{Host: "home", Port: 2222}})
	if err := os.WriteFile(filepath.Join(configDir, "viz.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	v := New()
	if v.ServiceID != "mac" || v.Control == nil || v.Control.Host != "home" || v.Control.Port != 2222 {
		t.Fatalf("viz config = %+v", v)
	}
}

func TestProjectionClientForwardsAuthorityCommandThroughSSHConfigAlias(t *testing.T) {
	binDir := t.TempDir()
	ssh := filepath.Join(binDir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\nprintf 'remote-error\\n' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	v := &Viz{Command: &targetConfig{Host: "home-relay"}}
	code, stdout, stderr, err := v.ForwardAuthorityCommand(context.Background(), []string{"handoff", "show", "goal with spaces"})
	if err != nil || code != 7 || !strings.Contains(stdout, "home-relay") || !strings.Contains(stdout, "goal with spaces") || stderr != "remote-error\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q err=%v", code, stdout, stderr, err)
	}
}

func TestProjectionHealthRequiresOwnedFollowerAndCaughtUpCursor(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := &Viz{ServiceID: "mac", Control: &targetConfig{Host: "home"}}
	if err := saveBytes(v.authoritySnapshotPath(), []byte(`{"v":1,"revision":9,"items":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveSequence(v.cursorPath(), 9); err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.ProjectionHealth(); ok {
		t.Fatal("unowned follower lock reported healthy")
	}
	lock, err := os.OpenFile(v.cursorPath()+".follow.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	if ok, detail := v.ProjectionHealth(); !ok || !strings.Contains(detail, "cursor=9") {
		t.Fatalf("projection health ok=%v detail=%q", ok, detail)
	}
}

func TestSurfaceCommandPinsTextAndEnterToWorkspace(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "text",
			got:  surfaceCommand("send", "surface:62", "workspace:9", "--", "decision needed"),
			want: []string{"send", "--surface", "surface:62", "--workspace", "workspace:9", "--", "decision needed"},
		},
		{
			name: "enter",
			got:  surfaceCommand("send-key", "surface:62", "workspace:9", "ENTER"),
			want: []string{"send-key", "--surface", "surface:62", "--workspace", "workspace:9", "ENTER"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.got, test.want) {
				t.Fatalf("surfaceCommand() = %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestParseWorkspaceRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"list-panes shape", `{"workspace_ref":"workspace:6","panes":[]}`, "workspace:6"},
		{"missing field", `{"panes":[]}`, ""},
		{"empty", ``, ""},
		{"garbage", `not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseWorkspaceRef([]byte(c.in)); got != c.want {
				t.Fatalf("parseWorkspaceRef(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestExtractSessionFlagStripsShellQuotes(t *testing.T) {
	cases := map[string]string{
		`relay resume --session demo`:                       "demo",
		`'/opt/relay resume --session demo'`:                "demo",
		`''\''/opt/relay resume --session nested-demo'\'''`: "nested-demo",
	}
	for command, want := range cases {
		if got := extractSessionFlag(command); got != want {
			t.Fatalf("extractSessionFlag(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestActiveWorkspacePrefersCallingCmuxWorkspace(t *testing.T) {
	t.Setenv("CMUX_WORKSPACE_ID", "workspace:beholder-pdf")

	v := New()
	if got := v.activeWorkspace(context.Background()); got != "workspace:beholder-pdf" {
		t.Fatalf("activeWorkspace() = %q, want caller workspace", got)
	}
}

func TestParseSurfaceLocation(t *testing.T) {
	raw := []byte(`{"windows":[{"workspaces":[{"ref":"workspace:7","panes":[{"ref":"pane:9","surfaces":[{"ref":"surface:11"}]}]}]}]}`)
	got := parseSurfaceLocation(raw, "surface:11")
	if got.Workspace != "workspace:7" || got.Pane != "pane:9" {
		t.Fatalf("location = %+v", got)
	}
	if missing := parseSurfaceLocation(raw, "surface:404"); missing != (surfaceLocation{}) {
		t.Fatalf("missing location = %+v", missing)
	}
}

func TestChildLayoutBuildsRightHandStack(t *testing.T) {
	parent := ports.Layout{
		Workspace: "workspace:1", Pane: "pane:parent", SourceSessionID: "sess-parent",
	}
	first := childLayout(parent, binding{})
	if first.Workspace != "workspace:1" || first.Pane != "pane:parent" || first.SplitDirection != "right" {
		t.Fatalf("first child layout = %+v", first)
	}
	second := childLayout(parent, binding{Workspace: "workspace:1", Pane: "pane:first-child"})
	if second.Workspace != "workspace:1" || second.Pane != "pane:first-child" || second.SplitDirection != "down" {
		t.Fatalf("second child layout = %+v", second)
	}
}

func TestFirstChildInheritsPersistedParentLocation(t *testing.T) {
	layout := ports.Layout{Workspace: "workspace:wrong-active", SourceSessionID: "sess-parent"}
	layout = parentChildLayout(layout, binding{Workspace: "workspace:parent", Pane: "pane:parent"})
	layout = childLayout(layout, binding{})
	if layout.Workspace != "workspace:parent" || layout.Pane != "pane:parent" || layout.SplitDirection != "right" {
		t.Fatalf("first child layout = %+v", layout)
	}
}

func TestChildLayoutPreservesExplicitPlacement(t *testing.T) {
	explicit := ports.Layout{
		Workspace: "workspace:custom", Pane: "pane:custom", SourceSessionID: "sess-parent", ExplicitPlace: true,
	}
	got := childLayout(explicit, binding{Workspace: "workspace:1", Pane: "pane:sibling"})
	if got.Workspace != explicit.Workspace || got.Pane != explicit.Pane || got.SplitDirection != "" {
		t.Fatalf("explicit layout changed: %+v", got)
	}
}

func TestBindingRoundTripKeepsParentAndLocation(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := New()
	created := time.Now().UTC().Truncate(time.Second)
	want := binding{
		SessionID: "sess-child", SourceSessionID: "sess-parent",
		Surface: "surface:3", Pane: "pane:2", Workspace: "workspace:1",
		Attach: "relay resume --session demo", Mode: "split", CreatedAt: created,
	}
	if err := v.persistBinding(want.SessionID, want); err != nil {
		t.Fatal(err)
	}
	got, err := v.loadBinding(want.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != 2 || got.SourceSessionID != want.SourceSessionID || got.Pane != want.Pane || got.Workspace != want.Workspace || !got.CreatedAt.Equal(created) {
		t.Fatalf("binding = %+v", got)
	}
}

func TestReparentBindingUpdatesOnlyLineage(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := New()
	want := binding{
		SessionID: "sess-child", SourceSessionID: "sess-wrong",
		Surface: "surface:3", Pane: "pane:2", Workspace: "workspace:1",
		Attach: "relay resume --session demo", Mode: "split",
	}
	if err := v.persistBinding(want.SessionID, want); err != nil {
		t.Fatal(err)
	}
	if err := v.ReparentBinding(want.SessionID, "sess-correct"); err != nil {
		t.Fatal(err)
	}
	got, err := v.loadBinding(want.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceSessionID != "sess-correct" || got.Surface != want.Surface || got.Pane != want.Pane || got.Workspace != want.Workspace {
		t.Fatalf("binding=%+v", got)
	}
}

func TestForgetBindingDoesNotTouchReplacement(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := New()
	for _, id := range []string{"sess-dead", "sess-replacement"} {
		if err := v.persistBinding(id, binding{SessionID: id, Surface: "surface:3", Attach: "relay resume --session engram"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.ForgetBinding("sess-dead"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.loadBinding("sess-dead"); err == nil {
		t.Fatal("dead binding remains")
	}
	if _, err := v.loadBinding("sess-replacement"); err != nil {
		t.Fatalf("replacement binding removed: %v", err)
	}
}

func TestClosePersistRemovesBindingWithoutCmux(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := &Viz{Bin: "/definitely/missing/cmux", bindings: map[string]binding{}}
	if err := v.persistBinding("sess-cleaned", binding{
		SessionID: "sess-cleaned", Surface: "surface:99",
		Attach: "relay resume --session cleaned-demo",
	}); err != nil {
		t.Fatal(err)
	}
	if removed := v.ClosePersist(context.Background(), "cleaned-demo"); removed != 1 {
		t.Fatalf("removed = %d", removed)
	}
	if _, err := os.Stat(bindPath("sess-cleaned")); !os.IsNotExist(err) {
		t.Fatalf("binding still exists: %v", err)
	}
}

func TestCloseServiceEventRemovesLocalBinding(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	v := &Viz{Bin: "/definitely/missing/cmux", ServiceID: "mac", Control: &targetConfig{Host: "home"}, bindings: map[string]binding{}}
	if err := v.persistBinding("sess-old", binding{SessionID: "sess-old", Surface: "surface:99"}); err != nil {
		t.Fatal(err)
	}
	result, err := v.handleServiceEvent(context.Background(), coord.Event{Seq: 1, Kind: "close", Meta: map[string]any{"session_id": "sess-old"}})
	if err != nil || result != "closed" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	tombstone, err := v.loadBinding("sess-old")
	if err != nil || !tombstone.Deleted || tombstone.Revision != 1 || tombstone.Surface != "" {
		t.Fatalf("tombstone=%+v err=%v", tombstone, err)
	}
}

func TestPresentInvalidatesDeadBindingAndAdoptsExactCheckpoint(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", stateDir)
	logPath := filepath.Join(t.TempDir(), "cmux.log")
	t.Setenv("CMUX_TEST_LOG", logPath)
	bin := filepath.Join(t.TempDir(), "cmux")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_TEST_LOG"
if [ "$1 $2" = "tree --all" ]; then
  printf '%s\n' '{"windows":[{"workspaces":[{"ref":"workspace:1","panes":[{"ref":"pane:2","surfaces":[{"ref":"surface:7"}]}]}]}]}'
elif [ "$1 $2 $3" = "surface resume get" ]; then
  printf '%s\n' '{"resume_binding":{"kind":"relay","checkpoint_id":"apex-v3","command":"relay resume --session apex-v3","cwd":"/repo"}}'
fi
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sessionID := "sess-apex"
	attach := "relay resume --session apex-v3"
	v := &Viz{Bin: bin, bindings: map[string]binding{}}
	if err := v.persistBinding(sessionID, binding{SessionID: sessionID, Surface: "surface:243", Attach: attach}); err != nil {
		t.Fatal(err)
	}
	surface, err := v.Present(context.Background(), sessionID, attach, ports.Layout{})
	if err != nil {
		t.Fatal(err)
	}
	if surface != "surface:7" {
		t.Fatalf("surface = %q", surface)
	}
	logRaw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logRaw), "new-split") {
		t.Fatalf("present created a duplicate pane:\n%s", logRaw)
	}
	if !strings.Contains(string(logRaw), "surface resume set --surface surface:7") {
		t.Fatalf("adopted pane was not stamped:\n%s", logRaw)
	}
}

func TestProjectionTombstoneRejectsOlderReplay(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	v := &Viz{Bin: "/definitely/missing/cmux", Control: &targetConfig{Host: "home"}, bindings: map[string]binding{}}
	if err := v.persistBinding("sess-old", binding{SessionID: "sess-old", Revision: 10, Deleted: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	surface, err := v.ApplyProjection(context.Background(), ports.ProjectionEvent{V: 1, Revision: 9, Op: ports.ProjectionUpsert, Item: ports.Presentation{SessionID: "sess-old", Target: "home", TmuxName: "old"}})
	if err != nil || surface != "" {
		t.Fatalf("stale replay surface=%q err=%v", surface, err)
	}
	got, err := v.loadBinding("sess-old")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deleted || got.Revision != 10 {
		t.Fatalf("tombstone regressed: %+v", got)
	}
}

func TestVizClientArchivesLocalAuthorityButKeepsProjectionState(t *testing.T) {
	state := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", state)
	for _, path := range []string{"sessions.json", "handoffs", "parent-inbox", "bridge-tokens"} {
		full := filepath.Join(state, path)
		if filepath.Ext(path) != "" {
			if err := os.WriteFile(full, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(full, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	vizDir := filepath.Join(state, "viz")
	if err := os.MkdirAll(vizDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := retireLocalAuthorityState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("sessions authority remains: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(state, "retired-local-authority", "*", "sessions.json"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("sessions archive missing: paths=%v err=%v", archives, err)
	}
	if _, err := os.Stat(vizDir); err != nil {
		t.Fatalf("projection state was removed: %v", err)
	}
	// The marker is an invariant, not a one-shot migration: authority state
	// recreated later is quarantined on the next follower start too.
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := retireLocalAuthorityState(); err != nil {
		t.Fatal(err)
	}
	archives, _ = filepath.Glob(filepath.Join(state, "retired-local-authority", "*", "sessions.json"))
	if len(archives) != 2 {
		t.Fatalf("recreated authority was not quarantined: %v", archives)
	}
	for _, dir := range []string{"handoffs", "parent-inbox", "parent-watch"} {
		if err := os.MkdirAll(filepath.Join(state, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := retireLocalAuthorityState(); err != nil {
		t.Fatal(err)
	}
	archives, _ = filepath.Glob(filepath.Join(state, "retired-local-authority", "*"))
	if len(archives) != 2 {
		t.Fatalf("empty recreated directories caused another retirement: %v", archives)
	}
}

// A relay pane whose cmux resume binding is gone is invisible to the
// checkpoint reclaim, so relay opened a SECOND pane for a session already on
// screen. Observed 2026-08-09: beholder2's surface had resume_binding: null,
// and every reconcile rebuilt it in whatever workspace was nearby (its madrid
// sibling's, or simply the focused one). The title is relay's own mark and is
// 1:1 with the persist name, so it is a safe secondary key.
func TestUnboundTitleMatchReclaimsRelaysOwnOrphanedPane(t *testing.T) {
	titles := map[string]string{
		"surface:399": "◆ RELAY · beholder2",
		"surface:365": "◆ RELAY · beholder",
		"surface:900": "some editor",
	}
	if got := unboundTitleMatch(titles, map[string]bool{}, "beholder2"); got != "surface:399" {
		t.Fatalf("expected to reclaim surface:399, got %q", got)
	}
}

func TestUnboundTitleMatchNeverStealsACheckpointedSurface(t *testing.T) {
	// A surface carrying a checkpoint already answered authoritatively; taking
	// it here would bind this session to another session's pane -- worse than
	// the duplicate this fallback exists to prevent.
	titles := map[string]string{"surface:399": "◆ RELAY · beholder2"}
	checkpointed := map[string]bool{"surface:399": true}
	if got := unboundTitleMatch(titles, checkpointed, "beholder2"); got != "" {
		t.Fatalf("stole a checkpointed surface: %q", got)
	}
}

func TestUnboundTitleMatchRefusesWhenAmbiguous(t *testing.T) {
	// Two panes with the same relay title: guessing could adopt the wrong one.
	// Opening a fresh pane is recoverable; a wrong binding is not obvious.
	titles := map[string]string{
		"surface:1": "◆ RELAY · beholder2",
		"surface:2": "◆ RELAY · beholder2",
	}
	if got := unboundTitleMatch(titles, map[string]bool{}, "beholder2"); got != "" {
		t.Fatalf("guessed between ambiguous candidates: %q", got)
	}
}

func TestUnboundTitleMatchIgnoresNonRelaySurfacesAndEmptyNames(t *testing.T) {
	titles := map[string]string{"surface:900": "beholder2"} // no relay brand
	if got := unboundTitleMatch(titles, map[string]bool{}, "beholder2"); got != "" {
		t.Fatalf("matched a surface relay does not own: %q", got)
	}
	if got := unboundTitleMatch(titles, map[string]bool{}, ""); got != "" {
		t.Fatalf("matched on an empty persist name: %q", got)
	}
}
