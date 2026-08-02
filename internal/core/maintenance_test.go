package core

import (
	"context"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

func TestParseHostState(t *testing.T) {
	out := "@TMUX\nsess-a\nsess-b\n@CHAN\nteamX\t5\t1000\nempty-chan\t0\t2000\nold-chan\t3\t500\n"
	tmux, chans := parseHostState(out)
	if !tmux["sess-a"] || !tmux["sess-b"] || len(tmux) != 2 {
		t.Fatalf("tmux parse wrong: %v", tmux)
	}
	if len(chans) != 3 {
		t.Fatalf("want 3 channels, got %d: %+v", len(chans), chans)
	}
	byName := map[string]channelStat{}
	for _, c := range chans {
		byName[c.name] = c
	}
	if byName["teamX"].lines != 5 || byName["teamX"].mtime != 1000 {
		t.Fatalf("teamX parse: %+v", byName["teamX"])
	}
	if byName["empty-chan"].lines != 0 {
		t.Fatalf("empty-chan lines: %+v", byName["empty-chan"])
	}
}

func TestGCReapsAndGCsChannels(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	reg := &Registry{}
	// one live session, one dead — both on h-ok
	_ = reg.PutSession(&Session{ID: "s-live", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "alive"}})
	_ = reg.PutSession(&Session{ID: "s-dead", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "gone"}})
	RememberResume(&Session{ID: "s-live", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "alive"}})
	RememberResume(&Session{ID: "s-dead", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "gone"}})

	now := time.Now().Unix()
	// probe: only "alive" tmux present; channels: fresh (keep), empty (gc), old (gc)
	out := "@TMUX\nalive\n@CHAN\n" +
		"fresh\t4\t" + itoa(now) + "\n" +
		"empty\t0\t" + itoa(now) + "\n" +
		"stale\t9\t" + itoa(now-100000) + "\n"
	sessions := &SessionService{Reg: reg, NewTransport: func(string) (ports.Transport, error) {
		return &fakeTransport{id: "h-ok", outputs: map[string]string{"h-ok": out}}, nil
	}}
	m := &MaintenanceService{Sessions: sessions, Reg: reg, NewTransport: sessions.NewTransport} // Viz nil

	rep, err := m.GC(context.Background(), []string{"h-ok"}, 24*time.Hour, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Hosts) != 1 || !rep.Hosts[0].Reachable {
		t.Fatalf("host result: %+v", rep.Hosts)
	}
	h := rep.Hosts[0]
	if !contains(h.ReapedSessions, "gone") || contains(h.ReapedSessions, "alive") {
		t.Fatalf("reap wrong: %+v", h.ReapedSessions)
	}
	if !contains(h.GCedChannels, "empty") || !contains(h.GCedChannels, "stale") || contains(h.GCedChannels, "fresh") {
		t.Fatalf("channel gc wrong: %+v", h.GCedChannels)
	}
	if h.KeptChannels != 1 {
		t.Fatalf("kept channels = %d want 1", h.KeptChannels)
	}
	if _, err := reg.GetSession("s-dead"); err == nil {
		t.Fatal("dead session row should be gone")
	}
	if _, err := reg.GetSession("s-live"); err != nil {
		t.Fatal("live session must survive")
	}
}

func TestGCDryRunNoMutation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_STATE_DIR", dir)
	reg := &Registry{}
	_ = reg.PutSession(&Session{ID: "s-dead", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "gone"}})
	RememberResume(&Session{ID: "s-dead", HostID: "h-ok", Persist: ports.PersistHandle{Kind: "tmux", Name: "gone"}})
	out := "@TMUX\n@CHAN\nempty\t0\t1\n" // reachable, no tmux, one empty channel
	sessions := &SessionService{Reg: reg, NewTransport: func(string) (ports.Transport, error) {
		return &fakeTransport{id: "h-ok", outputs: map[string]string{"h-ok": out}}, nil
	}}
	m := &MaintenanceService{Sessions: sessions, Reg: reg, NewTransport: sessions.NewTransport}

	rep, err := m.GC(context.Background(), []string{"h-ok"}, 24*time.Hour, false, true /*dryRun*/)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(rep.Hosts[0].ReapedSessions, "gone") || !contains(rep.Hosts[0].GCedChannels, "empty") {
		t.Fatalf("dry-run should still REPORT actions: %+v", rep.Hosts[0])
	}
	if _, err := reg.GetSession("s-dead"); err != nil {
		t.Fatal("dry-run must NOT delete the session row")
	}
	if p, _, _ := reg.ClassifyResume("gone"); p == PresenceCleaned {
		t.Fatal("dry-run must NOT mark cleaned")
	}
}

func TestRepairDanglingLineageMovesOnlyPreviouslyManagedSessionsToApex(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	apex := &Session{ID: "sess-apex", HostID: "home", Persist: ports.PersistHandle{Name: "apex-v3"}, Labels: map[string]string{ApexLabel: "true"}, CreatedAt: now}
	orphan := &Session{ID: "sess-orphan", SourceSessionID: "sess-dead", CreatedByHandoffID: "ho-orphan", CreatedAt: now}
	root := &Session{ID: "sess-intentional-root", CreatedAt: now}
	for _, sess := range []*Session{apex, orphan, root} {
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.PutHandoff(&Handoff{ID: "ho-orphan", SessionID: orphan.ID, SourceSessionID: "sess-dead", Status: StatusRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	m := &MaintenanceService{Reg: reg}
	repaired := m.repairDanglingLineage()
	if len(repaired) != 1 || repaired[0] != orphan.ID {
		t.Fatalf("repaired=%v", repaired)
	}
	gotOrphan, _ := reg.GetSession(orphan.ID)
	gotRoot, _ := reg.GetSession(root.ID)
	gotHandoff, _ := reg.GetHandoff("ho-orphan")
	if gotOrphan.SourceSessionID != apex.ID || gotHandoff.SourceSessionID != apex.ID {
		t.Fatalf("orphan session=%+v handoff=%+v", gotOrphan, gotHandoff)
	}
	if gotRoot.SourceSessionID != "" {
		t.Fatalf("intentional root was annexed: %+v", gotRoot)
	}
}

func itoa(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
