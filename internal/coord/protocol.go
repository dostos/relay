// Package coord defines the relayd event protocol shared by server and clients.
package coord

import "time"

const (
	SocketRel = ".local/state/relay/relayd.sock"
	StateRel  = ".local/state/relay"
	EventsRel = ".local/state/relay/events"
	BinName   = "relayd"

	HeartbeatInterval = 15 * time.Second
	Version           = "0.1.0"
)

// Request is a single newline-delimited JSON request to relayd.
type Request struct {
	Op      string         `json:"op"` // ping|status|emit|subscribe
	Session string         `json:"session,omitempty"`
	Kind    string         `json:"kind,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
	From    int64          `json:"from,omitempty"`
	Follow  bool           `json:"follow,omitempty"`
}

// Response is a one-shot reply (ping/status/emit ack).
type Response struct {
	OK      bool           `json:"ok"`
	Error   string         `json:"error,omitempty"`
	Seq     int64          `json:"seq,omitempty"`
	Version string         `json:"version,omitempty"`
	Uptime  string         `json:"uptime,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// Event is one streamed/log event line.
type Event struct {
	TS        string         `json:"ts"`
	Seq       int64          `json:"seq"`
	Sess      string         `json:"sess"`
	Kind      string         `json:"kind"`
	Meta      map[string]any `json:"meta,omitempty"`
	Heartbeat bool           `json:"heartbeat,omitempty"`
}
