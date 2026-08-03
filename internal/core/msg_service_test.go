package core

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// fakeCoord implements ports.Coord in-memory. Subscribe replays canned events
// for a stream, then (in follow mode) blocks until ctx is cancelled — modelling
// a real relayd follow stream so WaitOne's cancel/leak path is exercised.
type fakeCoord struct {
	mu         sync.Mutex
	events     map[string][]coord.Event
	nextSeq    map[string]int64
	subscribed chan struct{}
}

func newFakeCoord() *fakeCoord {
	return &fakeCoord{events: map[string][]coord.Event{}, nextSeq: map[string]int64{}}
}
func (f *fakeCoord) Kind() string                                        { return "fake" }
func (f *fakeCoord) Ensure(ctx context.Context, t ports.Transport) error { return nil }
func (f *fakeCoord) EventsPath(name string) string                       { return name }
func (f *fakeCoord) SensorCommand(session, kind string) (string, error) {
	return session + ":" + kind, nil
}
func (f *fakeCoord) Emit(ctx context.Context, t ports.Transport, session, kind string, meta map[string]any) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq[session]++
	seq := f.nextSeq[session]
	f.events[session] = append(f.events[session], coord.Event{Seq: seq, Kind: kind, Sess: session, Meta: meta})
	return seq, nil
}
func (f *fakeCoord) Subscribe(ctx context.Context, t ports.Transport, session string, fromSeq int64, follow bool, w io.Writer) error {
	if f.subscribed != nil {
		select {
		case f.subscribed <- struct{}{}:
		default:
		}
	}
	f.mu.Lock()
	evs := append([]coord.Event(nil), f.events[session]...)
	f.mu.Unlock()
	for _, ev := range evs {
		if ev.Seq <= fromSeq {
			continue
		}
		b, _ := json.Marshal(ev)
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	if follow {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func newFakeMsg(c *fakeCoord) *MsgService {
	return &MsgService{Coord: c, NewTransport: func(string) (ports.Transport, error) {
		return &fakeTransport{id: "h", outputs: map[string]string{"h": ""}}, nil
	}}
}

func TestWaitOneFanInFirstWins(t *testing.T) {
	c := newFakeCoord()
	_, _ = c.Emit(context.Background(), nil, channelStream("a"), "result", map[string]any{"text": "from-a"})
	s := newFakeMsg(c)
	m, timedOut, err := s.WaitOne(context.Background(), "h", []string{"a", "b"}, map[string]int64{"a": 0, "b": 0}, 3*time.Second)
	if err != nil || timedOut {
		t.Fatalf("expected a message, got timedOut=%v err=%v", timedOut, err)
	}
	if m.Channel != "a" || m.Text != "from-a" {
		t.Fatalf("wrong message: %+v", m)
	}
}

func TestReadProjectsKnownChannelOnlyOnce(t *testing.T) {
	c := newFakeCoord()
	_, _ = c.Emit(context.Background(), nil, channelStream("known"), "msg", map[string]any{"from": "a", "text": "one"})
	_, _ = c.Emit(context.Background(), nil, channelStream("known"), "msg", map[string]any{"from": "b", "text": "two"})
	s := newFakeMsg(c)
	messages, _, err := s.Read(context.Background(), "h", "known", 0, false, 0)
	if err != nil || len(messages) != 2 {
		t.Fatalf("read=%+v err=%v", messages, err)
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "channel") || strings.Contains(string(raw), "\"ts\"") {
		t.Fatalf("single-channel read repeated durable context: %s", raw)
	}
	for _, message := range messages {
		if message.Seq == 0 || message.Kind != "msg" || message.Text == "" {
			t.Fatalf("decision content lost: %+v", message)
		}
	}
	legacy := []map[string]any{}
	for _, message := range messages {
		legacy = append(legacy, map[string]any{"channel": "known", "seq": message.Seq, "ts": "2026-08-03T00:00:00Z", "kind": message.Kind, "from": message.From, "text": message.Text})
	}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= len(legacyRaw) {
		t.Fatalf("known-channel projection did not shrink: before=%d after=%d", len(legacyRaw), len(raw))
	}
	t.Logf("two_message_read_bytes=%d->%d token_estimate=%d->%d", len(legacyRaw), len(raw), (len(legacyRaw)+3)/4, (len(raw)+3)/4)
}

func TestWaitOneTimeout(t *testing.T) {
	c := newFakeCoord()
	s := newFakeMsg(c)
	start := time.Now()
	m, timedOut, err := s.WaitOne(context.Background(), "h", []string{"x", "y"}, map[string]int64{}, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut || m != nil {
		t.Fatalf("expected timeout, got m=%+v timedOut=%v", m, timedOut)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout took too long — likely a stuck goroutine")
	}
}

func TestWaitOneRespectsCursor(t *testing.T) {
	c := newFakeCoord()
	// seq 1 is backlog we must skip via cursor; nothing newer ⇒ timeout.
	_, _ = c.Emit(context.Background(), nil, channelStream("a"), "note", map[string]any{"text": "old"})
	s := newFakeMsg(c)
	_, timedOut, _ := s.WaitOne(context.Background(), "h", []string{"a"}, map[string]int64{"a": 1}, 300*time.Millisecond)
	if !timedOut {
		t.Fatal("cursor at 1 must skip seq-1 backlog and time out")
	}
}

func TestReadDrainsBacklog(t *testing.T) {
	c := newFakeCoord()
	for i := 0; i < 3; i++ {
		_, _ = c.Emit(context.Background(), nil, channelStream("a"), "msg", map[string]any{"text": "x"})
	}
	s := newFakeMsg(c)
	msgs, next, err := s.Read(context.Background(), "h", "a", 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || next != 3 {
		t.Fatalf("read got %d msgs next=%d, want 3/3", len(msgs), next)
	}
	// re-read from cursor ⇒ nothing new
	msgs2, _, _ := s.Read(context.Background(), "h", "a", next, false, 0)
	if len(msgs2) != 0 {
		t.Fatalf("re-read from cursor should be empty, got %d", len(msgs2))
	}
}
