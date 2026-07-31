package core

import (
	"encoding/json"
	"strings"
	"testing"
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
