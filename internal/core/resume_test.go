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

type resumePersist struct{ ports.Persistence }

type hostTransport struct{ *fakeTransport }

func (resumePersist) AttachCommand(h ports.PersistHandle, _ string) string {
	return "tmux attach-session -t =" + h.Name
}

func TestResumeWithHostAttachesWithoutLocalAuthority(t *testing.T) {
	// A Forge tab runs `relay resume --session NAME --host HOST` and must
	// attach to that host with no local registry row.
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	var target string
	transport := &hostTransport{fakeTransport: &fakeTransport{id: "hamburg"}}
	svc := &SessionService{
		Reg: &Registry{}, Persist: resumePersist{},
		NewTransport: func(host string) (ports.Transport, error) {
			target = host
			return transport, nil
		},
	}
	if err := svc.ResumeOpts(context.Background(), "phyzfuzz-feas-3", "", ResumeOpts{TargetHost: "hamburg"}); err != nil {
		t.Fatal(err)
	}
	if target != "hamburg" {
		t.Fatalf("target=%q", target)
	}
	sessions, err := svc.Reg.ListSessions()
	if err != nil || len(sessions) != 0 {
		t.Fatalf("host-named resume created local authority: sessions=%v err=%v", sessions, err)
	}
}

func TestResumeRejectsSSHOptionHost(t *testing.T) {
	svc := &SessionService{Reg: &Registry{}, Persist: resumePersist{}, NewTransport: func(string) (ports.Transport, error) {
		t.Fatal("invalid host reached transport")
		return nil, nil
	}}
	if err := svc.ResumeOpts(context.Background(), "demo", "", ResumeOpts{TargetHost: "-oProxyCommand=bad"}); err == nil {
		t.Fatal("SSH option accepted as host")
	}
}

type testDiagnostic string

func (d testDiagnostic) LastDiagnostic() string { return string(d) }

func TestLatestDiagnosticPrefersCurrentTransportDetail(t *testing.T) {
	got := latestDiagnostic(errors.New("exit status 255"), testDiagnostic("network unreachable"), testDiagnostic("stale stderr detail"))
	if got != "network unreachable" {
		t.Fatalf("latestDiagnostic() = %q", got)
	}
}

func TestResumeRetryStatusIncludesLastError(t *testing.T) {
	status := resumeRetryStatus("test-host/demo", 2, 3*time.Second, "ssh: connect to host 203.0.113.7 port 2222: Operation timed out")
	for _, want := range []string{"waiting test-host/demo", "last error: ssh: connect to host", "retry 2 in 3s"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status %q missing %q", status, want)
		}
	}
}

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
