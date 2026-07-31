package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "0s",
		2 * time.Second:  "2s",
		65 * time.Second: "1m5s",
		2 * time.Minute:  "2m",
	}
	for d, want := range cases {
		if got := FormatDuration(d); got != want {
			t.Fatalf("FormatDuration(%v)=%q want %q", d, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("abcdef", 4); got != "abc…" {
		t.Fatalf("got %q", got)
	}
	if got := Truncate("ab", 10); got != "ab" {
		t.Fatalf("got %q", got)
	}
}

func TestAttemptLabel(t *testing.T) {
	if AttemptLabel(1) != "attempt" || AttemptLabel(2) != "attempts" {
		t.Fatal("pluralization")
	}
}

func TestSSHNoiseFilterDropsDisconnectChatter(t *testing.T) {
	var buf bytes.Buffer
	f := &SSHNoiseFilter{W: &buf}
	_, _ = f.Write([]byte("Connection closed by UNKNOWN port 65535\n"))
	_, _ = f.Write([]byte("Shared connection to example.com closed.\n"))
	_, _ = f.Write([]byte("client_loop: send disconnect: Broken pipe\n"))
	_, _ = f.Write([]byte("Read from remote host example.com: Connection reset by peer\n"))
	_, _ = f.Write([]byte("ssh: connect to host 203.0.113.7 port 2222: Operation timed out\n"))
	_, _ = f.Write([]byte("Error: remote port forwarding failed for listen path /tmp/relay-bridge-sess-a.sock\n"))
	_, _ = f.Write([]byte("Permission denied (publickey).\n"))
	_ = f.Flush()
	got := buf.String()
	if strings.Contains(got, "Connection closed") || strings.Contains(got, "Shared connection") ||
		strings.Contains(got, "client_loop") || strings.Contains(got, "Connection reset") {
		t.Fatalf("noise leaked: %q", got)
	}
	if !strings.Contains(got, "Permission denied") {
		t.Fatalf("real error filtered: %q", got)
	}
	if got := f.LastDiagnostic(); got != "Error: remote port forwarding failed for listen path /tmp/relay-bridge-sess-a.sock" {
		t.Fatalf("last error = %q", got)
	}
}

func TestSSHNoiseFilterKeepsAppStderr(t *testing.T) {
	var buf bytes.Buffer
	f := &SSHNoiseFilter{W: &buf}
	keep := []string{
		"app: Connection closed by user request\n",
		"INFO Shared connection to pool is healthy\n",
		"debug: client_loop finished ok\n",
		"timeout: Read from remote host failed for unrelated reason\n",
	}
	for _, line := range keep {
		_, _ = f.Write([]byte(line))
	}
	_ = f.Flush()
	got := buf.String()
	for _, line := range keep {
		if !strings.Contains(got, strings.TrimSpace(line)) {
			t.Fatalf("legitimate stderr dropped: want %q in %q", line, got)
		}
	}
}

func TestSSHNoiseFilterChunkedWrite(t *testing.T) {
	var buf bytes.Buffer
	f := &SSHNoiseFilter{W: &buf}
	msg := "Connection closed by UNKNOWN port 65535\n"
	_, _ = f.Write([]byte(msg[:10]))
	_, _ = f.Write([]byte(msg[10:]))
	_ = f.Flush()
	if buf.Len() != 0 {
		t.Fatalf("chunked noise leaked: %q", buf.String())
	}
}

func TestStatusNonTTYWaitOneLine(t *testing.T) {
	var buf bytes.Buffer
	st := NewStatusTo(&buf)
	done := make(chan struct{})
	ok := st.Wait(30*time.Millisecond, func(left time.Duration) string {
		return "phase · retry in " + FormatDuration(left)
	}, done)
	if !ok {
		t.Fatal("expected complete")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("non-TTY Wait should emit one line, got %d: %q", len(lines), buf.String())
	}
}

func TestStatusWaitCancel(t *testing.T) {
	var buf bytes.Buffer
	st := NewStatusTo(&buf)
	cancel := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(cancel)
	}()
	ok := st.Wait(2*time.Second, func(left time.Duration) string {
		return "waiting"
	}, cancel)
	if ok {
		t.Fatal("expected cancel")
	}
}

func TestStatusNonTTYWritesOncePerChange(t *testing.T) {
	var buf bytes.Buffer
	st := NewStatusTo(&buf)
	st.Render("a · attempt 1 · retry in 2s")
	st.Render("a · attempt 1 · retry in 2s")
	st.Render("a · attempt 1 · retry in 1s")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
}

func TestNoteWarnDone(t *testing.T) {
	var buf bytes.Buffer
	NoteTo(&buf, "hello")
	WarnTo(&buf, "careful")
	DoneTo(&buf, "done")
	s := buf.String()
	if !strings.Contains(s, "relay · hello") || !strings.Contains(s, "relay ! careful") || !strings.Contains(s, "relay ok done") {
		t.Fatalf("bad notes: %q", s)
	}
}
