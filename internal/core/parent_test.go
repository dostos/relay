package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

type fakeParentNotifier struct {
	bound   []string
	notices []ParentNotice
}

func (f *fakeParentNotifier) BindLocalParent(_ context.Context, sessionID, surface string) (string, error) {
	f.bound = append(f.bound, sessionID+"@"+surface)
	return surface, nil
}
func (f *fakeParentNotifier) NotifyParent(_ context.Context, _ string, notice ParentNotice) error {
	f.notices = append(f.notices, notice)
	return nil
}

func newParentTestService(t *testing.T) (*ParentService, *fakeParentNotifier, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	notifier := &fakeParentNotifier{}
	sessions := &SessionService{Reg: reg, Persist: &renamePersistence{}, NewTransport: func(host string) (ports.Transport, error) {
		return &fakeTransport{id: host, outputs: map[string]string{host: ""}}, nil
	}}
	return &ParentService{Reg: reg, Sessions: sessions, Notifier: notifier}, notifier, reg
}

func TestRegisterLocalParentCreatesRealSessionAndBinding(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	repo := t.TempDir()
	sess, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{
		Surface: "surface:41", Name: "personal-db-main", RepoRefs: []string{repo}, WakeMode: "notify",
	})
	if err != nil || !created {
		t.Fatalf("register created=%v err=%v", created, err)
	}
	if !isLocalParent(sess) || sess.VizSurfaceRef != "surface:41" || sess.RepoRefs[0] != repo {
		t.Fatalf("session = %+v", sess)
	}
	if len(notifier.bound) != 1 {
		t.Fatalf("bindings = %v", notifier.bound)
	}
	stored, err := reg.GetSession(sess.ID)
	if err != nil || stored.Labels["parent_state"] != "active" {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}

	again, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Surface: "surface:41", RepoRefs: []string{repo}})
	if err != nil || created || again.ID != sess.ID {
		t.Fatalf("idempotent register = %+v created=%v err=%v", again, created, err)
	}
}

func TestLinkChildAdoptsExistingGoalExactlyOnce(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "existing-goal"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-existing", SessionID: child.ID, HostID: "c3", Kind: KindAgent, Status: StatusRunning, CreatedAt: now}
	for _, sess := range []*Session{parent, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}

	linked, err := service.LinkChild(parent.ID, ho.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked.SourceSessionID != parent.ID || linked.SourceHostID != LocalHostID || linked.SourcePersistName != parent.Persist.Name {
		t.Fatalf("linked handoff = %+v", linked)
	}
	stored, err := reg.GetSession(child.ID)
	if err != nil || stored.SourceSessionID != parent.ID || stored.CreatedByHandoffID != ho.ID {
		t.Fatalf("linked child = %+v err=%v", stored, err)
	}
	if _, err := service.LinkChild(parent.ID, ho.ID); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Edges) != 1 || graph.Edges[0].HandoffID != ho.ID {
		t.Fatalf("history = %+v err=%v", graph, err)
	}
}

func TestRouteChildEventDeduplicatesAndKeepsMessageCompact(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local-main"}, Labels: map[string]string{"role": ParentRole, "wake_mode": "notify"}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	for _, sess := range []*Session{parent, child} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: "c3", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	ev := coord.Event{Seq: 7, Kind: "permission_required", Meta: map[string]any{"text": strings.Repeat("approve ", 300), "correlation_id": "req-7"}}
	msg, err := service.RouteChildEvent(context.Background(), ho, ev)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != "permission_required" || msg.CorrelationID != "req-7" || len(msg.Text) > parentTextLimit {
		t.Fatalf("message = %+v", msg)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("notices = %d", len(notifier.notices))
	}
	dup, err := service.RouteChildEvent(context.Background(), ho, ev)
	if err != nil || dup.ID != msg.ID || len(notifier.notices) != 1 {
		t.Fatalf("duplicate = %+v err=%v notices=%d", dup, err, len(notifier.notices))
	}
	pending, err := service.ListMessages(parent.ID, true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	graph, err := LoadHistory()
	if err != nil || len(graph.Communications) != 1 || graph.Communications[0].Action != "request" {
		t.Fatalf("history=%+v err=%v", graph, err)
	}
}

func TestReplyAndAckCloseInboxWithCorrelatedHistory(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	child := &Session{ID: "sess-child", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"}, CreatedAt: now}
	_ = reg.PutSession(parent)
	_ = reg.PutSession(child)
	ho := &Handoff{ID: "ho-1", SessionID: child.ID, HostID: "c3", Kind: KindAgent, Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	ask, _ := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 1, Kind: "ask", Meta: map[string]any{"q": "deploy?"}})
	replied, err := service.Reply(context.Background(), ask.ID, "yes")
	if err != nil || replied.State != ParentMessageReplied {
		t.Fatalf("reply=%+v err=%v", replied, err)
	}
	result, _ := service.RouteChildEvent(context.Background(), ho, coord.Event{Seq: 2, Kind: "result", Meta: map[string]any{"text": "done"}})
	acked, err := service.Ack(result.ID)
	if err != nil || acked.State != ParentMessageAcked {
		t.Fatalf("ack=%+v err=%v", acked, err)
	}
	pending, _ := service.ListMessages(parent.ID, true)
	if len(pending) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
	graph, _ := LoadHistory()
	if len(graph.Communications) != 4 {
		t.Fatalf("communications = %+v", graph.Communications)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Relay Test", "GIT_AUTHOR_EMAIL=relay@example.invalid", "GIT_COMMITTER_NAME=Relay Test", "GIT_COMMITTER_EMAIL=relay@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func cleanPushedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo, remote := filepath.Join(root, "repo"), filepath.Join(root, "remote.git")
	if out, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare: %v %s", err, out)
	}
	if out, err := exec.Command("git", "init", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README")
	gitRun(t, repo, "commit", "-m", "init")
	gitRun(t, repo, "remote", "add", "origin", remote)
	gitRun(t, repo, "push", "-u", "origin", "main")
	return repo
}

func TestRetirementGateRequiresStateChildrenInboxAndPushedRepos(t *testing.T) {
	service, _, reg := newParentTestService(t)
	repo := cleanPushedRepo(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local"}, RepoRef: repo, RepoRefs: []string{repo}, Labels: map[string]string{"role": ParentRole, "parent_state": "complete"}, CreatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	gate, err := service.RetirementStatus(context.Background(), parent.ID)
	if err != nil || !gate.Eligible {
		t.Fatalf("clean gate=%+v err=%v", gate, err)
	}

	if err := os.WriteFile(filepath.Join(repo, "dirty"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gate, _ = service.RetirementStatus(context.Background(), parent.ID)
	if gate.Eligible {
		t.Fatalf("dirty repo eligible: %+v", gate)
	}
	if err := os.Remove(filepath.Join(repo, "dirty")); err != nil {
		t.Fatal(err)
	}

	ho := &Handoff{ID: "ho-active", SessionID: "sess-child", HostID: "c3", Status: StatusRunning, SourceSessionID: parent.ID, CreatedAt: now}
	_ = reg.PutHandoff(ho)
	gate, _ = service.RetirementStatus(context.Background(), parent.ID)
	if gate.Eligible || len(gate.ActiveChildren) != 1 {
		t.Fatalf("active child gate=%+v", gate)
	}
	ho.Status, ho.Outcome = StatusDone, "done"
	_ = reg.PutHandoff(ho)
	retired, err := service.Retire(context.Background(), parent.ID, false)
	if err != nil || !retired.Eligible || !retired.Closed {
		t.Fatalf("retire=%+v err=%v", retired, err)
	}
	if _, err := reg.GetSession(parent.ID); err == nil {
		t.Fatal("retired parent remains registered")
	}
}

func TestSessionDestroyCannotBypassParentGate(t *testing.T) {
	service, _, reg := newParentTestService(t)
	now := time.Now().UTC()
	parent := &Session{ID: "sess-parent", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "local"}, Labels: map[string]string{"role": ParentRole}, CreatedAt: now}
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	if err := service.Sessions.Destroy(context.Background(), parent.ID, false); err == nil || !strings.Contains(err.Error(), "parent retire") {
		t.Fatalf("unguarded destroy error = %v", err)
	}
}
