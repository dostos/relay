package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHandoffLayoutPreservesSourcePlacement(t *testing.T) {
	layout := handoffLayout(HandoffOpts{Workspace: "workspace:9", Pane: "pane:12", SourceSessionID: "sess-parent"})

	if layout.Mode != "remote" {
		t.Fatalf("mode = %q, want remote", layout.Mode)
	}
	if layout.Workspace != "workspace:9" {
		t.Fatalf("workspace = %q, want workspace:9", layout.Workspace)
	}
	if layout.Pane != "pane:12" || layout.SourceSessionID != "sess-parent" {
		t.Fatalf("source placement lost: %+v", layout)
	}
}

func TestSubscribeRetryStatusShowsStructuredLastError(t *testing.T) {
	status := subscribeRetryStatus("test-host", 2, 3*time.Second, errors.New("ssh stream to test-host: ssh: connect timed out"))
	for _, want := range []string{"waiting test-host", "last error: ssh stream to test-host", "retry 2/6 in 3s"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status %q missing %q", status, want)
		}
	}
}
