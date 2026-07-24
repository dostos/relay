package relayd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dostos/relay/internal/coord"
)

// Store is an append-only per-session JSONL event log with monotonic seq.
type Store struct {
	dir string
	mu  sync.Mutex
	seq map[string]int64
	sub map[string][]chan coord.Event // session -> subscribers
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{
		dir: dir,
		seq: map[string]int64{},
		sub: map[string][]chan coord.Event{},
	}, nil
}

func (s *Store) path(session string) string {
	return filepath.Join(s.dir, session+".jsonl")
}

func (s *Store) loadSeqLocked(session string) error {
	if _, ok := s.seq[session]; ok {
		return nil
	}
	f, err := os.Open(s.path(session))
	if err != nil {
		if os.IsNotExist(err) {
			s.seq[session] = 0
			return nil
		}
		return err
	}
	defer f.Close()
	var last int64
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		var ev coord.Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Seq > last {
			last = ev.Seq
		}
	}
	s.seq[session] = last
	return sc.Err()
}

// Emit appends an event and notifies subscribers.
func (s *Store) Emit(session, kind string, meta map[string]any) (coord.Event, error) {
	if session == "" || kind == "" {
		return coord.Event{}, fmt.Errorf("session and kind required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadSeqLocked(session); err != nil {
		return coord.Event{}, err
	}
	s.seq[session]++
	ev := coord.Event{
		TS:   time.Now().UTC().Format(time.RFC3339),
		Seq:  s.seq[session],
		Sess: session,
		Kind: kind,
		Meta: meta,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return coord.Event{}, err
	}
	f, err := os.OpenFile(s.path(session), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return coord.Event{}, err
	}
	_, err = f.Write(append(b, '\n'))
	_ = f.Close()
	if err != nil {
		return coord.Event{}, err
	}
	for _, ch := range s.sub[session] {
		select {
		case ch <- ev:
		default:
			// drop if subscriber is slow — they can resume from seq
		}
	}
	return ev, nil
}

// Replay returns events with seq > from.
func (s *Store) Replay(session string, from int64) ([]coord.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadSeqLocked(session); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(session))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []coord.Event
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		var ev coord.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Seq > from {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}

// SubscribeLive registers for new events; caller must Unsubscribe.
func (s *Store) SubscribeLive(session string) chan coord.Event {
	ch := make(chan coord.Event, 64)
	s.mu.Lock()
	s.sub[session] = append(s.sub[session], ch)
	s.mu.Unlock()
	return ch
}

func (s *Store) Unsubscribe(session string, ch chan coord.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sub[session]
	for i, c := range list {
		if c == ch {
			s.sub[session] = append(list[:i], list[i+1:]...)
			break
		}
	}
	close(ch)
}

func (s *Store) LastSeq(session string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadSeqLocked(session); err != nil {
		return 0, err
	}
	return s.seq[session], nil
}
