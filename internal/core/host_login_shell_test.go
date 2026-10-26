package core

import (
	"strings"
	"testing"
)

func TestHostLoginShellGuardsAgainstZshExec(t *testing.T) {
	got := HostLoginShell("echo hi")
	if !IsHostLoginShell(got) {
		t.Fatalf("missing guard markers: %q", got)
	}
	for _, want := range []string{
		"RELAY_HOST_SHELL=1",
		"ZSH_VERSION=relay-host-shell",
		"bash -ilc",
		"echo hi",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// Must not be a bare bash -ilc: that is what hosts' `exec zsh -l` hijack.
	if strings.HasPrefix(got, "bash -ilc ") {
		t.Fatalf("bare bash -ilc is hijacked by fleet bashrc zsh auto-switch: %q", got)
	}
}

