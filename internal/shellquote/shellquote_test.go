package shellquote

import "testing"

func TestValidateSessionName(t *testing.T) {
	ok := []string{"sess-1", "dostos-workspace-abc", "a", "A9._-"}
	for _, s := range ok {
		if err := ValidateSessionName(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	bad := []string{"", "../etc", "foo/bar", "x;rm", "$(hi)", "a'b", "-bad"}
	for _, s := range bad {
		if err := ValidateSessionName(s); err == nil {
			t.Fatalf("expected reject %q", s)
		}
	}
}

func TestValidateEventKind(t *testing.T) {
	ok := []string{"exit", "idle", "started", "a1._-"}
	for _, s := range ok {
		if err := ValidateEventKind(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	bad := []string{"", "x;rm", "$(hi)", "-bad", "a b"}
	for _, s := range bad {
		if err := ValidateEventKind(s); err == nil {
			t.Fatalf("expected reject %q", s)
		}
	}
}

func TestPathExprTildeSafe(t *testing.T) {
	got, err := PathExpr(`~/$(rm -rf ~)`)
	if err != nil {
		t.Fatal(err)
	}
	// Must single-quote the payload so $() does not execute.
	want := `"$HOME"'/$(rm -rf ~)'`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
