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
	all, err := s.Replay("sess1", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("replay %v %v", all, err)
	}
	after, err := s.Replay("sess1", 1)
	if err != nil || len(after) != 1 || after[0].Kind != "exit" {
		t.Fatalf("after %+v", after)
	}
}
