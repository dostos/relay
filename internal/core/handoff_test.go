package core

import "testing"

func TestHandoffLayoutPreservesExplicitWorkspace(t *testing.T) {
	layout := handoffLayout(HandoffOpts{Workspace: "workspace:9"})

	if layout.Mode != "remote" {
		t.Fatalf("mode = %q, want remote", layout.Mode)
	}
	if layout.Workspace != "workspace:9" {
		t.Fatalf("workspace = %q, want workspace:9", layout.Workspace)
	}
}
