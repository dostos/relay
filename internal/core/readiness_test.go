package core

import "testing"

// These are verbatim tails from a real conductor launch. Each one is a gate
// that grants something, so each must classify as blocked — never as ready
// (which would hide an inert apex) and never as absent (which would invite an
// automation to relaunch over a pending security prompt).
func TestLoginGateIsBlocked(t *testing.T) {
	got := ClassifyAgentPane(`
   Claude Code can be used with your Claude subscription or billed based on API
   Select login method:
   ❯ 1. Claude account with subscription · Pro, Max, Team, or Enterprise
     2. Anthropic Console account · API usage billing`)
	if got.State != AgentBlocked {
		t.Fatalf("login gate must block, got %+v", got)
	}
	if got.Reason == "" {
		t.Fatal("a blocked agent must say why")
	}
}

func TestFolderTrustGateIsBlocked(t *testing.T) {
	got := ClassifyAgentPane(`
   Accessing workspace:
   /home/dostos/dev/dostos-workspace
   ⚠ This folder pre-approves 7 tool permissions in .claude/settings.local.json:
     Bash(git push:*), Bash(git commit:*)
   ❯ 1. Yes, I trust this folder
     2. No, exit
   Enter to confirm · Esc to cancel`)
	if got.State != AgentBlocked {
		t.Fatalf("folder trust must block, got %+v", got)
	}
	// The reason should name the actual hazard, since this gate silently
	// activates a repo's pre-approved tool permissions.
	if got.Reason == "" {
		t.Fatal("trust gate must explain what it grants")
	}
}

func TestThemePickerIsBlocked(t *testing.T) {
	got := ClassifyAgentPane(`
     3. Light mode
     4. Dark mode (colorblind-friendly)
   Syntax theme: Monokai Extended (ctrl+t to disable)`)
	if got.State != AgentBlocked {
		t.Fatalf("first-run theme picker must block, got %+v", got)
	}
}

func TestBareShellIsAbsent(t *testing.T) {
	got := ClassifyAgentPane("dostos@Jingyu-Home:~/dev/dostos-workspace$ ")
	if got.State != AgentAbsent {
		t.Fatalf("a bare shell means no agent, got %+v", got)
	}
}

func TestEmptyPaneIsAbsent(t *testing.T) {
	if got := ClassifyAgentPane("   \n\n"); got.State != AgentAbsent {
		t.Fatalf("empty pane must be absent, got %+v", got)
	}
}

func TestRunningAgentIsReady(t *testing.T) {
	got := ClassifyAgentPane(`
> I'll review the escalation from beholder-pdf and apply the project rules.

  ⏺ Reading ~/.config/relay/rules/beholder-pdf.md
  ✓ Rule matched: auto-approve eval-flywheel steps`)
	if got.State != AgentReady {
		t.Fatalf("a working agent must be ready, got %+v", got)
	}
}

// A gate wins over a shell prompt in the scrollback: classifying this as
// "absent" would invite a relaunch on top of a pending security decision.
func TestGateWinsOverShellPromptInScrollback(t *testing.T) {
	got := ClassifyAgentPane(`
dostos@Jingyu-Home:~$ claude
   Select login method:
   ❯ 1. Claude account with subscription`)
	if got.State != AgentBlocked {
		t.Fatalf("a pending gate must win over scrollback, got %+v", got)
	}
}
