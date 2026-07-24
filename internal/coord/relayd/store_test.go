package relayd

import (
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
