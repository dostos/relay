package main

import (
	"testing"

	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func TestVisualizationAuthoritySnapshotCarriesCurrentLineageAndContext(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	reg := &core.Registry{}
	for _, session := range []*core.Session{
		{ID: "sess-apex", HostID: "home", Persist: ports.PersistHandle{Kind: "tmux", Name: "apex"}},
		{ID: "sess-engram", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "engram"}, SourceSessionID: "sess-apex"},
	} {
		if err := reg.PutSession(session); err != nil {
			t.Fatal(err)
		}
	}
	items, err := visualizationAuthoritySnapshot()
	if err != nil || len(items) != 2 {
		t.Fatalf("snapshot=%+v err=%v", items, err)
	}
	if items[1].SessionID != "sess-engram" || items[1].ParentSessionID != "sess-apex" || items[1].Target != "c3" || items[1].TmuxName != "engram" {
		t.Fatalf("current authority fields lost: %+v", items[1])
	}
	resolution, err := visualizationAuthorityResume("apex")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.SessionID != "sess-apex" || resolution.Target != "home" || resolution.TmuxName != "apex" {
		t.Fatalf("resume resolution=%+v", resolution)
	}
	_, events, err := coordrelayd.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	store, err := coordrelayd.NewStore(events)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Emit("relay-viz-mac", "project", map[string]any{"session_id": "sess-apex"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := visualizationAuthoritySnapshotV2("relay-viz-mac")
	if err != nil || snapshot.V != 1 || snapshot.Revision != 1 || len(snapshot.Items) != 2 {
		t.Fatalf("v2 snapshot=%+v err=%v", snapshot, err)
	}
}

func TestVisualizationAuthorityResumeRejectsAmbiguousOrInvalidName(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &core.Registry{}
	for _, id := range []string{"sess-a", "sess-b"} {
		if err := reg.PutSession(&core.Session{ID: id, HostID: "home", Persist: ports.PersistHandle{Kind: "tmux", Name: "shared"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := visualizationAuthorityResume("shared"); err == nil {
		t.Fatal("ambiguous authoritative name accepted")
	}
	if _, err := visualizationAuthorityResume("-oProxyCommand=bad"); err == nil {
		t.Fatal("invalid broker name accepted")
	}
}

func TestConsistentAuthoritySnapshotRetriesAcrossConcurrentEvents(t *testing.T) {
	revisions := []int64{10, 12, 12, 12}
	revision := func() (int64, error) {
		value := revisions[0]
		revisions = revisions[1:]
		return value, nil
	}
	reads := 0
	snapshot, err := consistentAuthoritySnapshot(revision, func() ([]ports.Presentation, error) {
		reads++
		return []ports.Presentation{{SessionID: "sess-current", Target: "home", TmuxName: "current"}}, nil
	})
	if err != nil || reads != 2 || snapshot.Revision != 12 {
		t.Fatalf("snapshot=%+v reads=%d err=%v", snapshot, reads, err)
	}
}
