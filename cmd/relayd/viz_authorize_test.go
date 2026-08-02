package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorizeVizKeyInstallsRestrictedEntryIdempotently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	publicKey := generateTestPublicKey(t)
	first, err := authorizeVizKey("relay-viz-mac", publicKey)
	if err != nil || !first.Installed || first.Fingerprint == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	raw, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	line := string(raw)
	if !strings.HasPrefix(line, `restrict,command="$HOME/.local/bin/relayd viz-broker --service relay-viz-mac" ssh-ed25519 `) || strings.Contains(line, "PRIVATE") {
		t.Fatalf("unsafe authorization: %q", line)
	}
	second, err := authorizeVizKey("relay-viz-mac", publicKey)
	if err != nil || second.Installed {
		t.Fatalf("idempotent result=%+v err=%v", second, err)
	}
}

func TestAuthorizeVizKeyRefusesExistingUnrestrictedCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	publicKey := generateTestPublicKey(t)
	raw, err := os.ReadFile(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	authorized := filepath.Join(sshDir, "authorized_keys")
	if err := os.WriteFile(authorized, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizeVizKey("relay-viz-mac", publicKey); err == nil || !strings.Contains(err.Error(), "unrestricted") {
		t.Fatalf("unrestricted duplicate accepted: %v", err)
	}
	got, _ := os.ReadFile(authorized)
	if string(got) != string(raw) {
		t.Fatal("refused authorization still changed authorized_keys")
	}
}

func generateTestPublicKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "viz")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "relay-viz-test", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v (%s)", err, out)
	}
	return path + ".pub"
}
