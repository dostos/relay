package relayd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStoreEmitReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ev1, err := s.Emit("sess1", "started", nil)
	if err != nil || ev1.Seq != 1 {
		t.Fatalf("ev1 %+v err %v", ev1, err)
	}
	ev2, err := s.Emit("sess1", "exit", map[string]any{"code": 0})
	if err != nil || ev2.Seq != 2 {
		t.Fatalf("ev2 %+v err %v", ev2, err)
	}
	all, ch, err := s.ReplayAndSubscribe("sess1", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("replay %v %v", all, err)
	}
	s.Unsubscribe("sess1", ch)
	after, ch2, err := s.ReplayAndSubscribe("sess1", 1)
	if err != nil || len(after) != 1 || after[0].Kind != "exit" {
		t.Fatalf("after %+v", after)
	}
	s.Unsubscribe("sess1", ch2)
}

func TestStoreRepairsPartialTailBeforeNextSequence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if event, err := store.Emit("sess1", "started", nil); err != nil || event.Seq != 1 {
		t.Fatalf("first event=%+v err=%v", event, err)
	}
	path := filepath.Join(dir, "sess1.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"ts":"partial"`)
	_ = file.Close()

	restarted, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.Emit("sess1", "result", map[string]any{"text": "done"})
	if err != nil || second.Seq != 2 {
		t.Fatalf("recovered event=%+v err=%v", second, err)
	}
	events, channel, err := restarted.ReplayAndSubscribe("sess1", 0)
	if err != nil || len(events) != 2 || events[1].Seq != 2 {
		t.Fatalf("replay=%+v err=%v", events, err)
	}
	restarted.Unsubscribe("sess1", channel)
}

func TestStoreReportsInteriorAndOversizedCorruption(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "interior", raw: "not-json\n{}\n", want: "record 1 is corrupt"},
		{name: "oversized", raw: strings.Repeat("x", maxEventRecordBytes+1) + "\n", want: "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "sess1.jsonl"), []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LastSeq("sess1"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("corruption error=%v want=%q", err, tc.want)
			}
		})
	}
}

func TestStoreRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Emit("../etc", "x", nil); err == nil {
		t.Fatal("expected reject")
	}
}

func TestStoreConcurrentSessionsIndependent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, sess := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := s.Emit(name, "tick", nil); err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
			}
		}(sess)
	}
	wg.Wait()
	for _, name := range []string{"a", "b", "c"} {
		seq, err := s.LastSeq(name)
		if err != nil || seq != 20 {
			t.Fatalf("%s last=%d err=%v", name, seq, err)
		}
	}
}

func TestStoreEmitNoSeqGapOnWriteFail(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ev1, err := s.Emit("sess1", "started", nil)
	if err != nil || ev1.Seq != 1 {
		t.Fatalf("ev1 %+v err %v", ev1, err)
	}
	p := filepath.Join(dir, "sess1.jsonl")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if _, err := s.Emit("sess1", "exit", nil); err == nil {
		t.Fatal("expected write failure")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	ev2, err := s.Emit("sess1", "exit", nil)
	if err != nil || ev2.Seq != 2 {
		t.Fatalf("want seq 2 after recovered write, got %+v err %v", ev2, err)
	}
}
