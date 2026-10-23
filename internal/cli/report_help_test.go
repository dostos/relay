package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportAggregates(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	now := time.Now().UTC()
	evs := []usageEvent{
		{At: now.Add(-time.Hour), Cmd: "session.send", Target: "c1/a", State: "submitted", Evidence: "fresh_working_indicator", Keyed: true},
		{At: now.Add(-time.Hour), Cmd: "session.send", Target: "c1/a", State: "queued", Evidence: "accepted_behind_busy_turn", Replayed: true, Keyed: true},
		{At: now.Add(-50 * time.Minute), Cmd: "session.send", Target: "c1/b", State: "not_sent", Evidence: "send_lock_busy", Exit: 1, Error: "another send to this session is still in progress (waited 1m0s); nothing was typed"},
		{At: now.Add(-40 * time.Minute), Cmd: "session.send", Target: "c1/b", State: "not_sent", Evidence: "composer_not_empty", Exit: 1, Error: "target composer already holds unrelated text"},
		{At: now.Add(-30 * time.Minute), Cmd: "session.send", Target: "c3/x", State: "uncertain", Evidence: "no_fresh_turn", Exit: 67, Error: "no fresh turn observed in sess-0123abcd after submit", Direct: true},
		{At: now.Add(-20 * time.Minute), Cmd: "session.wait", Target: "c1/a", State: "timeout", Exit: 124},
		{At: now.Add(-48 * time.Hour), Cmd: "session.send", State: "uncertain"},
	}
	var b strings.Builder
	for _, e := range evs {
		e.Build = "abc1234"
		raw, _ := json.Marshal(e)
		b.Write(raw)
		b.WriteByte('\n')
	}
	b.WriteString("not json\n")
	_ = os.MkdirAll(usageDir(), 0o700)
	for _, day := range []time.Time{now, now.Add(-48 * time.Hour), now.Add(-time.Hour)} {
		_ = os.WriteFile(filepath.Join(usageDir(), day.Format("2006-01-02")+".jsonl"), []byte(b.String()), 0o600)
	}
	r := buildReport(usageDir(), now, 24*time.Hour, 3)
	if r.Send.Total < 5 || r.LockContention == 0 || r.ComposerRefusals == 0 || r.Send.Replays == 0 || r.Send.Direct == 0 || r.Wait["timeout"] == 0 {
		t.Fatalf("report = %+v", r)
	}
	if len(r.Recent) != 3 || r.Malformed == 0 || len(r.Findings) == 0 {
		t.Fatalf("recent/findings: %+v", r)
	}
	for _, e := range r.Errors {
		if strings.Contains(e.Key, "sess-0123") {
			t.Fatalf("error not normalized: %q", e.Key)
		}
	}
}

func TestHelpTopicsHaveUsageAndExamples(t *testing.T) {
	for k, tp := range helpTopics {
		if tp.usage == "" || tp.summary == "" || len(tp.examples) == 0 {
			t.Errorf("topic %q lacks usage/summary/examples", k)
		}
	}
	for _, cmd := range []string{"session", "fleet", "agents", "report", "host", "container", "auth", "resume", "capabilities", "version", "doctor", "targets", "client", "pane"} {
		if _, ok := helpTopics[cmd]; !ok {
			t.Errorf("top-level command %q has no --help topic", cmd)
		}
	}
	out := captureStdout(t, func() { (&App{noUsage: true}).Run([]string{"session", "wait", "x", "--help"}) })
	if !strings.Contains(out, "Usage: relay [--json] session wait") || !strings.Contains(out, "Examples:") {
		t.Fatalf("session wait --help:\n%s", out)
	}
	out = captureStdout(t, func() { (&App{noUsage: true}).Run([]string{"help", "exit-codes"}) })
	if !strings.Contains(out, "124") || !strings.Contains(out, "67") {
		t.Fatalf("exit codes:\n%s", out)
	}
	// help after -- is payload, not a help request
	if wantsHelp([]string{"session", "send", "x", "--", "--help"}) {
		t.Fatal("--help after -- treated as help")
	}
}
