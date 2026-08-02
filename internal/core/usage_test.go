package core

import (
	"context"
	"testing"
)

func hint(m map[string]int) UsageHint { return UsageHint{Weekly: m} }

func TestSelectAgent(t *testing.T) {
	cands := []string{"cursor-agent", "claude", "codex"}
	tests := []struct {
		name      string
		cands     []string
		preferred string
		hint      UsageHint
		min       int
		want      string
	}{
		{
			name: "preferred with headroom wins", cands: cands, preferred: "cursor-agent",
			hint: hint(map[string]int{"cursor-agent": 40, "claude": 90, "codex": 10}), min: 5,
			want: "cursor-agent",
		},
		{
			name: "exhausted preferred yields to highest headroom", cands: cands, preferred: "cursor-agent",
			hint: hint(map[string]int{"cursor-agent": 0, "claude": 80, "codex": 30}), min: 5,
			want: "claude",
		},
		{
			name: "no usage data keeps preferred", cands: cands, preferred: "codex",
			hint: hint(nil), min: 5,
			want: "codex",
		},
		{
			name: "unknown preferred usage keeps preferred", cands: cands, preferred: "claude",
			hint: hint(map[string]int{"codex": 50}), min: 5, // claude unknown
			want: "claude",
		},
		{
			name: "all below threshold falls back to preferred", cands: cands, preferred: "codex",
			hint: hint(map[string]int{"cursor-agent": 2, "claude": 1, "codex": 0}), min: 5,
			want: "codex",
		},
		{
			name: "preferred not a candidate picks highest", cands: cands, preferred: "ccs:hcs",
			hint: hint(map[string]int{"cursor-agent": 20, "claude": 70, "codex": 60}), min: 5,
			want: "claude",
		},
		{
			name: "empty candidates yields empty", cands: nil, preferred: "claude",
			hint: hint(nil), min: 5, want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ranks := SelectAgent(tc.cands, tc.preferred, tc.hint, tc.min)
			if got != tc.want {
				t.Fatalf("pick = %q, want %q", got, tc.want)
			}
			if tc.want != "" {
				var chosen string
				for _, r := range ranks {
					if r.Chosen {
						chosen = r.Agent
					}
				}
				if chosen != tc.want {
					t.Fatalf("ranking chosen = %q, want %q", chosen, tc.want)
				}
			}
		})
	}
}

func TestSuggestUsesExhaustedLaunchProfileWithSharedQuota(t *testing.T) {
	profile := &HostProfile{
		Agents: []AgentSpec{
			{Name: "grok-fast", Command: "cursor-agent", Args: []string{"--model", "cursor-grok-4.5-high-fast"}, UsageKey: "cursor"},
			{Name: "auto", Command: "cursor-agent", Args: []string{"--model", "auto"}, UsageKey: "cursor"},
		},
		Defaults: HostDefaults{PreferredAgent: "grok-fast", ExhaustedAgent: "auto", UsageMinRemaining: 5},
	}
	pick, ranks := selectAgent(candidateAgents(profile), profile.Defaults.PreferredAgent, profile.Defaults.ExhaustedAgent, hint(map[string]int{"cursor": 0}), 5, map[string]string{"grok-fast": "cursor", "auto": "cursor"})
	if pick != "auto" {
		t.Fatalf("pick = %q, want auto; ranks=%+v", pick, ranks)
	}
	if !ranks[1].Chosen || !ranks[1].Exhausted || ranks[1].UsageKey != "cursor" {
		t.Fatalf("fallback rank = %+v", ranks[1])
	}
}

func TestSuggestKeepsPreferredWhenUsageIsUnknown(t *testing.T) {
	profile := &HostProfile{
		Agents:   []AgentSpec{{Name: "grok-fast", UsageKey: "cursor"}, {Name: "auto", UsageKey: "cursor"}},
		Defaults: HostDefaults{PreferredAgent: "grok-fast", ExhaustedAgent: "auto", UsageMinRemaining: 5},
	}
	pick, _ := selectAgent(candidateAgents(profile), "grok-fast", "auto", UsageHint{}, 5, map[string]string{"grok-fast": "cursor", "auto": "cursor"})
	if pick != "grok-fast" {
		t.Fatalf("unknown telemetry pick = %q, want grok-fast", pick)
	}
}

func TestLoadUsageHint(t *testing.T) {
	ctx := context.Background()

	t.Run("valid json", func(t *testing.T) {
		h, ok := LoadUsageHint(ctx, `echo '{"agents":{"cursor-agent":{"weekly_remaining":42},"codex":{"weekly_remaining":0}}}'`)
		if !ok {
			t.Fatal("want ok=true")
		}
		if v, known := h.Remaining("cursor-agent"); !known || v != 42 {
			t.Fatalf("cursor-agent = (%d,%v), want (42,true)", v, known)
		}
		if v, known := h.Remaining("codex"); !known || v != 0 {
			t.Fatalf("codex = (%d,%v), want (0,true)", v, known)
		}
		if _, known := h.Remaining("claude"); known {
			t.Fatal("claude should be unknown")
		}
	})

	t.Run("empty hook disabled", func(t *testing.T) {
		if _, ok := LoadUsageHint(ctx, "   "); ok {
			t.Fatal("empty hook must be ok=false")
		}
	})

	t.Run("malformed json ignored", func(t *testing.T) {
		if _, ok := LoadUsageHint(ctx, `echo 'not json'`); ok {
			t.Fatal("malformed hook must be ok=false")
		}
	})

	t.Run("nonzero exit ignored", func(t *testing.T) {
		if _, ok := LoadUsageHint(ctx, `exit 3`); ok {
			t.Fatal("failing hook must be ok=false")
		}
	})

	t.Run("empty agents ignored", func(t *testing.T) {
		if _, ok := LoadUsageHint(ctx, `echo '{"agents":{}}'`); ok {
			t.Fatal("no rows must be ok=false")
		}
	})
}

func TestCandidateAgents(t *testing.T) {
	p := &HostProfile{
		Agents: []AgentSpec{{Name: "cursor-agent"}, {Name: "claude"}, {Name: "codex"}},
		Probe: map[string]ProbeResult{
			"claude": {Present: false}, // not present → dropped
			"codex":  {Present: true},
		},
	}
	got := candidateAgents(p)
	want := []string{"cursor-agent", "codex"} // claude dropped, cursor-agent has no probe (kept)
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}
