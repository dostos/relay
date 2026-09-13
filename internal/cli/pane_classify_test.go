package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const blockedSample = `Accessing workspace: /home/jingyulee/dev/oqb
   Quick safety check: Is this a project you created or one you trust?
   ❯ 1. Yes, I trust this folder
     2. No, exit
`

func runClassify(t *testing.T, stdin string, argv ...string) (int, map[string]any) {
	t.Helper()
	a := New()
	a.Stdin = strings.NewReader(stdin)
	var code int
	out := captureStdout(t, func() { code = a.Run(append([]string{"pane", "classify"}, argv...)) })
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("pane classify must print JSON, got %q (%v)", out, err)
	}
	return code, resp
}

func TestPaneClassifyReadsStdinAndPrintsTheClassifierJSON(t *testing.T) {
	code, resp := runClassify(t, blockedSample)
	if code != 0 || resp["state"] != "blocked" {
		t.Fatalf("code=%d resp=%v", code, resp)
	}
	gate, _ := resp["gate"].(map[string]any)
	choices, _ := gate["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("gate choices must reach the caller verbatim: %v", resp)
	}
	if _, ok := resp["sampled_at"]; !ok {
		t.Fatalf("report must carry sampled_at: %v", resp)
	}
}

func TestPaneClassifyReadyAndEmpty(t *testing.T) {
	if code, resp := runClassify(t, "● Working…\n  editing foo.go\n"); code != 0 || resp["state"] != "ready" {
		t.Fatalf("ready: %d %v", code, resp)
	}
	code, resp := runClassify(t, "")
	if code != 0 || resp["state"] != "unknown" || resp["reason"] != "no text" {
		t.Fatalf("empty stdin must be unknown/no text with exit 0: %d %v", code, resp)
	}
	if code, _ := runClassify(t, blockedSample, "--lines", "12"); code != 0 {
		t.Fatalf("--lines is accepted: %d", code)
	}
}

func TestPaneOtherVerbsStayUnknownCommands(t *testing.T) {
	// The presenter verbs retired; only `classify` lives under `pane`.
	for _, argv := range [][]string{{"pane"}, {"pane", "list"}, {"pane", "present", "x"}} {
		a := New()
		a.JSON = true
		out := captureStdout(t, func() {
			if code := a.Run(argv); code == 0 {
				t.Fatalf("%v must not be a command", argv)
			}
		})
		if !strings.Contains(out, "unknown command") {
			t.Fatalf("%v: %q", argv, out)
		}
	}
}

func TestPaneClassifyStaysLocalInsteadOfTheBridge(t *testing.T) {
	if !commandNeedsLocalTTY([]string{"--json", "pane", "classify"}) {
		t.Fatal("pane classify reads stdin; it must never be forwarded over the bridge")
	}
	if commandNeedsLocalTTY([]string{"session", "readiness", "sess-1"}) {
		t.Fatal("session readiness captures over the wire; it may go through the bridge")
	}
}

func TestSessionListReadinessFlagsParse(t *testing.T) {
	on, lines, err := parseReadinessFlags([]string{"--readiness", "--readiness-lines", "12"})
	if err != nil || !on || lines != 12 {
		t.Fatalf("%v %v %v", on, lines, err)
	}
	if on, _, err := parseReadinessFlags(nil); err != nil || on {
		t.Fatalf("no flags: %v %v", on, err)
	}
	for _, bad := range [][]string{{"--bogus"}, {"--readiness-lines"}, {"--readiness-lines", "0"}, {"--readiness-lines", "x"}} {
		if _, _, err := parseReadinessFlags(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
