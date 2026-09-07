package ssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestConfiguredEndpointKeepsStrictNonInteractivePolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tr := New("100.108.118.32")
	if err := tr.ConfigureEndpoint("dostos", 2222, "~/.ssh/viz"); err != nil {
		t.Fatal(err)
	}
	args, err := tr.interactiveArgs("tmux attach-session -t =apex-v4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "-p 2222", "-i " + filepath.Join(os.Getenv("HOME"), ".ssh/viz"), "dostos@100.108.118.32"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
}

func TestControlOptionsMakeTheMasterMortal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	opts, err := controlOpts()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(opts, " ")
	// With ControlMaster=auto the master inherits the options of whichever
	// launch creates it first, so keepalives must live on the shared options
	// or a master can be born unable to notice a dead peer.
	for _, want := range []string{"ServerAliveInterval=", "ServerAliveCountMax=", "TCPKeepAlive=yes"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("shared control options missing %q: %s", want, joined)
		}
	}
}

func TestResolvedControlPathIsCachedPerEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tr := New("relay-test-invalid-host")
	tr.controlPathCache = "/tmp/relay-cached-socket"

	got, err := tr.resolvedControlPath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/relay-cached-socket" {
		t.Fatalf("cached control path not reused: %q", got)
	}
}

func TestWedgedControlSocketIsRemovedWhenMasterWillNotAnswer(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not available")
	}
	t.Setenv("HOME", t.TempDir())
	sock := filepath.Join(t.TempDir(), "wedged-master")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tr := New("relay-test-invalid-host")
	tr.controlPathCache = sock

	tr.ensureLiveControlMaster(context.Background())

	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("wedged control socket survived: stat err = %v", err)
	}
}

func TestLiveControlMasterCheckIsSkippedWhenNoSocketExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "absent")
	tr := New("relay-test-invalid-host")
	tr.controlPathCache = missing

	tr.ensureLiveControlMaster(context.Background())

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("probe must not create a socket path: %v", err)
	}
}
