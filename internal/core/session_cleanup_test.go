package core

import (
	"context"
	"os"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

func cleanupFixture(t *testing.T) (*SessionService, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	child := &Session{
		ID: "sess-child", HostID: "worker",
		Persist: ports.PersistHandle{Kind: "tmux", Name: "failed-child"},
		Labels:  map[string]string{"role": "interactive"},
	}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	return &SessionService{
		Reg: reg, Persist: &renamePersistence{},
		NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "worker"}, nil },
	}, reg
}

func TestDestroyKeepRemotePreservesResumeAndBridgeIdentity(t *testing.T) {
	service, _ := cleanupFixture(t)
	child, err := service.Reg.GetSession("sess-child")
	if err != nil {
		t.Fatal(err)
	}
	RememberResume(child)
	if err := rememberBridgeToken(child.ID, "br-child"); err != nil {
		t.Fatal(err)
	}
	if err := service.Destroy(context.Background(), child.ID, true); err != nil {
		t.Fatal(err)
	}
	resume, err := LookupResume(child.Persist.Name)
	if err != nil || resume.State != ResumeStateResumable {
		t.Fatalf("kept remote became non-resumable: %+v err=%v", resume, err)
	}
	if _, err := os.Stat(bridgeTokenPath(child.ID)); err != nil {
		t.Fatalf("kept remote lost bridge identity: %v", err)
	}
}
