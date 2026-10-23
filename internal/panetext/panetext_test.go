package panetext

import "testing"

func TestComposerDraft(t *testing.T) {
	cases := []struct {
		name, screen, want string
		found              bool
	}{
		{"codex empty", "done.\n\n› \n\n  gpt-5.6-sol default · ~/dev\n", "", true},
		{"codex dim placeholder", "done.\n\n› \x1b[2mImplement {feature}\x1b[22m\n\n  \x1b[2m? for shortcuts\x1b[0m\n", "", true},
		{"codex placeholder plain", "done.\n\n› Ask Codex to do anything\n\n  100% context left\n", "", true},
		{"grey 256 placeholder", "> \x1b[38;5;244mTry \"fix lint\"\x1b[39m\n", "", true},
		{"human draft", "done.\n\n› half-written thought about the API\n\n  gpt-5.6-sol default · ~/dev\n", "half-written thought about the API", true},
		{"multiline draft", "› first line\n  second line\n\n  ? for shortcuts\n", "first line\nsecond line", true},
		{"claude box empty", "───────\n> \n───────\n  ? for shortcuts\n", "", true},
		{"claude box draft", "───────\n> 리뷰 부탁해\n───────\n  ? for shortcuts\n", "리뷰 부탁해", true},
		{"transcript glyph then output", "> old prompt\nanswer text\n", "", false},
		{"no composer", "$ ls\nfoo\n$ ", "", false},
	}
	for _, tc := range cases {
		got, found := ComposerDraft(tc.screen)
		if got != tc.want || found != tc.found {
			t.Errorf("%s: got %q found=%v, want %q found=%v", tc.name, got, found, tc.want, tc.found)
		}
	}
}

func TestTailWorkingLines(t *testing.T) {
	screen := "• Working (3s • esc to interrupt)\nold\n" + "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n› \n"
	if got := TailWorkingLines(screen, 8); len(got) != 0 {
		t.Fatalf("scrollback working line counted as live: %v", got)
	}
	live := "answer\n• Working (3s • esc to interrupt)\n\n› \n  ? for shortcuts\n"
	if got := TailWorkingLines(live, 8); len(got) != 1 {
		t.Fatalf("live working line missed: %v", got)
	}
}

func TestLiveWorkingLines(t *testing.T) {
	ended := "› hello\n• Working (3s • esc to interrupt)\nDone: hello\n› \n"
	if got := LiveWorkingLines(ended, 8); got != nil {
		t.Fatalf("ended turn still live: %v", got)
	}
	live := "› hello\n• Working (3s • esc to interrupt)\n  ↳ queued thing\n\n› \n  ? for shortcuts\n"
	if got := LiveWorkingLines(live, 8); len(got) != 1 {
		t.Fatalf("live turn missed: %v", got)
	}
	claude := "✻ Thinking… (12s · esc to interrupt)\n  ⎿  Tip: use /clear\n────────\n> \n────────\n"
	if got := LiveWorkingLines(claude, 8); len(got) != 1 {
		t.Fatalf("claude live turn missed: %v", got)
	}
}
