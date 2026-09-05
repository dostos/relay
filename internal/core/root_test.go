package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type replacementProjectionViz struct {
	fail  bool
	items []ports.Presentation
}

func (*replacementProjectionViz) Kind() string                   { return "test" }
func (*replacementProjectionViz) Available(context.Context) bool { return true }
func (*replacementProjectionViz) Present(context.Context, string, string, ports.Layout) (string, error) {
	return "", nil
}
func (*replacementProjectionViz) Focus(context.Context, string) error                  { return nil }
func (*replacementProjectionViz) Close(context.Context, string) error                  { return nil }
func (*replacementProjectionViz) Layout(context.Context) (string, error)               { return "", nil }
func (*replacementProjectionViz) SaveRestorable(context.Context) (int, error)          { return 0, nil }
func (*replacementProjectionViz) RestoreSaved(context.Context) (int, error)            { return 0, nil }
func (*replacementProjectionViz) BrandLabels(context.Context, map[string]string) error { return nil }
func (v *replacementProjectionViz) ApplyProjection(_ context.Context, event ports.ProjectionEvent) (string, error) {
	if v.fail {
		return "", errors.New("viz offline")
	}
	v.items = append(v.items, event.Item)
	return "queued", nil
}

func newRootTestService(t *testing.T) (*RootService, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	// An always-on apex candidate and two project roots, all currently roots.
	for _, id := range []string{"sess-apex", "sess-proj-a", "sess-proj-b"} {
		sess := &Session{
			ID: id, HostID: "home",
			Persist:   ports.PersistHandle{Kind: "tmux", Name: id},
			CreatedAt: now,
		}
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
		if id != "sess-apex" {
			if err := reg.PutHandoff(&Handoff{ID: "ho-" + id, SessionID: id, HostID: "home", Kind: KindAgent, Status: StatusRunning, EventsPath: id + ".jsonl", CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &RootService{}, reg
}

func TestRulesPathIsProjectScopedAndRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_RULES_DIR", dir)
	got, err := RulesPath("beholder")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "beholder.md") {
		t.Fatalf("unexpected rules path %q", got)
	}
	for _, bad := range []string{"", "../etc/passwd", "a/b"} {
		if _, err := RulesPath(bad); err == nil {
			t.Fatalf("project %q must be rejected", bad)
		}
	}
}

// Enrolling must never imply autonomy the deployment cannot deliver: when the
// control plane can sleep, governance pauses with it.
func TestControlPlaneDisclosesWhenGovernancePauses(t *testing.T) {
	config := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", config)
	if err := os.WriteFile(filepath.Join(config, "host.yaml"), []byte("version: 1\nhost_id: test-host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_CONTROL_PLANE_ALWAYS_ON", "")
	cp := DescribeControlPlane()
	if cp.AlwaysOn {
		t.Fatal("must not claim always-on without an explicit declaration")
	}
	if cp.Warning == "" {
		t.Fatal("a sleepable control plane must carry a warning")
	}
	if cp.Host == "" {
		t.Fatal("the control plane must name its host")
	}
	if cp.HostID != "test-host" {
		t.Fatalf("control plane host id=%q", cp.HostID)
	}

	t.Setenv("RELAY_CONTROL_PLANE_ALWAYS_ON", "1")
	cp = DescribeControlPlane()
	if !cp.AlwaysOn {
		t.Fatal("an explicit declaration must be honoured")
	}
	if cp.Warning != "" {
		t.Fatalf("a declared always-on plane needs no warning, got %q", cp.Warning)
	}
}

func TestControlPlaneDeclarationPersistsInHostProfile(t *testing.T) {
	config := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", config)
	t.Setenv("RELAY_CONTROL_PLANE_ALWAYS_ON", "")
	path := filepath.Join(config, "host.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nhost_id: home-relay\nagents: []\npath_map: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := SetLocalControlPlaneAlwaysOn(true)
	if err != nil || !cp.AlwaysOn || cp.DeclaredBy != "host_config" || cp.HostID != "home-relay" {
		t.Fatalf("declaration=%+v err=%v", cp, err)
	}
	raw, err := os.ReadFile(filepath.Join(config, "control-plane.json"))
	if err != nil || !strings.Contains(string(raw), `"host_id": "home-relay"`) || !strings.Contains(string(raw), `"always_on": true`) {
		t.Fatalf("persisted declaration=%q err=%v", raw, err)
	}
	cp, err = SetLocalControlPlaneAlwaysOn(false)
	if err != nil || cp.AlwaysOn || cp.Warning == "" {
		t.Fatalf("sleepable declaration=%+v err=%v", cp, err)
	}
}

func TestControlPlaneMalformedOrMissingProfileFailsClosed(t *testing.T) {
	config := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", config)
	t.Setenv("RELAY_CONTROL_PLANE_ALWAYS_ON", "")
	if err := os.WriteFile(filepath.Join(config, "host.yaml"), []byte("control_plane: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cp := DescribeControlPlane(); cp.AlwaysOn || cp.Warning == "" {
		t.Fatalf("malformed profile claimed autonomy: %+v", cp)
	}
	if _, err := SetLocalControlPlaneAlwaysOn(true); err == nil {
		t.Fatal("malformed profile was overwritten")
	}
}

func TestControlPlaneDeclarationCopiedToAnotherHostFailsClosed(t *testing.T) {
	config := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", config)
	t.Setenv("RELAY_CONTROL_PLANE_ALWAYS_ON", "")
	if err := os.WriteFile(filepath.Join(config, "host.yaml"), []byte("version: 1\nhost_id: laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "control-plane.json"), []byte(`{"v":1,"host_id":"home-relay","always_on":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cp := DescribeControlPlane(); cp.AlwaysOn || cp.Warning == "" {
		t.Fatalf("foreign declaration claimed autonomy: %+v", cp)
	}
}
