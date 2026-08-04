package relayd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/shellquote"
)

const maxEventRecordBytes = 1 << 20

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
	events, err := readEventLog(p, true)
	if err != nil {
		if os.IsNotExist(err) {
			b.seq = 0
			return nil
		}
		return err
	}
	var last int64
	for _, ev := range events {
		if ev.Seq > last {
			last = ev.Seq
		}
	}
	b.seq = last
	return nil
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
	if len(raw) > maxEventRecordBytes {
		return coord.Event{}, fmt.Errorf("event record exceeds %d bytes", maxEventRecordBytes)
	}
	p, err := s.path(session)
	if err != nil {
		return coord.Event{}, err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return coord.Event{}, err
	}
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return coord.Event{}, err
	}
	if closeErr != nil {
		return coord.Event{}, closeErr
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
	events, err := readEventLog(p, true)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	var out []coord.Event
	for _, ev := range events {
		if ev.Seq > from {
			out = append(out, ev)
		}
	}
	ch := make(chan coord.Event, 64)
	b.sub = append(b.sub, ch)
	return out, ch, nil
}

// readEventLog accepts the supported bounded JSONL record shape, repairs only
// a conclusively partial final write, and reports interior corruption honestly.
// Callers hold the session bucket lock, so truncation cannot race Emit.
func readEventLog(path string, repairTail bool) ([]coord.Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	var events []coord.Event
	var offset int64
	for recordNumber := 1; ; recordNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			return events, nil
		}
		if len(line) > maxEventRecordBytes {
			return nil, fmt.Errorf("event log record %d exceeds %d bytes", recordNumber, maxEventRecordBytes)
		}
		lineEnd := offset + int64(len(line))
		trimmed := line
		if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\n' {
			trimmed = trimmed[:len(trimmed)-1]
		}
		var event coord.Event
		decodeErr := json.Unmarshal(trimmed, &event)
		if decodeErr == nil && (event.Seq < 1 || event.Sess == "" || event.Kind == "") {
			decodeErr = fmt.Errorf("incomplete event")
		}
		if decodeErr != nil {
			if errors.Is(readErr, io.EOF) && repairTail {
				if err := file.Close(); err != nil {
					return nil, err
				}
				repairFile, err := os.OpenFile(path, os.O_WRONLY, 0o600)
				if err != nil {
					return nil, err
				}
				if err := repairFile.Truncate(offset); err == nil {
					err = repairFile.Sync()
				}
				closeErr := repairFile.Close()
				if err != nil {
					return nil, err
				}
				if closeErr != nil {
					return nil, closeErr
				}
				return events, nil
			}
			return nil, fmt.Errorf("event log record %d is corrupt", recordNumber)
		}
		events = append(events, event)
		offset = lineEnd
		if errors.Is(readErr, io.EOF) {
			if repairTail {
				appendFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					return nil, err
				}
				_, writeErr := appendFile.Write([]byte{'\n'})
				if writeErr == nil {
					writeErr = appendFile.Sync()
				}
				closeErr := appendFile.Close()
				if writeErr != nil {
					return nil, writeErr
				}
				if closeErr != nil {
					return nil, closeErr
				}
			}
			return events, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
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
