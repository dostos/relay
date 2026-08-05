package core

import (
	"context"
	"regexp"
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
	// AgentUnknown means Relay could not verify the pane. It never authorizes
	// replacement or input because it may represent connectivity or trust loss.
	AgentUnknown AgentState = "unknown"
)

// AgentReadiness reports an agent pane's state and why.
type AgentReadiness struct {
	State  AgentState    `json:"state"`
	Reason string        `json:"reason,omitempty"`
	Gate   *SecurityGate `json:"gate,omitempty"`
}

type GateChoice struct {
	Index    int    `json:"index"`
	Label    string `json:"label"`
	Selected bool   `json:"selected,omitempty"`
}

// SecurityGate is the exact decision surface observed in the pane. This parser
// records facts, not policy; a downstream authority rule may choose only after
// adding lineage and workspace facts.
type SecurityGate struct {
	Reason            string       `json:"reason"`
	Directory         string       `json:"directory,omitempty"`
	DirectoryObserved bool         `json:"directory_observed,omitempty"`
	Subject           string       `json:"subject,omitempty"`
	Choices           []GateChoice `json:"choices,omitempty"`
}

var numberedGateChoice = regexp.MustCompile(`^\s*([›❯>]?)[[:space:]]*([0-9]+)\.[[:space:]]+(.+?)\s*$`)
var selectedGateChoice = regexp.MustCompile(`^\s*([›❯→])[[:space:]]+(.+?)\s*$`)
var shortcutGateChoice = regexp.MustCompile(`^\s*(.+?)[[:space:]]+\((?:tab|y|n|esc(?:[[:space:]]+or[[:space:]]+[a-z])?)\)\s*$`)

func parseSecurityGate(lines []string, reason string) *SecurityGate {
	gate := &SecurityGate{Reason: reason}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		content := strings.TrimSpace(strings.TrimLeft(trimmed, ">›❯"))
		lower := strings.ToLower(content)
		if strings.HasPrefix(lower, "you are in ") {
			gate.Directory = strings.TrimSpace(content[len("You are in "):])
			gate.DirectoryObserved = gate.Directory != ""
		} else if strings.HasPrefix(lower, "accessing workspace:") && i+1 < len(lines) {
			gate.Directory = strings.TrimSpace(lines[i+1])
			gate.DirectoryObserved = gate.Directory != ""
		}
		if match := numberedGateChoice.FindStringSubmatch(line); len(match) == 4 {
			idx := 0
			for _, r := range match[2] {
				idx = idx*10 + int(r-'0')
			}
			for _, existing := range gate.Choices {
				if existing.Index == idx {
					gate.Choices = nil // a repeated frame; retain only the newest one
					break
				}
			}
			gate.Choices = append(gate.Choices, GateChoice{Index: idx, Label: strings.TrimSpace(match[3]), Selected: match[1] != ""})
		} else if match := selectedGateChoice.FindStringSubmatch(line); len(match) == 3 {
			gate.Choices = append(gate.Choices, GateChoice{Index: len(gate.Choices) + 1, Label: strings.TrimSpace(match[2]), Selected: true})
		} else if match := shortcutGateChoice.FindStringSubmatch(line); len(match) == 2 {
			gate.Choices = append(gate.Choices, GateChoice{Index: len(gate.Choices) + 1, Label: strings.TrimSpace(match[1])})
		}
	}
	if reason == "waiting for tool-permission approval" {
		var subjects []string
		inBody := false
		for _, line := range lines {
			content := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), ">›❯→⚠"))
			lower := strings.ToLower(content)
			if strings.HasPrefix(lower, "run this command?") {
				inBody = true
				continue
			}
			if numberedGateChoice.MatchString(line) || selectedGateChoice.MatchString(line) || shortcutGateChoice.MatchString(line) {
				if inBody {
					break
				}
				continue
			}
			if lower == "" || strings.Trim(content, "─- ") == "" {
				continue
			}
			if inBody || strings.HasPrefix(lower, "not in allowlist:") || strings.HasPrefix(content, "$") {
				subjects = append(subjects, content)
			}
		}
		gate.Subject = strings.Join(subjects, " | ")
	}
	if gate.Subject == "" && gate.Directory != "" {
		gate.Subject = gate.Directory
	}
	return gate
}

func gateChoicesMatch(reason string, choices []GateChoice) bool {
	if len(choices) == 0 {
		return false
	}
	want := map[string][]string{
		"waiting for account login":             {"account", "subscription", "console", "sign in", "login"},
		"waiting for folder-trust approval":     {"trust", "yes", "no", "continue", "quit", "exit"},
		"waiting for first-run theme selection": {"mode", "light", "dark", "theme"},
		"waiting for tool-permission approval":  {"run", "allow", "skip", "deny"},
	}
	markers := want[reason]
	if len(markers) == 0 {
		return true
	}
	for _, choice := range choices {
		label := strings.ToLower(choice.Label)
		for _, marker := range markers {
			if strings.Contains(label, marker) {
				return true
			}
		}
	}
	return false
}

func promptLine(line, marker string) bool {
	content := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "›❯→⚠"))
	if match := numberedGateChoice.FindStringSubmatch(content); len(match) == 4 {
		content = match[3]
	}
	lower := strings.ToLower(strings.TrimSpace(content))
	if strings.HasPrefix(lower, marker) {
		return true
	}
	return marker == "pre-approves" && strings.Contains(lower, "folder pre-approves")
}

// securityGates are prompts that grant something. Classification records only
// the exact decision surface; policy is applied later with lineage and
// workspace facts that are not available here.
var securityGates = []struct{ marker, reason string }{
	{"select login method", "waiting for account login"},
	{"claude account with subscription", "waiting for account login"},
	{"anthropic console account", "waiting for account login"},
	{"sign in with", "waiting for account login"},
	{"press any key to log in", "waiting for account login"},
	{"do you trust", "waiting for folder-trust approval"},
	{"trust this folder", "waiting for folder-trust approval"},
	{"yes, i trust", "waiting for folder-trust approval"},
	{"pre-approves", "waiting for folder-trust approval (folder pre-approves tool permissions)"},
	{"is this a project you created or one you trust", "waiting for folder-trust approval"},
	{"choose the text style", "waiting for first-run theme selection"},
	{"dark mode (colorblind-friendly)", "waiting for first-run theme selection"},
	{"enter to confirm", "waiting at an interactive confirmation"},
	{"run this command?", "waiting for tool-permission approval"},
	{"not in allowlist", "waiting for tool-permission approval"},
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
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = lines[i]
			break
		}
	}

	// A trailing shell prompt decides first. A LIVE gate always leaves the
	// cursor at its own prompt, never back at a shell — so if the shell has the
	// last word, any gate text above it is scrollback from a prompt that has
	// already been answered or abandoned. Checking gates first made stale text
	// mask a stopped agent indefinitely.
	for _, p := range shellPrompts {
		if strings.HasSuffix(last, p) || strings.HasSuffix(strings.TrimRight(last, " "), strings.TrimSpace(p)) {
			return AgentReadiness{State: AgentAbsent, Reason: "pane is at a shell prompt; no agent running"}
		}
	}

	// Otherwise a gate still wins over anything else in the tail: treating a
	// pending security prompt as merely "absent" would invite an automation to
	// relaunch on top of a decision the human has not made. Gate type takes
	// precedence over line recency so a generic trailing "Enter to confirm"
	// cannot erase the specific login, trust, or theme decision above it.
	for _, gate := range securityGates {
		for i := len(lines) - 1; i >= 0; i-- {
			if !promptLine(lines[i], gate.marker) {
				continue
			}
			start := i
			for j := i - 1; j >= 0 && j >= i-4; j-- {
				contextLine := strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[j]), ">›❯→⚠")))
				if strings.HasPrefix(contextLine, "you are in ") || strings.HasPrefix(contextLine, "accessing workspace:") || strings.HasPrefix(contextLine, "$") {
					start = j
					break
				}
			}
			end := i + 10
			if end > len(lines) {
				end = len(lines)
			}
			parsed := parseSecurityGate(lines[start:end], gate.reason)
			if len(parsed.Choices) > 0 && !gateChoicesMatch(gate.reason, parsed.Choices) {
				parsed.Choices = nil
			}
			if len(parsed.Choices) == 0 && i != len(lines)-1 {
				// A strong prompt with no parseable choices is still blocked only
				// when it owns the active tail. The same sentence followed by agent
				// prose is a quotation, not a live decision surface.
				continue
			}
			return AgentReadiness{State: AgentBlocked, Reason: gate.reason, Gate: parsed}
		}
	}
	return AgentReadiness{State: AgentReady}
}

// AgentReadinessFor captures a session's pane and classifies it.
func (r *RootService) AgentReadinessFor(ctx context.Context, sessions *SessionService, sessionID string) AgentReadiness {
	if sessions == nil {
		return AgentReadiness{State: AgentUnknown, Reason: "no session service to inspect the pane"}
	}
	capture, err := sessions.Capture(ctx, sessionID, 40)
	if err != nil {
		return AgentReadiness{State: AgentUnknown, Reason: "could not capture the pane: " + err.Error()}
	}
	return ClassifyAgentPane(capture)
}
