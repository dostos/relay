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
	// The real gate: inside a relay pane RELAY_BRIDGE_SOCK is set, and the
	// bridge request carries argv only — a forwarded classify would read no
	// stdin at all. forwardThroughDesktopBridge must decline before it even
	// looks for a socket.
	t.Setenv("RELAY_BRIDGE_SOCK", "/nonexistent/desktop-bridge.sock")
	t.Setenv("RELAY_SESSION_ID", "sess-x")
	a := &App{}
	for _, argv := range [][]string{{"pane", "classify"}, {"--json", "pane", "classify"}} {
		if code, forwarded := a.forwardThroughDesktopBridge(argv); forwarded || code != 0 {
			t.Fatalf("%v was forwarded (code %d) with the bridge socket set", argv, code)
		}
	}
}

func TestPaneClassifyShapeIsStableAcrossStates(t *testing.T) {
	// One shape for every state: a presenter must find reason and gate keys
	// on a ready pane too (null gate), not probe for their presence.
	for _, text := range []string{"$ \n", "", blockedSample} {
		_, resp := runClassify(t, text)
		for _, key := range []string{"state", "reason", "gate", "lines", "sampled_at"} {
			if _, ok := resp[key]; !ok {
				t.Fatalf("missing %q for %q: %v", key, text, resp)
			}
		}
	}
}
