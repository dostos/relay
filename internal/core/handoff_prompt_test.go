package core

import (
	"strings"
	"testing"
)

func TestAgentGoalPromptExposesCompactProviderNeutralSignal(t *testing.T) {
	goal := "fix the parser"
	prompt := agentGoalPrompt(goal)
	if !strings.Contains(prompt, "relay signal ask") || !strings.Contains(prompt, "permission/result/exit") {
		t.Fatalf("missing child protocol: %q", prompt)
	}
	if !strings.HasSuffix(prompt, goal) {
		t.Fatalf("goal was not preserved: %q", prompt)
	}
	if len(prompt)-len(goal) > 130 {
		t.Fatalf("child protocol preamble is too large: %d bytes", len(prompt)-len(goal))
	}
}
