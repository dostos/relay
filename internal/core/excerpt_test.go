package core

import (
	"strings"
	"testing"
)

// The escalation a manager actually received:
//
//	"...manager decide blocked/completed: └─────────┴──────────────┴────────┘ M..."
//
// Six lines of table frame, truncated mid-glyph. It told the manager nothing,
// so the manager ran `relay agent capture -n 260` and pulled 169 lines of diff
// into its context just to learn what was being asked. Every escalation paid
// that cost.
func TestDecisionExcerptSkipsTableFrames(t *testing.T) {
	capture := `
The IRS extraction diverges on capture 14 of 15: the extra row appears
only when the track predicate sees a denormalised float.

┌─────────┬────────────────────────────┬─────────────────────────────────┐
│ capture │ rust                       │ js                              │
├─────────┼────────────────────────────┼─────────────────────────────────┤
│ 14      │ 37 rows                    │ 36 rows                         │
└─────────┴────────────────────────────┴─────────────────────────────────┘
`
	got := decisionExcerpt(capture)
	if strings.ContainsAny(got, "┌┬┐├┼┤└┴┘│") {
		t.Fatalf("table frame must not reach the manager, got %q", got)
	}
	if !strings.Contains(got, "diverges on capture 14") {
		t.Fatalf("the prose above the table is the decision context, got %q", got)
	}
}

// Agent UIs end every screen with a spinner, a hint bar, and a composer. Those
// are the last lines on screen, so a naive tail always returns them.
func TestDecisionExcerptSkipsAgentChrome(t *testing.T) {
	capture := `
Blocked: the sealed Rust pin no longer matches composition.rs.

• Working (2s • esc to interrupt)
  … +169 lines (ctrl + t to view transcript)

› Improve documentation in @filename

  gpt-5.6-sol high · ~/dev/dostos-workspace · Main [default]
`
	got := decisionExcerpt(capture)
	for _, noise := range []string{"esc to interrupt", "ctrl + t", "Improve documentation", "gpt-5.6-sol"} {
		if strings.Contains(got, noise) {
			t.Fatalf("UI chrome %q must not reach the manager, got %q", noise, got)
		}
	}
	if !strings.Contains(got, "sealed Rust pin") {
		t.Fatalf("the actual blocker must survive, got %q", got)
	}
}

// If everything on screen is chrome there is no decision context to send.
// Returning frame characters would be worse than saying nothing: the caller
// has a generic fallback that at least does not mislead.
func TestDecisionExcerptIsEmptyWhenOnlyChromeIsOnScreen(t *testing.T) {
	capture := `
┌────────┬────────┐
└────────┴────────┘
• Working (2s • esc to interrupt)
› Improve documentation in @filename
`
	if got := decisionExcerpt(capture); got != "" {
		t.Fatalf("want no excerpt rather than noise, got %q", got)
	}
}

func TestDecisionExcerptKeepsOrdinaryProse(t *testing.T) {
	capture := "first line\nsecond line\nthird line\n"
	got := decisionExcerpt(capture)
	if !strings.Contains(got, "third line") || !strings.Contains(got, "first line") {
		t.Fatalf("plain output must pass through, got %q", got)
	}
}

// A prose line that merely mentions a pipe or dash is not chrome.
func TestDecisionExcerptKeepsProseContainingPunctuation(t *testing.T) {
	capture := "decide blocked | completed — the pin is stale\n"
	got := decisionExcerpt(capture)
	if !strings.Contains(got, "the pin is stale") {
		t.Fatalf("prose with punctuation must survive, got %q", got)
	}
}

// Chrome is per-agent, and the first version of this filter only knew Claude
// Code's. Run against a live Cursor pane it returned 269 characters of braille
// spinner, token counter, product tip, keybinding hint and status bar — no
// decision content at all. These are that pane's exact trailing lines.
func TestDecisionExcerptSkipsCursorChrome(t *testing.T) {
	capture := `
▎+ Does not alter default table claims. Does not change Auth A / v2.1
▎  seals.
▎ … truncated (164 more lines) · ctrl+r to review

⠀⠞ Editing  9.43k tokens
Tip: Try Cursor Grok 4.5 via /model, frontier intelligence at a fraction of
the cost.


→ Add a follow-up — /plan to review and build                   ctrl+c to stop


Cursor Grok 4.5 High Fast · 79.6% · 83 files edited             Run Everything
~/gh/dostos-workspace
`
	got := decisionExcerpt(capture)
	for _, noise := range []string{"⠀", "tokens", "Tip:", "the cost", "ctrl+", "truncated", "Run Everything", "~/gh"} {
		if strings.Contains(got, noise) {
			t.Errorf("chrome %q reached the manager: %q", noise, got)
		}
	}
	if !strings.Contains(got, "Does not alter default table claims") {
		t.Fatalf("the decidable statement must survive, got %q", got)
	}
	// The diff gutter is a rendering artefact, not content.
	if strings.Contains(got, "▎") {
		t.Errorf("gutter marker survived: %q", got)
	}
}
