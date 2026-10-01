package agentwait

import (
	"encoding/json"
	"testing"
)

func TestDecideNextMatrix(t *testing.T) {
	cases := []struct {
		kind     Kind
		ev       string
		timedOut bool
		want     string
	}{
		{KindJob, "exit", false, "done"},
		{KindAgent, "exit", false, "done"},
		{KindJob, "idle", false, "wait"},
		{KindAgent, "idle", false, "send"},
		{KindJob, "needs_input", false, "escalate"},
		{KindAgent, "needs_input", false, "send"},
		{KindAgent, "permission_required", false, "send"},
		{KindAgent, "ask", false, "send"},
		{KindJob, "ask", false, "send"},
		{KindJob, "started", false, "wait"},
		{KindAgent, "progress", false, "wait"},
		{KindAgent, "note", false, "wait"},
		{KindAgent, "result", false, "wait"},
		{KindAgent, "", true, "wait"},
		{KindJob, "", true, "wait"},
	}
	for _, tc := range cases {
		got := DecideNext(tc.kind, tc.ev, tc.timedOut)
		if got != tc.want {
			t.Fatalf("kind=%s ev=%q timeout=%v got=%s want=%s", tc.kind, tc.ev, tc.timedOut, got, tc.want)
		}
	}
}

func TestShouldWakeOnNeedsInput(t *testing.T) {
	// #49 correctness: needs_input (and any next=send state) must wake wait.
	for _, ev := range []string{"needs_input", "permission_required", "ask", "idle"} {
		if !ShouldWake(KindAgent, UntilEvent, ev, nil) {
			t.Fatalf("UntilEvent must wake on %q (next=%s)", ev, DecideNext(KindAgent, ev, false))
		}
		if !ShouldWake(KindAgent, UntilNeedsInput, ev, nil) {
			t.Fatalf("UntilNeedsInput must wake on %q", ev)
		}
	}
	// Job idle stays telemetry; job needs_input escalates.
	if ShouldWake(KindJob, UntilEvent, "idle", nil) {
		t.Fatal("job idle must not wake UntilEvent")
	}
	if !ShouldWake(KindJob, UntilEvent, "needs_input", nil) {
		t.Fatal("job needs_input must wake UntilEvent")
	}
}

func TestShouldWakeRejectsOldManagerOnlyPolicy(t *testing.T) {
	// Regression for the 19869fd change that wired AgentWait to
	// eventWakesManager and dropped agent idle — the deadlock in #49.
	if eventWakesManager("idle", nil) {
		t.Fatal("eventWakesManager still skips idle (delivery boundary)")
	}
	if !ShouldWake(KindAgent, UntilEvent, "idle", nil) {
		t.Fatal("ShouldWake must wake on agent idle even though eventWakesManager does not")
	}
	status := StatusAfterEvent(KindAgent, StatusRunning, "idle")
	if status != StatusNeedsInput {
		t.Fatalf("agent idle must project status needs_input, got %s", status)
	}
	if DecideNext(KindAgent, "idle", false) != "send" {
		t.Fatal("agent idle next must be send")
	}
}

func TestUntilModes(t *testing.T) {
	// Bare event wait wakes on send-next and on non-hook result (re-arm).
	if !ShouldWake(KindAgent, UntilEvent, "result", map[string]any{"source": "milestone"}) {
		t.Fatal("UntilEvent should wake on non-hook result so the caller can re-arm")
	}
	if ShouldWake(KindAgent, UntilEvent, "result", map[string]any{"source": "hook"}) {
		t.Fatal("hook result is a receipt, not a wake")
	}
	if ShouldWake(KindAgent, UntilEvent, "progress", nil) {
		t.Fatal("progress must not wake UntilEvent")
	}

	// needs-input ignores interim result wakes.
	if ShouldWake(KindAgent, UntilNeedsInput, "result", map[string]any{"source": "milestone"}) {
		t.Fatal("UntilNeedsInput must not wake on interim result")
	}
	if !ShouldWake(KindAgent, UntilNeedsInput, "needs_input", nil) {
		t.Fatal("UntilNeedsInput must wake on needs_input")
	}
	if !ShouldWake(KindAgent, UntilNeedsInput, "exit", nil) {
		t.Fatal("UntilNeedsInput must also wake on terminal exit")
	}

	// terminal only cares about exit/done.
	if ShouldWake(KindAgent, UntilTerminal, "needs_input", nil) {
		t.Fatal("UntilTerminal must not wake on needs_input")
	}
	if !ShouldWake(KindAgent, UntilTerminal, "exit", nil) {
		t.Fatal("UntilTerminal must wake on exit")
	}
}

func TestEnvelopeAlwaysNonEmptyJSON(t *testing.T) {
	// #49: interim wake must never yield empty stdout. Simulate the seq=1
	// started→result path that previously looked like "done" to a backgrounded
	// parent because nothing was printed.
	events := []struct {
		kind string
		meta map[string]any
	}{
		{"started", nil},
		{"progress", map[string]any{"text": "halfway"}},
		{"result", map[string]any{"source": "milestone", "text": "checkpoint"}},
		{"idle", nil},
		{"needs_input", nil},
		{"exit", nil},
	}
	from := int64(0)
	status := StatusRunning
	var wakes []Envelope
	for i, ev := range events {
		seq := int64(i + 1)
		status = StatusAfterEvent(KindAgent, status, ev.kind)
		if !ShouldWake(KindAgent, UntilEvent, ev.kind, ev.meta) {
			continue
		}
		next := DecideNext(KindAgent, ev.kind, false)
		env := BuildEnvelope("ho-test", status, from, seq, next, ev.kind, false, UntilEvent)
		raw, err := MarshalEnvelope(env)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
			t.Fatalf("empty wake stdout at seq %d: %q", seq, raw)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"ok", "handoff_id", "status", "from_seq", "last_seq", "next", "argv"} {
			if _, ok := decoded[key]; !ok {
				t.Fatalf("wake envelope missing %q: %s", key, raw)
			}
		}
		wakes = append(wakes, env)
		from = seq
	}
	if len(wakes) < 2 {
		t.Fatalf("expected interim + needs_input/exit wakes, got %#v", wakes)
	}
	// First wake is the interim result (next=wait) — must still carry argv re-arm.
	if wakes[0].Next != "wait" || len(wakes[0].Argv) < 6 {
		t.Fatalf("interim wake must re-arm wait, got %#v", wakes[0])
	}
	if wakes[0].Argv[4] != "--from" || wakes[0].Argv[5] != "3" {
		t.Fatalf("interim re-arm --from want 3, got %#v", wakes[0].Argv)
	}
	// A later wake is needs_input / idle with next=send.
	foundSend := false
	for _, w := range wakes {
		if w.Next == "send" && (w.EventKind == "idle" || w.EventKind == "needs_input") {
			foundSend = true
			if w.Status != string(StatusNeedsInput) {
				t.Fatalf("send wake status=%s want needs_input", w.Status)
			}
		}
	}
	if !foundSend {
		t.Fatalf("expected a needs_input/idle send wake, got %#v", wakes)
	}
}

func TestUntilNeedsInputSkipsInterimAndWakesOnIdle(t *testing.T) {
	from := int64(0)
	status := StatusRunning
	var woke *Envelope
	for i, ev := range []string{"started", "progress", "result", "idle"} {
		seq := int64(i + 1)
		status = StatusAfterEvent(KindAgent, status, ev)
		meta := map[string]any{}
		if ev == "result" {
			meta["source"] = "milestone"
		}
		if !ShouldWake(KindAgent, UntilNeedsInput, ev, meta) {
			continue
		}
		next := DecideNext(KindAgent, ev, false)
		env := BuildEnvelope("ho-child", status, from, seq, next, ev, false, UntilNeedsInput)
		woke = &env
		break
	}
	if woke == nil {
		t.Fatal("UntilNeedsInput must wake when child goes idle/needs_input")
	}
	if woke.EventKind != "idle" || woke.Next != "send" || woke.Status != string(StatusNeedsInput) {
		t.Fatalf("wake=%#v", woke)
	}
	raw, err := MarshalEnvelope(*woke)
	if err != nil || len(raw) == 0 {
		t.Fatalf("envelope empty: %s err=%v", raw, err)
	}
}

func TestTimeoutEnvelope(t *testing.T) {
	env := BuildEnvelope("ho-1", StatusRunning, 7, 7, "wait", "", true, UntilEvent)
	raw, err := MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.OK || !decoded.TimedOut || decoded.FromSeq != 7 || decoded.LastSeq != 7 || decoded.Next != "wait" {
		t.Fatalf("timeout envelope: %#v / %s", decoded, raw)
	}
	if len(decoded.Argv) == 0 {
		t.Fatal("timeout envelope must include re-arm argv")
	}
}

func TestParseUntil(t *testing.T) {
	cases := map[string]Until{
		"":             UntilEvent,
		"event":        UntilEvent,
		"needs-input":  UntilNeedsInput,
		"needs_input":  UntilNeedsInput,
		"send":         UntilNeedsInput,
		"terminal":     UntilTerminal,
		"done":         UntilTerminal,
	}
	for in, want := range cases {
		got, err := ParseUntil(in)
		if err != nil || got != want {
			t.Fatalf("ParseUntil(%q)=%q err=%v want %q", in, got, err, want)
		}
	}
	if _, err := ParseUntil("forever"); err == nil {
		t.Fatal("expected error for unknown until")
	}
}

func TestBareWaitIsNotRunToCompletion(t *testing.T) {
	// Documented contract: UntilEvent returns on the first actionable /
	// interim-wake event, not when the child finishes.
	if !ShouldWake(KindAgent, UntilEvent, "result", map[string]any{"source": "milestone"}) {
		t.Fatal("bare wait wakes on interim milestone")
	}
	if ShouldWake(KindAgent, UntilEvent, "progress", nil) {
		t.Fatal("bare wait does not wake on pure telemetry")
	}
	// Callers who want "wake when child needs me or is finished" must opt in.
	if ShouldWake(KindAgent, UntilNeedsInput, "result", map[string]any{"source": "milestone"}) {
		t.Fatal("needs-input until must not treat interim result as done")
	}
}
