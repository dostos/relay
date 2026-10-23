// Package panetext holds the pure text heuristics Relay applies to a captured
// agent pane: is a turn running, and what is sitting in the input composer.
// Both the tmux adapter (send receipts) and core (readiness, wait --idle)
// read panes, so the rules live here once.
package panetext

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// workingNeedles mark a live agent turn in Codex and Claude Code panes.
var workingNeedles = []string{
	"esc to interrupt",
	"• working",
	"● working",
	"working (",
	"working…",
	"working...",
}

// IsWorkingLine reports whether one line shows an agent turn in progress.
func IsWorkingLine(line string) bool {
	lower := strings.ToLower(line)
	for _, needle := range workingNeedles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// WorkingLines returns every working-indicator line in screen, trimmed.
func WorkingLines(screen string) []string {
	var out []string
	for _, line := range strings.Split(screen, "\n") {
		if IsWorkingLine(line) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// TailWorkingLines returns the working-indicator lines among the last n
// non-blank lines. A live turn's status line sits just above the composer;
// a working line further up is transcript scrollback.
func TailWorkingLines(screen string, n int) []string {
	lines := strings.Split(strings.TrimRight(screen, " \t\n"), "\n")
	var tail []string
	for i := len(lines) - 1; i >= 0 && len(tail) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			tail = append(tail, lines[i])
		}
	}
	var out []string
	for i := len(tail) - 1; i >= 0; i-- {
		if IsWorkingLine(tail[i]) {
			out = append(out, strings.TrimSpace(tail[i]))
		}
	}
	return out
}

// LiveWorkingLines is TailWorkingLines restricted to a status line that is
// still the live one: below the last working line there may only be blank
// lines, rules, the composer and indented lines (queued input, tips,
// footers). Unindented output below it means the turn ended and the line is
// transcript.
func LiveWorkingLines(screen string, n int) []string {
	tail := TailWorkingLines(screen, n)
	if len(tail) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimRight(screen, " \t\n"), "\n")
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if IsWorkingLine(lines[i]) {
			last = i
			break
		}
	}
	for _, l := range lines[last+1:] {
		t := strings.TrimSpace(l)
		if t == "" || IsRuleLine(t) || IsComposerGlyphLine(l) {
			continue
		}
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") || strings.HasPrefix(t, "↳") || strings.HasPrefix(t, "⎿") || strings.HasPrefix(t, "│") {
			continue
		}
		return nil
	}
	return tail
}

var elapsedRE = regexp.MustCompile(`\((?:(\d+)h\s*)?(?:(\d+)m\s*)?(\d+)s\b`)

// ElapsedSeconds parses a turn timer such as "(1m 05s" from a working line.
func ElapsedSeconds(line string) (int, bool) {
	m := elapsedRE.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	s, _ := strconv.Atoi(m[3])
	return h*3600 + mi*60 + s, true
}

// IsComposerGlyphLine reports a line that starts with an input prompt glyph.
func IsComposerGlyphLine(line string) bool {
	t := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "│┃|"))
	return strings.HasPrefix(t, "›") || strings.HasPrefix(t, ">") || strings.HasPrefix(t, "❯")
}

// IsRuleLine reports a horizontal rule (Claude Code draws its input box with them).
func IsRuleLine(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && strings.Trim(s, "─━═-╌┄╭╮╰╯│") == ""
}

// footerNeedles identify status/footer lines TUIs print under the composer.
// They are indented like a continuation line but are never draft text.
var footerNeedles = []string{
	"for shortcuts", "context left", "esc to", "⏎", "tokens", "auto-accept",
	"bypass permissions", "plan mode", "ctrl+", "⌃", "shift+tab", "% context",
	"newline", "· ~/", " default · ",
}

func isFooterLine(s string) bool {
	lower := strings.ToLower(s)
	for _, n := range footerNeedles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// placeholderDrafts are the hint texts idle composers show. Without styling
// (plain capture) they look like typed text, so they never count as a draft.
var placeholderDrafts = []string{
	"ask codex to do anything", "implement {feature}", "find and fix a bug in @filename",
	"explain this codebase", "summarize recent commits", "write tests for @filename",
	"improve documentation in @filename", "run /review on my current changes",
	"use /skills to list available skills", "try \"", "type your message",
	"plan, search, build anything", "add a follow-up",
}

func isPlaceholder(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	for _, p := range placeholderDrafts {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// ComposerDraft returns the text a human (or an earlier send) left in the
// pane's input composer. styled is a capture taken with escape sequences
// (tmux capture-pane -e); faint/grey runs are placeholder hints and are
// dropped. found is false when no composer is recognizable, in which case the
// caller must not infer anything.
//
// The composer is the last prompt-glyph line plus its indented continuation
// lines; unindented output below the glyph means that line was a submitted
// transcript entry, not the live input.
func ComposerDraft(styled string) (draft string, found bool) {
	raw := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	plain := make([]string, len(raw))
	for i, l := range raw {
		plain[i] = StripANSI(l)
	}
	idx := -1
	for i, l := range plain {
		if IsComposerGlyphLine(l) {
			idx = i
		}
	}
	if idx < 0 {
		return "", false
	}
	block := []string{visibleText(raw[idx])}
	for i := idx + 1; i < len(plain); i++ {
		p := plain[i]
		trimmed := strings.TrimSpace(p)
		if trimmed == "" || IsRuleLine(trimmed) {
			continue
		}
		if !strings.HasPrefix(p, " ") && !strings.HasPrefix(p, "\t") && !strings.HasPrefix(p, "│") {
			return "", false
		}
		if isFooterLine(trimmed) {
			continue
		}
		block = append(block, visibleText(raw[i]))
	}
	first := strings.TrimSpace(strings.Trim(strings.TrimSpace(block[0]), "│┃|"))
	for _, g := range []string{"›", "❯", ">"} {
		if strings.HasPrefix(first, g) {
			first = strings.TrimSpace(strings.TrimPrefix(first, g))
			break
		}
	}
	block[0] = first
	var kept []string
	for _, l := range block {
		l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│┃"))
		if l == "" {
			continue
		}
		kept = append(kept, l)
	}
	draft = strings.TrimSpace(strings.Join(kept, "\n"))
	if isPlaceholder(draft) {
		return "", true
	}
	return draft, true
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]`)

// StripANSI removes terminal escape sequences.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// visibleText drops runs rendered faint (SGR 2), italic-dim hints, or in a
// grey foreground, which is how Codex and Claude Code draw placeholders.
func visibleText(s string) string {
	var b strings.Builder
	dim := false
	grey := false
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			loc := ansiRE.FindStringIndex(s[i:])
			if loc != nil && loc[0] == 0 {
				seq := s[i : i+loc[1]]
				if strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") {
					dim, grey = applySGR(seq[2:len(seq)-1], dim, grey)
				}
				i += loc[1]
				continue
			}
		}
		r, size := rune(s[i]), 1
		if s[i] >= 0x80 {
			for _, rr := range s[i:] {
				r = rr
				break
			}
			size = len(string(r))
		}
		if !dim && !grey {
			b.WriteRune(r)
		} else if unicode.IsSpace(r) {
			b.WriteRune(' ')
		}
		i += size
	}
	return b.String()
}

func applySGR(params string, dim, grey bool) (bool, bool) {
	if params == "" {
		return false, false
	}
	parts := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "0":
			dim, grey = false, false
		case "2":
			dim = true
		case "22":
			dim = false
		case "90":
			grey = true
		case "39":
			grey = false
		case "30", "31", "32", "33", "34", "35", "36", "37", "91", "92", "93", "94", "95", "96", "97":
			grey = false
		case "38":
			if i+2 < len(parts) && parts[i+1] == "5" {
				n, _ := strconv.Atoi(parts[i+2])
				grey = n == 8 || (n >= 236 && n <= 250)
				i += 2
			} else if i+4 < len(parts) && parts[i+1] == "2" {
				r, _ := strconv.Atoi(parts[i+2])
				g, _ := strconv.Atoi(parts[i+3])
				bl, _ := strconv.Atoi(parts[i+4])
				grey = abs(r-g) < 12 && abs(g-bl) < 12 && r < 170 && r > 40
				i += 4
			}
		}
	}
	return dim, grey
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
