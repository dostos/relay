package main

import (
	"testing"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func TestVisualizationAuthoritySnapshotCarriesCurrentLineageAndContext(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
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
}
