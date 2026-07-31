package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestPaneBindingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)

	err := WritePaneBinding(PaneBinding{
		Surface:     "surface:68",
		PersistName: "opaquebench-oqb",
		HostID:      "c3",
		Pinned:      true,
		UpdatedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "panes", "surface_68.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}
	got, err := ReadPaneBinding("68")
	if err != nil {
		t.Fatal(err)
	}
	if got.PersistName != "opaquebench-oqb" || got.HostID != "c3" || !got.Pinned {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveResumeFromPanePrefersPinned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	t.Setenv("CMUX_SURFACE_REF", "surface:90")
	identifySurface = func() (string, error) { return "", nil }
	t.Cleanup(func() { identifySurface = defaultIdentifySurface })
	cmuxResumeCheckpoint = func(string) (string, string, error) {
		t.Fatal("should not call cmux when pinned history exists")
		return "", "", nil
	}
	t.Cleanup(func() { cmuxResumeCheckpoint = defaultCmuxResumeCheckpoint })

	RememberPane("surface:90", &Session{
		HostID: "c1", RemoteCWD: "~/x",
		Persist: ports.PersistHandle{Name: "phyzfuzz"},
		RepoRef: "/tmp/phyz",
	}, true)

	name, cwd, surf, err := ResolveResumeFromPane()
	if err != nil {
		t.Fatal(err)
	}
	if name != "phyzfuzz" || surf != "surface:90" || cwd != "/tmp/phyz" {
		t.Fatalf("name=%q cwd=%q surf=%q", name, cwd, surf)
	}
}

func TestResolveResumeFromPaneFallsBackToCmux(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	t.Setenv("CMUX_SURFACE_REF", "surface:70")
	identifySurface = func() (string, error) { return "", nil }
	t.Cleanup(func() { identifySurface = defaultIdentifySurface })
	cmuxResumeCheckpoint = func(surface string) (string, string, error) {
		if surface != "surface:70" {
			t.Fatalf("surface=%s", surface)
		}
		return "beholder-minecraft-1", "/Users/jingyu/dev/mc", nil
	}
	t.Cleanup(func() { cmuxResumeCheckpoint = defaultCmuxResumeCheckpoint })

	name, cwd, surf, err := ResolveResumeFromPane()
	if err != nil {
		t.Fatal(err)
	}
	if name != "beholder-minecraft-1" || cwd == "" || surf != "surface:70" {
		t.Fatalf("name=%q cwd=%q surf=%q", name, cwd, surf)
	}
	// Cached as pinned for next bare resume.
	b, err := ReadPaneBinding("surface:70")
	if err != nil || !b.Pinned || b.PersistName != "beholder-minecraft-1" {
		t.Fatalf("cache %+v err %v", b, err)
	}
}

func TestCurrentSurfaceFromEnv(t *testing.T) {
	t.Setenv("CMUX_SURFACE_REF", "surface:12")
	identifySurface = func() (string, error) {
		t.Fatal("should not identify when env set")
		return "", nil
	}
	t.Cleanup(func() { identifySurface = defaultIdentifySurface })
	s, err := CurrentSurface()
	if err != nil || s != "surface:12" {
		t.Fatalf("got %q err %v", s, err)
	}
}

func TestSurfaceFromEnvironmentDoesNotIdentify(t *testing.T) {
	t.Setenv("CMUX_SURFACE_REF", "")
	t.Setenv("CMUX_SURFACE", "")
	identifySurface = func() (string, error) {
		t.Fatal("environment-only lookup must not inspect focused cmux state")
		return "", nil
	}
	t.Cleanup(func() { identifySurface = defaultIdentifySurface })
	if surface, ok := SurfaceFromEnvironment(); ok || surface != "" {
		t.Fatalf("surface=%q ok=%v", surface, ok)
	}
}

func TestExtractSessionFlag(t *testing.T) {
	cases := []struct{ in, want string }{
		{`'/Users/x/relay' resume --session opaquebench-oqb`, "opaquebench-oqb"},
		{`relay resume -s foo`, "foo"},
		{`relay resume --session=bar`, "bar"},
		{`echo hi`, ""},
	}
	for _, c := range cases {
		if got := extractSessionFlag(c.in); got != c.want {
			t.Fatalf("%q → %q want %q", c.in, got, c.want)
		}
	}
}
