package relayd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dostos/relay/internal/coord"
)

// Server is the always-on Unix-socket relayd (NO TCP — net.Listen("unix", …) only).
type Server struct {
	SockPath string
	Store    *Store
	started  time.Time
	ln       net.Listener
	lock     *os.File
}

// DefaultPaths returns socket and events dir under $HOME.
func DefaultPaths() (sock, events string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	sock = filepath.Join(home, coord.SocketRel)
	events = filepath.Join(home, coord.EventsRel)
	return sock, events, nil
}

// Serve listens on a Unix socket only.
func (s *Server) Serve() error {
	if s.SockPath == "" {
		return fmt.Errorf("socket path required")
	}
	if err := os.MkdirAll(filepath.Dir(s.SockPath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.SockPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("relayd already owns %s", s.SockPath)
	}
	s.lock = lock
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		s.lock = nil
	}()
	if conn, dialErr := net.DialTimeout("unix", s.SockPath, 300*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("relayd already listening at %s", s.SockPath)
	}
	if err := os.Remove(s.SockPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", s.SockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.SockPath, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	s.ln = ln
	s.started = time.Now()
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) Close() error {
	if s.ln != nil {
		_ = os.Remove(s.SockPath)
		return s.ln.Close()
	}
	return nil
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
	sc := bufio.NewScanner(c)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	if !sc.Scan() {
		return
	}
	var req coord.Request
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		writeJSON(c, coord.Response{OK: false, Error: "bad request"})
		return
	}
	switch req.Op {
	case "ping":
		writeJSON(c, coord.Response{OK: true, Version: coord.Version, Build: coord.Build})
	case "status":
		writeJSON(c, coord.Response{
			OK:      true,
			Version: coord.Version,
			Build:   coord.Build,
			Uptime:  time.Since(s.started).Round(time.Second).String(),
		})
	case "emit":
		ev, err := s.Store.Emit(req.Session, req.Kind, req.Meta)
		if err != nil {
			writeJSON(c, coord.Response{OK: false, Error: err.Error()})
			return
		}
		writeJSON(c, coord.Response{OK: true, Seq: ev.Seq})
	case "subscribe":
		_ = c.SetDeadline(time.Time{})
		s.subscribe(c, req.Session, req.From, req.Follow)
	default:
		writeJSON(c, coord.Response{OK: false, Error: "unknown op"})
	}
}

func (s *Server) subscribe(c net.Conn, session string, from int64, follow bool) {
	events, live, err := s.Store.ReplayAndSubscribe(session, from)
	if err != nil {
		writeJSON(c, coord.Response{OK: false, Error: err.Error()})
		return
	}
	if follow {
		defer s.Store.Unsubscribe(session, live)
	} else {
		s.Store.Unsubscribe(session, live)
		live = nil
	}
	enc := json.NewEncoder(c)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
	if !follow || live == nil {
		return
	}
	tick := time.NewTicker(coord.HeartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-live:
			if !ok {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
		case <-tick.C:
			hb := coord.Event{
				TS:        time.Now().UTC().Format(time.RFC3339),
				Seq:       0,
				Sess:      session,
				Kind:      "heartbeat",
				Heartbeat: true,
			}
			if err := enc.Encode(hb); err != nil {
				return
			}
		}
	}
}

func writeJSON(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	_, _ = c.Write(append(b, '\n'))
}
