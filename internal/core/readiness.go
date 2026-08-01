package core

import (
	"context"
	"strings"
)

// An agent can be launched, bound, and designated the apex while sitting at a
// login screen doing nothing. That state is indistinguishable from a working
// agent unless someone looks at the pane — which is how an apex ends up
// configured and inert, with escalations arriving into a prompt that will never
// answer them.
//
// Readiness classification makes that visible, and makes it impossible for an
// automation to "handle" a security gate by sending Enter at it: a gate is a
// terminal state that must be reported to the human, never answered on their
// behalf.

// AgentState is what an agent pane is actually doing.
type AgentState string

const (
	// AgentReady means the agent is running and past its gates.
	AgentReady AgentState = "ready"
	// AgentBlocked means it is waiting at an auth/trust/onboarding gate that
	// only the human may answer.
	AgentBlocked AgentState = "blocked"
	// AgentAbsent means no agent is running — typically a bare shell.
	AgentAbsent AgentState = "absent"
)

// AgentReadiness reports an agent pane's state and why.
type AgentReadiness struct {
	State  AgentState `json:"state"`
	Reason string     `json:"reason,omitempty"`
}

// securityGates are prompts that grant something. They must be surfaced to the
// human, never auto-answered — several of them hand an unattended agent broad
// authority (folder trust silently activates a repo's pre-approved tool
// permissions, which can include git push).
var securityGates = []struct{ marker, reason string }{
	{"select login method", "waiting for account login"},
	{"claude account with subscription", "waiting for account login"},
	{"anthropic console account", "waiting for account login"},
	{"sign in with", "waiting for account login"},
	{"do you trust", "waiting for folder-trust approval"},
	{"trust this folder", "waiting for folder-trust approval"},
	{"yes, i trust", "waiting for folder-trust approval"},
	{"pre-approves", "waiting for folder-trust approval (folder pre-approves tool permissions)"},
	{"is this a project you created or one you trust", "waiting for folder-trust approval"},
	{"choose the text style", "waiting for first-run theme selection"},
	{"dark mode (colorblind-friendly)", "waiting for first-run theme selection"},
	{"enter to confirm", "waiting at an interactive confirmation"},
}

// shellPrompts indicate a bare shell rather than a running agent.
var shellPrompts = []string{"$ ", "# ", "% "}

// ClassifyAgentPane decides what an agent pane is doing from its tail.
//
// Gate detection runs FIRST and wins: a pane showing a login or trust prompt is
// blocked even if a shell prompt is also visible in the scrollback, because
// treating it as merely "absent" would invite an automation to relaunch over it.
func ClassifyAgentPane(capture string) AgentReadiness {
	trimmed := strings.TrimRight(capture, " \t\n")
	if strings.TrimSpace(trimmed) == "" {
		return AgentReadiness{State: AgentAbsent, Reason: "pane is empty"}
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	tail := strings.ToLower(strings.Join(lines, "\n"))

	for _, gate := range securityGates {
		if strings.Contains(tail, gate.marker) {
			return AgentReadiness{State: AgentBlocked, Reason: gate.reason}
		}
	}

	// A trailing shell prompt with nothing after it means the agent exited or
	// was never started.
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = lines[i]
			break
		}
	}
	for _, p := range shellPrompts {
		if strings.HasSuffix(last, p) || strings.HasSuffix(strings.TrimRight(last, " "), strings.TrimSpace(p)) {
			return AgentReadiness{State: AgentAbsent, Reason: "pane is at a shell prompt; no agent running"}
		}
	}
	return AgentReadiness{State: AgentReady}
}

// AgentReadinessFor captures a session's pane and classifies it.
func (r *RootService) AgentReadinessFor(ctx context.Context, sessions *SessionService, sessionID string) AgentReadiness {
	if sessions == nil {
		return AgentReadiness{State: AgentAbsent, Reason: "no session service to inspect the pane"}
	}
	capture, err := sessions.Capture(ctx, sessionID, 40)
	if err != nil {
		return AgentReadiness{State: AgentAbsent, Reason: "could not capture the pane: " + err.Error()}
	}
	return ClassifyAgentPane(capture)
}
