package core

import (
	"testing"

	"github.com/dostos/relay/internal/coord"
)

func TestChannelStreamNamespaced(t *testing.T) {
	if got := channelStream("teamX"); got != "chan.teamX" {
		t.Fatalf("channelStream = %q, want chan.teamX", got)
	}
}

func TestEnvelopeFromEvent(t *testing.T) {
	ev := coord.Event{Seq: 5, Kind: "result", TS: "t0", Meta: map[string]any{
		"from": "agentA", "text": "built ok", "pr": float64(42),
	}}
	m := envelopeFromEvent("teamX", ev)
	if m.Channel != "teamX" || m.Seq != 5 || m.Kind != "result" {
		t.Fatalf("bad envelope: %+v", m)
	}
	if m.From != "agentA" || m.Text != "built ok" {
		t.Fatalf("from/text not lifted: %+v", m)
	}
	if m.Meta["pr"] != float64(42) {
		t.Fatalf("extra meta lost: %+v", m.Meta)
	}
	if _, ok := m.Meta["from"]; ok {
		t.Fatal("from must be lifted OUT of meta, not duplicated")
	}
	if _, ok := m.Meta["text"]; ok {
		t.Fatal("text must be lifted OUT of meta, not duplicated")
	}
}

func TestDecideNextExplicitSignals(t *testing.T) {
	cases := []struct {
		kind     HandoffKind
		ev       string
		wantNext string
	}{
		{KindAgent, "ask", "send"},
		{KindJob, "ask", "send"}, // explicit ask is actionable even for jobs
		{KindAgent, "note", "wait"},
		{KindAgent, "progress", "wait"},
		{KindAgent, "result", "wait"},
		{KindAgent, "exit", "done"},
	}
	for _, c := range cases {
		if n, _ := DecideNext(c.kind, c.ev, false); n != c.wantNext {
			t.Fatalf("DecideNext(%v,%q)=%q want %q", c.kind, c.ev, n, c.wantNext)
		}
	}
}

func TestEventText(t *testing.T) {
	if got := eventText(&Event{Meta: map[string]any{"q": "which env?"}}); got != "which env?" {
		t.Fatalf("q lift = %q", got)
	}
	if got := eventText(&Event{Meta: map[string]any{"text": "hi"}}); got != "hi" {
		t.Fatalf("text lift = %q", got)
	}
	if eventText(nil) != "" || eventText(&Event{}) != "" {
		t.Fatal("empty cases must be \"\"")
	}
}
