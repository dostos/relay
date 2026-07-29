package core

import (
	"testing"

	"github.com/dostos/relay/internal/ports"
)

func TestRemovePaneBindingsForPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)

	// Two surfaces pin the same session (the duplicate-pane case), one pins another.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(WritePaneBinding(PaneBinding{Surface: "surface:23", PersistName: "relay-painpoint-test", Pinned: true}))
	must(WritePaneBinding(PaneBinding{Surface: "surface:24", PersistName: "relay-painpoint-test", Pinned: true}))
	must(WritePaneBinding(PaneBinding{Surface: "surface:99", PersistName: "other-session", Pinned: true}))

	if n := RemovePaneBindingsForPersist("relay-painpoint-test"); n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	if _, err := ReadPaneBinding("surface:23"); err == nil {
		t.Fatal("surface:23 binding should be gone")
	}
	if _, err := ReadPaneBinding("surface:24"); err == nil {
		t.Fatal("surface:24 binding should be gone")
	}
	if _, err := ReadPaneBinding("surface:99"); err != nil {
		t.Fatalf("unrelated surface:99 must survive: %v", err)
	}

	// Idempotent + missing-file safe.
	if n := RemovePaneBindingsForPersist("relay-painpoint-test"); n != 0 {
		t.Fatalf("second sweep removed %d, want 0", n)
	}
	if err := RemovePaneBinding("surface:does-not-exist"); err != nil {
		t.Fatalf("RemovePaneBinding on missing file must be nil, got %v", err)
	}
}

func TestPruneResumeCleanedOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)

	// A resumable entry that must survive a cleaned-only prune, and two tombstones.
	upsertResume(&Session{ID: "sess-live", HostID: "h1", Persist: ports.PersistHandle{Kind: "tmux", Name: "keep-me"}}, ResumeStateResumable, "active")
	MarkResumeCleaned("dead-1", "destroyed")
	MarkResumeCleaned("dead-2", "destroyed")

	removed, err := PruneResume(true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("pruned %v, want 2 cleaned tombstones", removed)
	}
	if _, err := LookupResume("keep-me"); err != nil {
		t.Fatalf("resumable entry must survive cleaned-only prune: %v", err)
	}
}
