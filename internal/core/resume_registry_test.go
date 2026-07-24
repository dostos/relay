package core

import (
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestResumeRegistrySurvivesWithoutLiveSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	RememberResume(&Session{
		ID: "sess-x", HostID: "c3", RemoteCWD: "~/dev/dostos-workspace",
		Persist: ports.PersistHandle{Kind: "tmux", Name: "dostos-workspace-dead"},
		RepoRef: "/tmp/proj", UpdatedAt: time.Now().UTC(),
	})
	r := &Registry{}
	host, cwd, h, err := r.ResolveResumeTarget("dostos-workspace-dead", "")
	if err != nil {
		t.Fatal(err)
	}
	if host != "c3" || h.Name != "dostos-workspace-dead" || cwd == "" {
		t.Fatalf("host=%s cwd=%s h=%+v", host, cwd, h)
	}
}
