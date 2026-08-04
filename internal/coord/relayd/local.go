package relayd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/dostos/relay/internal/coord"
)

// Dial connects to the local Unix socket.
func Dial(sockPath string) (net.Conn, error) {
	return net.DialTimeout("unix", sockPath, 2*time.Second)
}

// PingLocal checks the local daemon.
func PingLocal(sockPath string) (*coord.Response, error) {
	return roundTrip(sockPath, coord.Request{Op: "ping"})
}

// EmitLocal emits via the local socket.
func EmitLocal(sockPath, session, kind string, meta map[string]any) (*coord.Response, error) {
	return roundTrip(sockPath, coord.Request{Op: "emit", Session: session, Kind: kind, Meta: meta})
}

// StatusLocal returns daemon status.
func StatusLocal(sockPath string) (*coord.Response, error) {
	return roundTrip(sockPath, coord.Request{Op: "status"})
}

func roundTrip(sockPath string, req coord.Request) (*coord.Response, error) {
	c, err := Dial(sockPath)
	if err != nil {
		return nil, fmt.Errorf("relay event service not running (socket %s): %w — run: relay service event run or relay host bootstrap", sockPath, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		return nil, fmt.Errorf("no response from relay event service")
	}
	var resp coord.Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return &resp, fmt.Errorf("relay event service: %s", resp.Error)
	}
	return &resp, nil
}

// SubscribeLocal streams events from the local socket to w.
func SubscribeLocal(sockPath, session string, from int64, follow bool, w io.Writer) error {
	c, err := Dial(sockPath)
	if err != nil {
		return fmt.Errorf("relay event service not running (socket %s): %w", sockPath, err)
	}
	defer c.Close()
	req := coord.Request{Op: "subscribe", Session: session, From: from, Follow: follow}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(c)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if first {
			first = false
			var probe map[string]any
			if json.Unmarshal(line, &probe) == nil {
				if ok, has := probe["ok"].(bool); has && !ok {
					errMsg, _ := probe["error"].(string)
					return fmt.Errorf("relay event service: %s", errMsg)
				}
			}
		}
		if _, err := w.Write(append(append([]byte{}, line...), '\n')); err != nil {
			return err
		}
	}
	return sc.Err()
}
