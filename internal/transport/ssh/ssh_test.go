package ssh

import (
	"errors"
	"strings"
	"testing"
)

func TestTailBufferRetainsOnlyTheLastBytes(t *testing.T) {
	var buf tailBuffer
	first := strings.Repeat("a", maxStreamStderrBytes)
	last := "\nssh: connect to host 203.0.113.7 port 2222: Operation timed out\n"
	if _, err := buf.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write([]byte(last)); err != nil {
		t.Fatal(err)
	}
	if len(buf.data) != maxStreamStderrBytes {
		t.Fatalf("buffer length = %d, want %d", len(buf.data), maxStreamStderrBytes)
	}
	if !strings.HasSuffix(buf.String(), last) {
		t.Fatalf("tail missing final diagnostic: %q", buf.String()[len(buf.String())-len(last):])
	}
}

func TestStreamErrorKeepsOnlyLastDiagnosticLine(t *testing.T) {
	err := streamError("test-host", errors.New("exit status 255"), "\nssh: first failure\nssh: connect to host 203.0.113.7 port 2222: Operation timed out\n")

	got := err.Error()
	if !strings.Contains(got, "ssh stream to test-host") {
		t.Fatalf("missing structured stream prefix: %q", got)
	}
	if !strings.Contains(got, "Operation timed out") {
		t.Fatalf("missing last diagnostic: %q", got)
	}
	if strings.Contains(got, "first failure") {
		t.Fatalf("included stale diagnostic: %q", got)
	}
}

func TestInteractiveBridgeUsesDedicatedReverseForward(t *testing.T) {
	tr := New("c3")
	tr.SetReverseUnixForward("/tmp/remote.sock", "/tmp/local.sock")
	args, err := tr.interactiveArgs("tmux attach -t named")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"ControlMaster=no", "StreamLocalBindUnlink=yes", "StreamLocalBindMask=0177",
		"-R /tmp/remote.sock:/tmp/local.sock", "-t c3 tmux attach -t named",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "ControlMaster=auto") {
		t.Fatalf("bridge attach must not share a control master: %s", joined)
	}
}

func TestReverseSocketCleanupCommandQuotesExactPath(t *testing.T) {
	got, err := reverseSocketCleanupCommand("/tmp/relay-bridge-sess-a.sock")
	if err != nil {
		t.Fatal(err)
	}
	if got != "rm -f -- '/tmp/relay-bridge-sess-a.sock'" {
		t.Fatalf("cleanup = %q", got)
	}
}
