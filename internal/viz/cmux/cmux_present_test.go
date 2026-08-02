package cmux

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

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
