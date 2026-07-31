package core

import (
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// Session is a durable named work context on a host.
type Session struct {
	ID            string              `json:"id"`
	HostID        string              `json:"host_id"`
	RemoteCWD     string              `json:"remote_cwd"`
	Persist       ports.PersistHandle `json:"persist"`
	RepoRef       string              `json:"repo_ref,omitempty"` // local git root if known
	Labels        map[string]string   `json:"labels,omitempty"`
	Container     *ContainerRef       `json:"container,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
	VizSurfaceRef string              `json:"viz_surface_ref,omitempty"`
	// SourceSessionID and CreatedByHandoffID form the durable relay lineage.
	// They are snapshots rather than pointers into a live pane lifecycle, so
	// history remains meaningful after sessions are finalized.
	SourceSessionID    string `json:"source_session_id,omitempty"`
	SourceHostID       string `json:"source_host_id,omitempty"`
	SourcePersistName  string `json:"source_persist_name,omitempty"`
	CreatedByHandoffID string `json:"created_by_handoff_id,omitempty"`
}

// HandoffKind is agent (interactive CLI) or job (long command).
type HandoffKind string

const (
	KindAgent HandoffKind = "agent"
	KindJob   HandoffKind = "job"
)

// HandoffStatus is the durable state machine.
type HandoffStatus string

const (
	StatusPending    HandoffStatus = "pending"
	StatusRunning    HandoffStatus = "running"
	StatusNeedsInput HandoffStatus = "needs_input"
	StatusDone       HandoffStatus = "done"
	StatusFailed     HandoffStatus = "failed"
	StatusAbandoned  HandoffStatus = "abandoned"
)

// Handoff is a goal-driven remote work unit bound to a session.
type Handoff struct {
	ID                string        `json:"id"`
	SessionID         string        `json:"session_id"`
	HostID            string        `json:"host_id"`
	Kind              HandoffKind   `json:"kind"`
	Status            HandoffStatus `json:"status"`
	Goal              string        `json:"goal,omitempty"`
	Agent             string        `json:"agent,omitempty"`
	Command           string        `json:"command,omitempty"`
	EventsPath        string        `json:"events_path"`
	LastSeq           int64         `json:"last_seq"`
	ExitCode          *int          `json:"exit_code,omitempty"`
	Outcome           string        `json:"outcome,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
	EndedAt           *time.Time    `json:"ended_at,omitempty"`
	SourceSessionID   string        `json:"source_session_id,omitempty"`
	SourceHostID      string        `json:"source_host_id,omitempty"`
	SourcePersistName string        `json:"source_persist_name,omitempty"`
}

// Event is one line from the remote JSONL event log.
// Event is the coordination event on the relayd bus. It is an alias for
// coord.Event (one wire type, not two): kind ∈ started|idle|needs_input|
// ask|note|progress|result|inject|exit|heartbeat, with optional meta.
type Event = coord.Event

// Binding is the JSON handoff returns for agents to re-attach after compaction.
type Binding struct {
	V                 int    `json:"v"`
	HandoffID         string `json:"handoff_id"`
	SessionID         string `json:"session_id"`
	HostID            string `json:"host_id"`
	Kind              string `json:"kind"`
	Goal              string `json:"goal,omitempty"`
	Events            string `json:"events"`
	Watch             string `json:"watch"`
	Pane              bool   `json:"pane"`
	SourceSessionID   string `json:"source_session_id,omitempty"`
	SourceHostID      string `json:"source_host_id,omitempty"`
	SourcePersistName string `json:"source_persist_name,omitempty"`
}
