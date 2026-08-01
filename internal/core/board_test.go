package core

import (
	"context"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func newBoardTestService(t *testing.T) (*BoardService, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	manager := &Session{
		ID: "sess-manager", HostID: "c1",
		Persist:   ports.PersistHandle{Kind: "tmux", Name: "manager"},
		CreatedAt: now,
	}
	for _, id := range []string{"sess-a", "sess-b"} {
		peer := &Session{
			ID: id, HostID: "c3",
			Persist:         ports.PersistHandle{Kind: "tmux", Name: id},
			SourceSessionID: manager.ID, CreatedAt: now,
		}
		if err := reg.PutSession(peer); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutSession(manager); err != nil {
		t.Fatal(err)
	}
	return &BoardService{Reg: reg, Msg: newFakeMsg(newFakeCoord())}, reg
}

func TestBoardQueryReturnsLatestValuePerNodeAndKey(t *testing.T) {
	board, _ := newBoardTestService(t)
	ctx := context.Background()
	if _, err := board.Post(ctx, "sess-a", "status", "phase", "capturing"); err != nil {
		t.Fatal(err)
	}
	if _, err := board.Post(ctx, "sess-b", "status", "phase", "idle"); err != nil {
		t.Fatal(err)
	}
	// sess-a moves on; the board holds state, so this supersedes.
	if _, err := board.Post(ctx, "sess-a", "status", "phase", "scoring"); err != nil {
		t.Fatal(err)
	}

	entries, err := board.Query(ctx, "sess-a", "status", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want one entry per node, got %d: %+v", len(entries), entries)
	}
	if entries[0].Node != "sess-a" || entries[0].Text != "scoring" {
		t.Fatalf("stale value for sess-a: %+v", entries[0])
	}
	if entries[1].Node != "sess-b" || entries[1].Text != "idle" {
		t.Fatalf("wrong value for sess-b: %+v", entries[1])
	}
}

func TestBoardQueryPeersOnlyDropsCallersOwnEntries(t *testing.T) {
	board, _ := newBoardTestService(t)
	ctx := context.Background()
	for _, p := range []struct{ id, text string }{{"sess-a", "mine"}, {"sess-b", "theirs"}} {
		if _, err := board.Post(ctx, p.id, "status", "phase", p.text); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := board.Query(ctx, "sess-a", "status", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Node != "sess-b" {
		t.Fatalf("want only the peer, got %+v", entries)
	}
}

// Scope is structural: a board id is derived from the caller's own manager, so
// siblings share one board and there is no way to name another subtree's.
func TestBoardIsSharedBySiblingsAndSeparatePerManager(t *testing.T) {
	board, reg := newBoardTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	other := &Session{ID: "sess-other-mgr", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "other"}, CreatedAt: now}
	outsider := &Session{ID: "sess-outsider", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "outsider"}, SourceSessionID: other.ID, CreatedAt: now}
	for _, s := range []*Session{other, outsider} {
		if err := reg.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := board.Post(ctx, "sess-a", "status", "phase", "capturing"); err != nil {
		t.Fatal(err)
	}

	// A sibling sees it.
	sib, err := board.Query(ctx, "sess-b", "status", "", false)
	if err != nil || len(sib) != 1 {
		t.Fatalf("siblings must share a board: %+v (%v)", sib, err)
	}
	// A node under a different manager cannot.
	out, err := board.Query(ctx, "sess-outsider", "status", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("another subtree's board leaked: %+v", out)
	}
}

func TestBoardRejectsRootAndBadCategory(t *testing.T) {
	board, reg := newBoardTestService(t)
	ctx := context.Background()
	root := &Session{ID: "sess-root", HostID: LocalHostID, Persist: ports.PersistHandle{Kind: LocalPersistKind, Name: "root"}, CreatedAt: time.Now().UTC()}
	if err := reg.PutSession(root); err != nil {
		t.Fatal(err)
	}
	if _, err := board.Post(ctx, "sess-root", "status", "k", "v"); err == nil {
		t.Fatal("a root has no peers and must be rejected")
	}
	if _, err := board.Post(ctx, "sess-a", "Bad Category!", "k", "v"); err == nil {
		t.Fatal("malformed category must be rejected")
	}
}

func TestBoardQueryNarrowsToOneKey(t *testing.T) {
	board, _ := newBoardTestService(t)
	ctx := context.Background()
	if _, err := board.Post(ctx, "sess-a", "resource", "gpu", "hamburg:2,3"); err != nil {
		t.Fatal(err)
	}
	if _, err := board.Post(ctx, "sess-a", "resource", "disk", "80%"); err != nil {
		t.Fatal(err)
	}
	entries, err := board.Query(ctx, "sess-b", "resource", "gpu", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != "gpu" || entries[0].Text != "hamburg:2,3" {
		t.Fatalf("key filter failed: %+v", entries)
	}
}
