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

// Build identifies the binary that was installed, stamped at link time by
// install.sh with the commit it was built from.
//
// Version above describes the wire format and so is deliberately invariant
// across rebuilds — which meant nothing could tell a relayd installed months
// ago from one installed minutes ago. Ensure() only checks that ping returns
// ok, so a stale remote passes identically to a current one. That is the same
// shape as the stale desktop bridge that silently rejected commands for hours:
// a long-lived process reporting fine while running code nobody remembers
// deploying.
var Build = "dev"

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
	Build   string         `json:"build,omitempty"`
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
