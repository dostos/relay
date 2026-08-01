package core

import (
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func putChain(t *testing.T, reg *Registry, ids ...string) {
	t.Helper()
	now := time.Now().UTC()
	for i, id := range ids {
		sess := &Session{
			ID: id, HostID: "h", Persist: ports.PersistHandle{Kind: "tmux", Name: id},
			CreatedAt: now,
		}
		if i+1 < len(ids) {
			sess.SourceSessionID = ids[i+1]
		}
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAncestorChainReturnsNearestFirstAndStopsAtRoot(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	// child -> mid -> root (root has empty SourceSessionID)
	putChain(t, reg, "sess-child", "sess-mid", "sess-root")

	chain := AncestorChain(reg, "sess-child")
	if len(chain) != 2 {
		t.Fatalf("want 2 ancestors, got %d: %+v", len(chain), chain)
	}
	if chain[0].ID != "sess-mid" || chain[1].ID != "sess-root" {
		t.Fatalf("wrong order: %s, %s", chain[0].ID, chain[1].ID)
	}
}

func TestAncestorChainStopsOnCycle(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	a := &Session{ID: "sess-a", SourceSessionID: "sess-b", CreatedAt: now}
	b := &Session{ID: "sess-b", SourceSessionID: "sess-a", CreatedAt: now}
	for _, s := range []*Session{a, b} {
		if err := reg.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	chain := AncestorChain(reg, "sess-a")
	if len(chain) != 1 || chain[0].ID != "sess-b" {
		t.Fatalf("cycle not bounded, got %+v", chain)
	}
}

func TestAncestorChainStopsOnMissingSession(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	orphan := &Session{ID: "sess-orphan", SourceSessionID: "sess-gone", CreatedAt: now}
	if err := reg.PutSession(orphan); err != nil {
		t.Fatal(err)
	}
	chain := AncestorChain(reg, "sess-orphan")
	if len(chain) != 0 {
		t.Fatalf("want empty chain, got %+v", chain)
	}
}
