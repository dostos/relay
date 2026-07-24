package relayd

import (
	"os"
	"path/filepath"
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
