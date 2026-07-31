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
