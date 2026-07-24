package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestFindByPersistNamePrefersRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	r := &Registry{}
	repoA := filepath.Join(dir, "proj-a")
	repoB := filepath.Join(dir, "proj-b")
	_ = os.MkdirAll(repoA, 0o755)
	_ = os.MkdirAll(repoB, 0o755)
	_ = r.PutSession(&Session{
		ID: "sess-old", HostID: "c1", RemoteCWD: "~/a", Persist: ports.PersistHandle{Kind: "tmux", Name: "shared-name"},
		RepoRef: repoA, CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC().Add(-time.Hour),
	})
	_ = r.PutSession(&Session{
		ID: "sess-new", HostID: "c3", RemoteCWD: "~/b", Persist: ports.PersistHandle{Kind: "tmux", Name: "shared-name"},
		RepoRef: repoB, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	got, err := r.FindByPersistName("shared-name", repoB)
	if err != nil || got.ID != "sess-new" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestResumeLaunchCmdCarriesSessionFlag(t *testing.T) {
	cmd := ResumeLaunchCmd("dostos-workspace-abc")
	if !strings.Contains(cmd, "resume") || !strings.Contains(cmd, "--session") || !strings.Contains(cmd, "dostos-workspace-abc") {
		t.Fatalf("cmd=%q", cmd)
	}
}

func TestShouldRetryAttach(t *testing.T) {
	if shouldRetryAttach(0) || shouldRetryAttach(130) {
		t.Fatal("clean exit / Ctrl+C must not retry")
	}
	if !shouldRetryAttach(255) || !shouldRetryAttach(1) {
		t.Fatal("SSH drop codes must retry")
	}
}

func TestAutoReconnectEnv(t *testing.T) {
	t.Setenv("RELAY_AUTO_RECONNECT", "")
	if !autoReconnectEnabled(false) {
		t.Fatal("default on")
	}
	if autoReconnectEnabled(true) {
		t.Fatal("--no-reconnect")
	}
	t.Setenv("RELAY_AUTO_RECONNECT", "0")
	if autoReconnectEnabled(false) {
		t.Fatal("env off")
	}
}
