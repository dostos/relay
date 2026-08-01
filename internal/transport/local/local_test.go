package local

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIDIsTheLocalHostID(t *testing.T) {
	if got := New().ID(); got != "local" {
		t.Fatalf("local transport must identify as the local host, got %q", got)
	}
}

func TestRunExecutesOnThisMachine(t *testing.T) {
	stdout, _, err := New().Run(context.Background(), "", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Fatalf("want hello, got %q", stdout)
	}
}

func TestRunHonoursCWD(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := New().Run(context.Background(), dir, "pwd")
	if err != nil {
		t.Fatal(err)
	}
	// macOS resolves TempDir through /private; compare the resolved forms.
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(stdout))
	if got != want {
		t.Fatalf("want cwd %q, got %q", want, got)
	}
}

// A failing command must surface both the error and stderr, the same way the
// SSH transport does — callers format "%w (%s)" from the pair.
func TestRunReportsFailureWithStderr(t *testing.T) {
	_, stderr, err := New().Run(context.Background(), "", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(stderr, "boom") {
		t.Fatalf("stderr must be captured, got %q", stderr)
	}
}

func TestRunStreamWritesOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := New().RunStream(context.Background(), "", "echo streamed", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "streamed") {
		t.Fatalf("stream must carry stdout, got %q", buf.String())
	}
}

func TestWriteThenReadFileRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "f.txt")
	tr := New()
	if err := tr.WriteFile(context.Background(), path, []byte("payload"), "600"); err != nil {
		t.Fatal(err)
	}
	got, err := tr.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Fatalf("want payload, got %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode must be applied, got %v", info.Mode().Perm())
	}
}

// relay's own callers pass paths like "~/.config/relay/host.yaml". The SSH
// transport gets tilde expansion for free because the remote shell does it;
// the local transport must not regress that by using os.ReadFile directly.
func TestPathsAreShellExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	stdout, _, err := New().Run(context.Background(), "", "printf %s ~")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != home {
		t.Fatalf("tilde must expand to %q, got %q", home, stdout)
	}
}

// The whole point of this transport: no ssh hop. Wrapping in "ssh -t local"
// is what made every local session uncapturable.
func TestInteractiveCommandDoesNotWrapInSSH(t *testing.T) {
	got := New().InteractiveCommand("tmux attach -t x")
	if strings.Contains(got, "ssh") {
		t.Fatalf("a local interactive command must not shell out to ssh, got %q", got)
	}
	if !strings.Contains(got, "tmux attach -t x") {
		t.Fatalf("the command must be preserved, got %q", got)
	}
}
