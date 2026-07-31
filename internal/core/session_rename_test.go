package core

import (
	"context"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type renamePersistence struct {
	from string
	to   string
}

func (p *renamePersistence) Kind() string { return "tmux" }
func (p *renamePersistence) Create(context.Context, ports.Transport, string, string, string) (ports.PersistHandle, error) {
	return ports.PersistHandle{}, nil
}
func (p *renamePersistence) Rename(_ context.Context, _ ports.Transport, from, to ports.PersistHandle) error {
	p.from, p.to = from.Name, to.Name
	return nil
}
func (p *renamePersistence) Exists(context.Context, ports.Transport, ports.PersistHandle) (bool, error) {
	return true, nil
}
func (p *renamePersistence) Destroy(context.Context, ports.Transport, ports.PersistHandle) error {
	return nil
}
func (p *renamePersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return "", nil
}
func (p *renamePersistence) Send(context.Context, ports.Transport, ports.PersistHandle, string, bool) error {
	return nil
}
func (p *renamePersistence) Resize(context.Context, ports.Transport, ports.PersistHandle) error {
	return nil
}
func (p *renamePersistence) AttachCommand(ports.PersistHandle, string) string { return "" }
func (p *renamePersistence) DeadStatus(context.Context, ports.Transport, ports.PersistHandle) (bool, int, error) {
	return false, 0, nil
}
func (p *renamePersistence) InstallSensors(context.Context, ports.Transport, ports.PersistHandle, int, func(string) (string, error)) error {
	return nil
}

func TestSessionRenameKeepsIdentityAndRetargetsDurableState(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	reg := &Registry{}
	sess := &Session{
		ID: "sess-beholder", HostID: "c3", RemoteCWD: "~/dev/beholder",
		Persist: ports.PersistHandle{Kind: "tmux", Name: "dostos-workspace-cdx"},
		Labels:  map[string]string{DisplayNameLabel: "beholder"}, CreatedAt: now, UpdatedAt: now,
	}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	RememberResume(sess)
	RememberPane("surface:187", sess, true)
	if err := AppendSessionStart(sess); err != nil {
		t.Fatal(err)
	}
	persist := &renamePersistence{}
	service := &SessionService{
		Reg: reg, Persist: persist,
		NewTransport: func(string) (ports.Transport, error) {
			return &fakeTransport{id: "c3", outputs: map[string]string{"c3": ""}}, nil
		},
	}

	renamed, err := service.Rename(context.Background(), sess.ID, "beholder")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != sess.ID || renamed.Persist.Name != "beholder" {
		t.Fatalf("renamed session = %+v", renamed)
	}
	if persist.from != "dostos-workspace-cdx" || persist.to != "beholder" {
		t.Fatalf("remote rename = %q -> %q", persist.from, persist.to)
	}
	stored, err := reg.GetSession(sess.ID)
	if err != nil || stored.Persist.Name != "beholder" {
		t.Fatalf("stored session = %+v, err %v", stored, err)
	}
	if _, err := LookupResume("dostos-workspace-cdx"); err == nil {
		t.Fatal("old resume key still exists")
	}
	resume, err := LookupResume("beholder")
	if err != nil || resume.SessionID != sess.ID {
		t.Fatalf("new resume = %+v, err %v", resume, err)
	}
	pane, err := ReadPaneBinding("surface:187")
	if err != nil || pane.PersistName != "beholder" {
		t.Fatalf("pane binding = %+v, err %v", pane, err)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Nodes) != 1 || graph.Nodes[0].PersistName != "beholder" {
		t.Fatalf("history = %+v, err %v", graph, err)
	}
}
