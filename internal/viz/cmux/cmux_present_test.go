package cmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

func TestVizServiceUsesRelayManagedReconnect(t *testing.T) {
	v := &Viz{Targets: map[string]targetConfig{
		"home-relay": {Host: "100.108.118.32", User: "dostos", Port: 2222, Identity: "~/.ssh/viz"},
	}}
	command, err := v.attachCommand(ports.Presentation{SessionID: "sess-1", Target: "home-relay", TmuxName: "beholder-pdf-main"})
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
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' '{\"session_id\":\"sess-live\",\"target\":\"home-relay\",\"tmux_name\":\"apex-v4\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	v := &Viz{
		ServiceID: "mac",
		Control:   &targetConfig{Host: "control.example", Identity: "~/.ssh/control"},
		Targets:   map[string]targetConfig{"home-relay": {Host: "home.example", Identity: "~/.ssh/attach"}},
	}
	target, err := v.ResolveProjectedResume(context.Background(), "apex-v4", ports.ResumeResolveOpts{})
	if err != nil || target.Host != "home.example" {
		t.Fatalf("live target=%+v err=%v", target, err)
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
	items := []ports.Presentation{{SessionID: "sess-engram", ParentSessionID: "sess-apex", Target: "c3", TmuxName: "engram"}}
	raw, _ := json.Marshal(items)
	if err := saveBytes(v.authoritySnapshotPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	projected, err := v.ProjectionSessions(context.Background())
	if err != nil || len(projected) != 1 {
		t.Fatalf("projection=%+v err=%v", projected, err)
	}
	got := projected[0]
	if got.ParentSessionID != "sess-apex" || got.Target != "c3" || got.TmuxName != "engram" {
		t.Fatalf("stale binding leaked into projection: %+v", got)
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
	if _, err := v.attachCommand(ports.Presentation{SessionID: "sess-1", Target: "raw-host", TmuxName: "safe"}); err == nil {
		t.Fatal("unknown policy key must not become a raw ssh destination")
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
