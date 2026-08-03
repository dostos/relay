package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type agentPanePersistence struct {
	renamePersistence
	capture string
	sent    []string
}

func TestAuthorizeHandoffManagerFailover(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	for _, s := range []*Session{
		{ID: "sess-apex", CreatedAt: now},
		{ID: "sess-parent", SourceSessionID: "sess-apex", CreatedAt: now},
		{ID: "sess-child", SourceSessionID: "sess-parent", CreatedAt: now},
	} {
		if err := reg.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutHandoff(&Handoff{ID: "ho-child", SessionID: "sess-child", SourceSessionID: "sess-parent", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	if skipped, err := AuthorizeHandoffManager(reg, "ho-child", "sess-parent", nil); err != nil || len(skipped) != 0 {
		t.Fatalf("direct manager skipped=%v err=%v", skipped, err)
	}
	if _, err := AuthorizeHandoffManager(reg, "ho-child", "sess-apex", func(string) (bool, error) { return true, nil }); err == nil || !strings.Contains(err.Error(), "is live") {
		t.Fatalf("live parent bypass err=%v", err)
	}
	skipped, err := AuthorizeHandoffManager(reg, "ho-child", "sess-apex", func(string) (bool, error) { return false, nil })
	if err != nil || len(skipped) != 1 || skipped[0] != "sess-parent" {
		t.Fatalf("absent parent failover skipped=%v err=%v", skipped, err)
	}
	if _, err := AuthorizeHandoffManager(reg, "ho-child", "sess-apex", func(string) (bool, error) { return false, context.DeadlineExceeded }); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown parent liveness err=%v", err)
	}
	if _, err := AuthorizeHandoffManager(reg, "ho-child", "sess-stranger", func(string) (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "not a manager ancestor") {
		t.Fatalf("stranger err=%v", err)
	}
}

func (p *agentPanePersistence) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	return p.capture, nil
}

func (p *agentPanePersistence) Send(_ context.Context, _ ports.Transport, _ ports.PersistHandle, text string, _ bool) error {
	p.sent = append(p.sent, text)
	return nil
}

func TestDecideNextMatrix(t *testing.T) {
	cases := []struct {
		kind     HandoffKind
		ev       string
		timedOut bool
		want     string
	}{
		{KindJob, "exit", false, "done"},
		{KindAgent, "exit", false, "done"},
		{KindJob, "idle", false, "wait"},
		{KindAgent, "idle", false, "send"},
		{KindJob, "needs_input", false, "escalate"},
		{KindAgent, "needs_input", false, "send"},
		{KindJob, "started", false, "wait"},
		{KindAgent, "", true, "wait"},
		{KindJob, "", true, "wait"},
	}
	for _, tc := range cases {
		got := DecideNext(tc.kind, tc.ev, tc.timedOut)
		if got != tc.want {
			t.Fatalf("kind=%s ev=%q timeout=%v got=%s want=%s", tc.kind, tc.ev, tc.timedOut, got, tc.want)
		}
	}
}

func TestAgentResponseDoesNotRepeatGoal(t *testing.T) {
	ho := &Handoff{ID: "ho-1", SessionID: "sess-1", HostID: "c3", Kind: KindAgent, Status: StatusRunning, Goal: strings.Repeat("expensive goal ", 1000), LastSeq: 42}
	resp := (&HandoffService{}).agentBase(ho)
	resp.Next = "wait"
	resp.Argv = append(argvFor("wait", ho.ID), "--from", "42")
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "expensive goal") || strings.Contains(string(raw), "\"goal\"") {
		t.Fatalf("agent response repeated durable goal: %s", raw)
	}
	if len(raw) > 220 {
		t.Fatalf("agent response is not compact: %d bytes: %s", len(raw), raw)
	}
}

func TestCompactAgentEventLiftsTextOnce(t *testing.T) {
	ev := compactAgentEvent(&Event{TS: "unused", Seq: 7, Sess: "unused", Kind: "ask", Meta: map[string]any{"q": "prod or dev?", "correlation_id": "env"}})
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "prod or dev?") != 1 {
		t.Fatalf("event text should appear once: %s", raw)
	}
	if strings.Contains(string(raw), "unused") || strings.Contains(string(raw), "\"q\"") {
		t.Fatalf("event projection leaked redundant fields: %s", raw)
	}
}

func TestArgvForWaitAndDone(t *testing.T) {
	a := argvFor("wait", "ho-1")
	if len(a) != 4 || a[0] != "relay" || a[2] != "wait" || a[3] != "ho-1" {
		t.Fatalf("bad wait argv %#v", a)
	}
	d := argvFor("done", "ho-1")
	if len(d) != 4 || d[2] != "done" || d[3] != "ho-1" {
		t.Fatalf("bad done argv %#v", d)
	}
	if argvFor("null", "ho-1") != nil {
		t.Fatal("null should have no argv")
	}
}

func TestManagedStartHasNoDuplicateWait(t *testing.T) {
	managed := AgentResponse{Next: "stale", Argv: []string{"stale"}}
	setStartContinuation(&managed, "ho-1", true)
	if !managed.Managed || managed.Next != "" || managed.Argv != nil {
		t.Fatalf("managed continuation = %+v", managed)
	}
	unmanaged := AgentResponse{}
	setStartContinuation(&unmanaged, "ho-2", false)
	if unmanaged.Managed || unmanaged.Next != "wait" || strings.Join(unmanaged.Argv, " ") != "relay agent wait ho-2" {
		t.Fatalf("unmanaged continuation = %+v", unmanaged)
	}
}

func TestStartedEventCannotClearDeliveryBlockedState(t *testing.T) {
	ho := &Handoff{Kind: KindAgent, Status: StatusNeedsInput, DeliveryState: EffectBlocked}
	applyHandoffEventStatus(ho, "started")
	if ho.Status != StatusNeedsInput || ho.DeliveryState != EffectBlocked {
		t.Fatalf("startup event cleared security hold: %+v", ho)
	}
}

func TestAbsentAgentCannotAdvertiseOrAcceptSend(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-exited", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "exited"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-exited", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusRunning, CreatedAt: now}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	persist := &agentPanePersistence{capture: "dostos@home:~/dev/relay$ "}
	sessions := &SessionService{
		Reg: reg, Persist: persist,
		NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil },
	}
	service := &HandoffService{Reg: reg, Sessions: sessions}

	captured, err := service.AgentCapture(context.Background(), ho.ID, 40)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Next != "done" || strings.Contains(strings.Join(captured.Argv, " "), " send ") {
		t.Fatalf("absent capture continuation = %+v", captured)
	}
	sent, err := service.AgentSend(context.Background(), ho.ID, "this must not reach bash")
	if err == nil || sent.Next != "done" || len(persist.sent) != 0 {
		t.Fatalf("absent send response=%+v err=%v sent=%v", sent, err, persist.sent)
	}
}

func TestSecurityGateCannotAcceptAgentSend(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-gate", HostID: "self", Persist: ports.PersistHandle{Kind: "tmux", Name: "gate"}, CreatedAt: now}
	ho := &Handoff{ID: "ho-gate", SessionID: sess.ID, HostID: "self", Kind: KindAgent, Status: StatusNeedsInput, DeliveryState: EffectBlocked, CreatedAt: now}
	_ = reg.PutSession(sess)
	_ = reg.PutHandoff(ho)
	persist := &agentPanePersistence{capture: "You are in /repo\nDo you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit\nPress enter to continue"}
	sessions := &SessionService{Reg: reg, Persist: persist, NewTransport: func(string) (ports.Transport, error) { return &fakeTransport{id: "self"}, nil }}
	service := &HandoffService{Reg: reg, Sessions: sessions}
	captured, err := service.AgentCapture(context.Background(), ho.ID, 40)
	if err != nil || captured.Next != "" || len(captured.Argv) != 0 {
		t.Fatalf("blocked capture advertised injection: %+v err=%v", captured, err)
	}
	sent, err := service.AgentSend(context.Background(), ho.ID, "approve")
	if err == nil || sent.Next != "" || len(persist.sent) != 0 {
		t.Fatalf("blocked gate accepted AgentSend: %+v err=%v sent=%v", sent, err, persist.sent)
	}
}

func TestMissingAgentSessionReturnsDoneContinuation(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	ho := &Handoff{ID: "ho-orphan", SessionID: "sess-gone", HostID: "self", Kind: KindAgent, Status: StatusRunning, CreatedAt: time.Now().UTC()}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	service := &HandoffService{Reg: reg, Sessions: &SessionService{Reg: reg}}

	captured, err := service.AgentCapture(context.Background(), ho.ID, 40)
	if err != nil || !captured.OK || captured.Next != "done" || strings.Join(captured.Argv, " ") != "relay agent done ho-orphan" {
		t.Fatalf("capture=%+v err=%v", captured, err)
	}
	sent, err := service.AgentSend(context.Background(), ho.ID, "must not send")
	if err == nil || sent.OK || sent.Next != "done" || strings.Join(sent.Argv, " ") != "relay agent done ho-orphan" {
		t.Fatalf("send=%+v err=%v", sent, err)
	}
}

func TestAgentRestartOptionsPreserveDurableGoalSpec(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	old := &Handoff{
		ID: "ho-old", SessionID: "sess-gone", HostID: "cancun", Kind: KindAgent,
		Status: StatusDone, Outcome: "done", Goal: "continue bounded work", Agent: "cursor-agent",
		Name: "folio-cycle", RepoRef: "/local/folio", RemoteCWD: "~/dev/folio",
		Container: "tools", NoPane: true, Silence: 75, EventsPath: "~/.local/state/relay/events/folio-cycle.jsonl",
		SourceSessionID: "sess-parent", SourceHostID: LocalHostID, SourcePersistName: "beholder-pdf-main",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := reg.PutHandoff(old); err != nil {
		t.Fatal(err)
	}
	opts, err := (&HandoffService{Reg: reg}).AgentRestartOptions(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Goal != old.Goal || opts.Agent != old.Agent || opts.HostID != old.HostID || opts.Name != old.Name || opts.RepoRef != old.RepoRef || opts.RemoteCWD != old.RemoteCWD || opts.Container != old.Container || !opts.NoPane || opts.Silence != old.Silence || opts.RestartedFromID != old.ID || opts.SourceSessionID != old.SourceSessionID {
		t.Fatalf("restart options lost durable state: %+v", opts)
	}
}

func TestAgentRestartOptionsUseLegacySessionAndAvoidNameCollision(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	sess := &Session{ID: "sess-old", HostID: "paris", RemoteCWD: "/data/engram", RepoRef: "/local/engram", Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"}, CreatedAt: now, UpdatedAt: now}
	old := &Handoff{ID: "ho-old", SessionID: sess.ID, HostID: "paris", Kind: KindAgent, Status: StatusDone, Outcome: "done", Goal: "goal", Agent: "codex", EventsPath: "~/.local/state/relay/events/engram.jsonl", CreatedAt: now, UpdatedAt: now}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(old); err != nil {
		t.Fatal(err)
	}
	opts, err := (&HandoffService{Reg: reg}).AgentRestartOptions(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if opts.RepoRef != sess.RepoRef || opts.RemoteCWD != sess.RemoteCWD || opts.Name != "" {
		t.Fatalf("legacy restart options=%+v", opts)
	}
}

func TestAgentRestartRejectsNonterminalGoal(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	if err := reg.PutHandoff(&Handoff{ID: "ho-live", HostID: "c3", Kind: KindAgent, Status: StatusRunning, Goal: "goal", Agent: "codex", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&HandoffService{Reg: reg}).AgentRestartOptions("ho-live"); err == nil || !strings.Contains(err.Error(), "finalize it before restart") {
		t.Fatalf("nonterminal restart error=%v", err)
	}
}
