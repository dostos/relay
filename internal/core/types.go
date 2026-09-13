package core

import (
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// Session is a durable named work context on a host.
type Session struct {
	ID        string              `json:"id"`
	HostID    string              `json:"host_id"`
	RemoteCWD string              `json:"remote_cwd"`
	Persist   ports.PersistHandle `json:"persist"`
	RepoRef   string              `json:"repo_ref,omitempty"`  // local git root if known
	RepoRefs  []string            `json:"repo_refs,omitempty"` // cleanup scope for local parents
	Labels    map[string]string   `json:"labels,omitempty"`
	Container *ContainerRef       `json:"container,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	// VizSurfaceRef is the presenter surface an older relay bound this session to.
	// Read for compatibility with existing state files; nothing writes it any
	// more, because relay has no presenter (Forge attaches with `relay resume`).
	VizSurfaceRef string `json:"viz_surface_ref,omitempty"`
	// SourceSessionID records the pane a session was started from (the
	// launch edge). It is a fact about how the session came to be, not an
	// ownership relation: nothing routes or refuses on it.
	SourceSessionID   string `json:"source_session_id,omitempty"`
	SourceHostID      string `json:"source_host_id,omitempty"`
	SourcePersistName string `json:"source_persist_name,omitempty"`
}

const LocalHostID = "local"

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
