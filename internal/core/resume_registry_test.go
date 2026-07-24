package core

import (
	"errors"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestResumeCleanedVsDisconnected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	r := &Registry{}

	RememberResume(&Session{
		ID: "sess-a", HostID: "c3", RemoteCWD: "~/dev/x",
		Persist: ports.PersistHandle{Kind: "tmux", Name: "proj-alive"},
		UpdatedAt: time.Now().UTC(),
	})
	presence, _, _ := r.ClassifyResume("proj-alive")
	if presence != PresenceDisconnected {
		t.Fatalf("want disconnected, got %s", presence)
	}
	_, _, _, p, err := r.ResolveResumeTarget("proj-alive", "")
	if err != nil || p != PresenceDisconnected {
		t.Fatalf("resolve disconnected: %v %s", err, p)
	}

	MarkResumeCleaned("proj-alive", "destroyed")
	presence, _, _ = r.ClassifyResume("proj-alive")
	if presence != PresenceCleaned {
		t.Fatalf("want cleaned, got %s", presence)
	}
	_, _, _, _, err = r.ResolveResumeTarget("proj-alive", "")
	if !errors.Is(err, ErrResumeCleaned) {
		t.Fatalf("want ErrResumeCleaned, got %v", err)
	}
}

func TestLivePresence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	r := &Registry{}
	_ = r.PutSession(&Session{
		ID: "sess-live", HostID: "c3", RemoteCWD: "~/x",
		Persist: ports.PersistHandle{Kind: "tmux", Name: "proj-live"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	RememberResume(&Session{
		ID: "sess-live", HostID: "c3", Persist: ports.PersistHandle{Name: "proj-live"},
	})
	presence, _, live := r.ClassifyResume("proj-live")
	if presence != PresenceLive || live == nil {
		t.Fatalf("got %s live=%v", presence, live)
	}
}
