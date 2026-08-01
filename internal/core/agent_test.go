package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

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
