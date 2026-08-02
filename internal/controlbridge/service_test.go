package controlbridge

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLinkLocalSocketReplacesStaleEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.sock")
	target := filepath.Join(dir, "bridge.sock")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkLocalSocket(path, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(path)
	if err != nil || got != target {
		t.Fatalf("link = %q, %v; want %q", got, err, target)
	}
}

func TestTunnelArgsAreDedicatedAndNonInteractive(t *testing.T) {
	got := tunnelArgs("/tmp/remote.sock", "/tmp/local.sock", "worker")
	want := []string{
		"-N", "-o", "BatchMode=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=4",
		"-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes",
		"-o", "StreamLocalBindMask=0177", "-R", "/tmp/remote.sock:/tmp/local.sock", "worker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v", got)
	}
}
