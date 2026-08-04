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

func TestCursorPressAnyKeyLoginGateIsBlocked(t *testing.T) {
	got := ClassifyAgentPane(`
             Cursor Agent
             v2026.07.23-e383d2b
             Press any key to log in...`)
	if got.State != AgentBlocked || got.Gate == nil || got.Reason != "waiting for account login" {
		t.Fatalf("Cursor login gate must block, got %+v", got)
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

func TestRepeatedCodexTrustFramePreservesExactDecision(t *testing.T) {
	frame := `
  Welcome to Codex, OpenAI's command-line coding agent
> You are in /home/user/src/project
  Do you trust the contents of this directory?
› 1. Yes, continue
  2. No, quit
  Press enter to continue`
	got := ClassifyAgentPane(frame + frame)
	if got.State != AgentBlocked || got.Gate == nil {
		t.Fatalf("repeated gate = %+v", got)
	}
	if got.Gate.Directory != "/home/user/src/project" || len(got.Gate.Choices) != 2 {
		t.Fatalf("gate facts lost: %+v", got.Gate)
	}
	if got.Gate.Choices[0].Index != 1 || got.Gate.Choices[0].Label != "Yes, continue" || !got.Gate.Choices[0].Selected || got.Gate.Choices[1].Label != "No, quit" {
		t.Fatalf("gate choices changed: %+v", got.Gate.Choices)
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

func TestGateWordsInProseAreNotAuthorityTransitions(t *testing.T) {
	for _, prose := range []string{
		`The manager said: "do not rephrase permission_required messages." Held. I won't retry.`,
		`Confirmed: no permission is required and nothing should be approved.`,
		`The literal gate name is permission_required; this is documentation.`,
		`I quoted "Do you trust the contents of this directory?" but no prompt is active.`,
		"I quoted \"Do you trust the contents of this directory?\" in a report.\n1. first finding\n2. second finding",
		"> Do you trust the contents of this directory?\n1. first finding\n2. second finding",
		"> Select login method:\n1. first finding\n2. second finding",
		"> Run this command?\n1. first finding\n2. second finding",
		"Do you trust the contents of this directory?\n1. first finding\n2. second finding",
	} {
		if got := ClassifyAgentPane(prose); got.State != AgentReady {
			t.Fatalf("prose %q became an authority transition: %+v", prose, got)
		}
	}
}

func TestStrongStandaloneGateWithoutParsedChoicesStillBlocks(t *testing.T) {
	for _, capture := range []string{
		"Select login method:",
		"Do you trust the contents of this directory? [y/N]",
		"Enter to confirm · Esc to cancel",
	} {
		if got := ClassifyAgentPane(capture); got.State != AgentBlocked || got.Gate == nil {
			t.Fatalf("unparsed live gate %q = %+v", capture, got)
		}
	}
}

func TestToolPermissionDecisionSurfaceIsBlocked(t *testing.T) {
	got := ClassifyAgentPane("Run this command?\nNot in allowlist: git status\n→ Run (once) (y)\n  Add Shell(git status) to allowlist? (tab)\n  Skip & tell the agent what to do instead (esc or n)")
	if got.State != AgentBlocked || got.Gate == nil || len(got.Gate.Choices) != 3 {
		t.Fatalf("live tool gate = %+v", got)
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

// Stale gate text in scrollback must not mask a stopped agent. A live gate
// always leaves the cursor at its own prompt, so a trailing shell prompt means
// the gate is history and the agent is gone.
func TestStaleGateInScrollbackDoesNotMaskAStoppedAgent(t *testing.T) {
	got := ClassifyAgentPane(`
   ❯ 1. Yes, I trust this folder
     2. No, exit
   Enter to confirm · Esc to cancel
  Resume this session with:
  claude --resume 376af40c-7afa-45bd-bbc8-9c1b1b403903
dostos@Jingyu-Home:~/dev/dostos-workspace$ `)
	if got.State != AgentAbsent {
		t.Fatalf("a stopped agent must read as absent, not %+v", got)
	}
}
