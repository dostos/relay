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
	"github.com/dostos/relay/internal/shellquote"
)

// sessionBucket holds per-session seq + subscribers.
type sessionBucket struct {
	mu  sync.Mutex
	seq int64
	sub []chan coord.Event
}

// Store is an append-only per-session JSONL event log with monotonic seq.
// Sessions are isolated under per-session locks; the map lock is only for lookup.
type Store struct {
	dir string
	mu  sync.Mutex
	ss  map[string]*sessionBucket
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{
		dir: dir,
		ss:  map[string]*sessionBucket{},
	}, nil
}

func (s *Store) path(session string) (string, error) {
	if err := shellquote.ValidateSessionName(session); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, session+".jsonl"), nil
}

func (s *Store) bucket(session string) *sessionBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.ss[session]
	if !ok {
		b = &sessionBucket{seq: -1} // -1 = not loaded from disk yet
		s.ss[session] = b
	}
	return b
}

func (s *Store) loadSeqLocked(b *sessionBucket, session string) error {
	if b.seq >= 0 {
		return nil
	}
	p, err := s.path(session)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			b.seq = 0
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
	b.seq = last
	return sc.Err()
}

// Emit appends an event and notifies subscribers.
// Seq advances only after a successful disk write so failures do not gap the stream.
func (s *Store) Emit(session, kind string, meta map[string]any) (coord.Event, error) {
	if session == "" || kind == "" {
		return coord.Event{}, fmt.Errorf("session and kind required")
	}
	b := s.bucket(session)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := s.loadSeqLocked(b, session); err != nil {
		return coord.Event{}, err
	}
	next := b.seq + 1
	ev := coord.Event{
		TS:   time.Now().UTC().Format(time.RFC3339),
		Seq:  next,
		Sess: session,
		Kind: kind,
		Meta: meta,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return coord.Event{}, err
	}
	p, err := s.path(session)
	if err != nil {
		return coord.Event{}, err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return coord.Event{}, err
	}
	_, err = f.Write(append(raw, '\n'))
	_ = f.Close()
	if err != nil {
		return coord.Event{}, err
	}
	b.seq = next
	for _, ch := range b.sub {
		select {
		case ch <- ev:
		default:
			// slow subscriber — client resumes from seq on reconnect
		}
	}
	return ev, nil
}

// ReplayAndSubscribe registers for live events THEN replays seq>from under one
// session lock so emits cannot slip between replay and subscribe.
func (s *Store) ReplayAndSubscribe(session string, from int64) ([]coord.Event, chan coord.Event, error) {
	b := s.bucket(session)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := s.loadSeqLocked(b, session); err != nil {
		return nil, nil, err
	}
	p, err := s.path(session)
	if err != nil {
		return nil, nil, err
	}
	var out []coord.Event
	f, err := os.Open(p)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if f != nil {
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
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return nil, nil, err
		}
	}
	ch := make(chan coord.Event, 64)
	b.sub = append(b.sub, ch)
	return out, ch, nil
}

func (s *Store) Unsubscribe(session string, ch chan coord.Event) {
	b := s.bucket(session)
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, c := range b.sub {
		if c == ch {
			b.sub = append(b.sub[:i], b.sub[i+1:]...)
			break
		}
	}
	close(ch)
}

func (s *Store) LastSeq(session string) (int64, error) {
	b := s.bucket(session)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := s.loadSeqLocked(b, session); err != nil {
		return 0, err
	}
	return b.seq, nil
}
