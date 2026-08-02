package core

import (
	"context"
	"os"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

func cleanupFixture(t *testing.T, withActiveHandoff bool) (*SessionService, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	manager := &Session{ID: "sess-manager", HostID: "home", Persist: ports.PersistHandle{Kind: "tmux", Name: "manager"}}
	child := &Session{
		ID: "sess-child", HostID: "worker", SourceSessionID: manager.ID,
		Persist: ports.PersistHandle{Kind: "tmux", Name: "failed-child"},
		Labels:  map[string]string{"role": "handoff"}, CreatedByHandoffID: "ho-child",
	}
	for _, session := range []*Session{manager, child} {
		if err := reg.PutSession(session); err != nil {
			t.Fatal(err)
		}
	}
	if withActiveHandoff {
		if err := reg.PutHandoff(&Handoff{ID: "ho-child", SessionID: child.ID, SourceSessionID: manager.ID, Status: StatusRunning}); err != nil {
			t.Fatal(err)
		}
	}
	return &SessionService{
		Reg: reg, Persist: &renamePersistence{},
		NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "worker"}, nil },
	}, reg
}

func TestCleanupFailedChildRetiresMissingHandoffArtifact(t *testing.T) {
	service, reg := cleanupFixture(t, false)
	if err := service.CleanupFailedChild(context.Background(), "sess-manager", "sess-child"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GetSession("sess-child"); err == nil {
		t.Fatal("failed child remains authoritative")
	}
}

func TestCleanupFailedChildRefusesActiveAndUnrelatedSessions(t *testing.T) {
	service, _ := cleanupFixture(t, true)
	if err := service.CleanupFailedChild(context.Background(), "sess-manager", "sess-child"); err == nil {
		t.Fatal("active handoff child was cleaned")
	}
	if err := service.CleanupFailedChild(context.Background(), "sess-other", "sess-child"); err == nil {
		t.Fatal("unrelated manager cleaned child")
	}
}

func TestDestroyKeepRemotePreservesResumeAndBridgeIdentity(t *testing.T) {
	service, _ := cleanupFixture(t, false)
	service.Viz = &deletionViz{}
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
