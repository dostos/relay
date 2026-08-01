package core

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// BridgeRemoteSocket is the owner-only stream-local endpoint exposed inside a
// relay tmux session. The SSH attach maps it to the desktop bridge socket.
func BridgeRemoteSocket(sessionID string) string {
	return "/tmp/relay-bridge-" + sanitizeID(sessionID) + ".sock"
}

func relaySessionCommand(command, sessionID, hostID, persistName, bridgeToken string) string {
	exports := []string{
		"RELAY_SESSION_ID=" + shellquote.Quote(sessionID),
		"RELAY_SESSION_HOST=" + shellquote.Quote(hostID),
		"RELAY_SESSION_NAME=" + shellquote.Quote(persistName),
		bridge.SocketEnv + "=" + shellquote.Quote(BridgeRemoteSocket(sessionID)),
		bridge.SourceTokenEnv + "=" + shellquote.Quote(bridgeToken),
	}
	return "export " + strings.Join(exports, " ") + "; exec " + command
}

func bridgeTokenPath(sessionID string) string {
	return filepath.Join(BridgeTokensDir(), sanitizeID(sessionID)+".token")
}

// BridgeIdentity is the owner-only fallback used by already-running adopted
// tmux sessions. New Relay sessions also receive it so nested handoffs keep
// working after an agent replaces itself without inheriting the launch env.
type BridgeIdentity struct {
	V           int    `json:"v"`
	SessionID   string `json:"session_id"`
	HostID      string `json:"host_id"`
	PersistName string `json:"persist_name"`
	Socket      string `json:"socket"`
	Token       string `json:"token"`
}

func remoteBridgeIdentityPath(sessionID string) string {
	return "~/" + RemoteStateRel + "/bridge-identities/" + sanitizeID(sessionID) + ".json"
}

func bridgeIdentityPath(sessionID string) string {
	return filepath.Join(BridgeIdentitiesDir(), sanitizeID(sessionID)+".json")
}

func provisionBridgeIdentity(ctx context.Context, t ports.Transport, sess *Session, token string) error {
	if sess == nil || t == nil || token == "" {
		return fmt.Errorf("bridge identity requires session, transport, and token")
	}
	identity := BridgeIdentity{
		V: 1, SessionID: sess.ID, HostID: sess.HostID, PersistName: sess.Persist.Name,
		Socket: BridgeRemoteSocket(sess.ID), Token: token,
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	return t.WriteFile(ctx, remoteBridgeIdentityPath(sess.ID), raw, "600")
}

func clearBridgeIdentity(ctx context.Context, t ports.Transport, sessionID string) {
	// Overwrite instead of shell-removing: this revokes the secret while keeping
	// cleanup inside Transport's bounded file API.
	_ = t.WriteFile(ctx, remoteBridgeIdentityPath(sessionID), []byte("{}\n"), "600")
}

// LoadBridgeIdentityForCurrentPane discovers the tmux session containing the
// caller, then loads only that session's owner-readable identity. It is used
// when an adopted/rerolled agent process lacks Relay's original launch env.
func LoadBridgeIdentityForCurrentPane() (*BridgeIdentity, error) {
	pane := strings.TrimSpace(os.Getenv("TMUX_PANE"))
	if pane == "" {
		return nil, fmt.Errorf("not running in tmux")
	}
	out, err := exec.Command("tmux", "display-message", "-p", "-t", pane, "#{session_name}").Output()
	if err != nil {
		return nil, err
	}
	return loadBridgeIdentityForPersist(strings.TrimSpace(string(out)))
}

func loadBridgeIdentityForPersist(persistName string) (*BridgeIdentity, error) {
	entries, err := os.ReadDir(BridgeIdentitiesDir())
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(BridgeIdentitiesDir(), entry.Name()))
		if readErr != nil {
			continue
		}
		var identity BridgeIdentity
		if json.Unmarshal(raw, &identity) == nil && identity.PersistName == persistName && identity.SessionID != "" && identity.Socket != "" && identity.Token != "" {
			return &identity, nil
		}
	}
	return nil, fmt.Errorf("no bridge identity for tmux session %q", persistName)
}

func rememberBridgeToken(sessionID, token string) error {
	if sessionID == "" || token == "" {
		return fmt.Errorf("bridge session and token required")
	}
	if err := EnsureStateDirs(); err != nil {
		return err
	}
	return os.WriteFile(bridgeTokenPath(sessionID), []byte(token), 0o600)
}

func forgetBridgeToken(sessionID string) {
	_ = os.Remove(bridgeTokenPath(sessionID))
}

// AuthorizeBridgeSource binds a forwarded request to the unguessable token
// injected into its originating tmux session.
func AuthorizeBridgeSource(source bridge.Source) error {
	if source.SessionID == "" || source.Token == "" {
		return fmt.Errorf("relay bridge source identity missing")
	}
	raw, err := os.ReadFile(bridgeTokenPath(source.SessionID))
	if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(raw))), []byte(source.Token)) != 1 {
		return fmt.Errorf("relay bridge source identity rejected")
	}
	if _, err := (&Registry{}).GetSession(source.SessionID); err != nil {
		return fmt.Errorf("relay bridge source session is not active locally")
	}
	return nil
}

// AppendSessionStart adds the durable node snapshot used by relay history.
func AppendSessionStart(sess *Session) error {
	if sess == nil {
		return nil
	}
	record := map[string]any{
		"v": 1, "type": "session_start", "ts": sess.CreatedAt.Format(time.RFC3339),
		"session_id": sess.ID, "host_id": sess.HostID, "persist_name": sess.Persist.Name,
		"repo_ref": sess.RepoRef, "remote_cwd": sess.RemoteCWD,
		"source_session_id": sess.SourceSessionID, "source_host_id": sess.SourceHostID,
		"source_persist_name":   sess.SourcePersistName,
		"created_by_handoff_id": sess.CreatedByHandoffID,
	}
	if role := sess.Labels["role"]; role != "" {
		record["role"] = role
	}
	if agent := sess.Labels["agent"]; agent != "" {
		record["agent"] = agent
	}
	return AppendLedger(record)
}

// AppendSessionRename preserves the stable session node while allowing
// history displays to use its current durable name.
func AppendSessionRename(sessionID, oldName, newName string) error {
	if sessionID == "" || oldName == "" || newName == "" || oldName == newName {
		return nil
	}
	return AppendLedger(map[string]any{
		"v": 1, "type": "session_rename", "ts": time.Now().UTC().Format(time.RFC3339),
		"session_id": sessionID, "old_persist_name": oldName, "persist_name": newName,
	})
}

// AppendRelayEdge records a new transition into an already-existing named
// session. Session creation already writes the initial edge itself.
func AppendRelayEdge(sourceSessionID, targetSessionID string) error {
	return AppendRelayHandoffEdge(sourceSessionID, targetSessionID, "")
}

// AppendRelayHandoffEdge records a migration edge with the goal handoff that
// owns it. It is used when adopting work created before a parent was known.
func AppendRelayHandoffEdge(sourceSessionID, targetSessionID, handoffID string) error {
	if sourceSessionID == "" || targetSessionID == "" {
		return nil
	}
	record := map[string]any{
		"v": 1, "type": "relay_edge", "ts": time.Now().UTC().Format(time.RFC3339),
		"source_session_id": sourceSessionID, "target_session_id": targetSessionID,
	}
	if handoffID != "" {
		record["handoff_id"] = handoffID
	}
	return AppendLedger(record)
}

// HistoryNode is a durable session snapshot.
type HistoryNode struct {
	SessionID   string    `json:"session_id"`
	HostID      string    `json:"host_id"`
	PersistName string    `json:"persist_name"`
	Role        string    `json:"role,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// HistoryEdge records who relayed work to whom.
type HistoryEdge struct {
	SourceSessionID string    `json:"source_session_id"`
	TargetSessionID string    `json:"target_session_id"`
	HandoffID       string    `json:"handoff_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type HistoryCommunication struct {
	Seq             int64     `json:"seq"`
	MessageID       string    `json:"message_id"`
	CorrelationID   string    `json:"correlation_id"`
	ParentSessionID string    `json:"parent_session_id"`
	ChildSessionID  string    `json:"child_session_id"`
	HandoffID       string    `json:"handoff_id,omitempty"`
	Kind            string    `json:"kind"`
	Action          string    `json:"action"`
	PolicyID        string    `json:"policy_id,omitempty"`
	AutoHandled     bool      `json:"auto_handled,omitempty"`
	Summary         string    `json:"summary,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// CommunicationPage is the compact cursor-based view consumed by managers.
// The append-only ledger retains full routing metadata; this projection omits
// graph nodes and transcripts so an agent reads only new goal transitions.
type CommunicationEntry struct {
	Seq           int64  `json:"seq"`
	MessageID     string `json:"message_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
	ChildSession  string `json:"child_session_id"`
	HandoffID     string `json:"handoff_id,omitempty"`
	Kind          string `json:"kind"`
	Action        string `json:"action"`
	Summary       string `json:"summary,omitempty"`
	PolicyID      string `json:"policy_id,omitempty"`
	AutoHandled   bool   `json:"auto_handled,omitempty"`
}

type CommunicationPage struct {
	Entries   []CommunicationEntry `json:"entries"`
	NextAfter int64                `json:"next_after"`
	HasMore   bool                 `json:"has_more"`
}

type HistoryGraph struct {
	Nodes          []HistoryNode          `json:"nodes"`
	Edges          []HistoryEdge          `json:"edges"`
	Communications []HistoryCommunication `json:"communications,omitempty"`
}

// LoadHistory reconstructs lineage from the append-only ledger. Malformed
// legacy lines are ignored; existing handoff-only records remain readable.
func LoadHistory() (*HistoryGraph, error) {
	f, err := os.Open(LedgerPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &HistoryGraph{}, nil
		}
		return nil, err
	}
	defer f.Close()
	nodes := map[string]HistoryNode{}
	var edges []HistoryEdge
	var communications []HistoryCommunication
	var communicationSeq int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)
	for sc.Scan() {
		var raw map[string]any
		if json.Unmarshal(sc.Bytes(), &raw) != nil {
			continue
		}
		typ, _ := raw["type"].(string)
		if typ == "parent_reparent" {
			handoffID := textField(raw, "handoff_id")
			kept := edges[:0]
			for _, edge := range edges {
				if edge.HandoffID != handoffID {
					kept = append(kept, edge)
				}
			}
			edges = kept
			ts, _ := time.Parse(time.RFC3339, textField(raw, "ts"))
			edges = append(edges, HistoryEdge{SourceSessionID: textField(raw, "parent_session_id"), TargetSessionID: textField(raw, "child_session_id"), HandoffID: handoffID, CreatedAt: ts})
			continue
		}
		if typ == "communication" {
			communicationSeq++
			ts, _ := time.Parse(time.RFC3339, textField(raw, "ts"))
			communications = append(communications, HistoryCommunication{
				Seq:       communicationSeq,
				MessageID: textField(raw, "message_id"), CorrelationID: textField(raw, "correlation_id"),
				ParentSessionID: textField(raw, "parent_session_id"), ChildSessionID: textField(raw, "child_session_id"),
				HandoffID: textField(raw, "handoff_id"), Kind: textField(raw, "kind"), Action: textField(raw, "action"),
				PolicyID: textField(raw, "policy_id"), AutoHandled: boolField(raw, "auto_handled"),
				Summary: firstTextField(raw, "summary", "text"), CreatedAt: ts,
			})
			continue
		}
		if typ == "session_rename" {
			id := textField(raw, "session_id")
			if node, ok := nodes[id]; ok {
				node.PersistName = textField(raw, "persist_name")
				nodes[id] = node
			}
			continue
		}
		if typ == "relay_edge" {
			ts, _ := time.Parse(time.RFC3339, textField(raw, "ts"))
			edges = append(edges, HistoryEdge{
				SourceSessionID: textField(raw, "source_session_id"),
				TargetSessionID: textField(raw, "target_session_id"), HandoffID: textField(raw, "handoff_id"), CreatedAt: ts,
			})
			continue
		}
		if typ != "session_start" {
			continue
		}
		id := textField(raw, "session_id")
		if id == "" {
			continue
		}
		if textField(raw, "role") == "auth" {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, textField(raw, "ts"))
		nodes[id] = HistoryNode{
			SessionID: id, HostID: textField(raw, "host_id"), PersistName: textField(raw, "persist_name"),
			Role: textField(raw, "role"), Agent: textField(raw, "agent"), CreatedAt: ts,
		}
		if source := textField(raw, "source_session_id"); source != "" {
			edges = append(edges, HistoryEdge{
				SourceSessionID: source, TargetSessionID: id,
				HandoffID: textField(raw, "created_by_handoff_id"), CreatedAt: ts,
			})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	graph := &HistoryGraph{Edges: edges, Communications: communications}
	for _, node := range nodes {
		graph.Nodes = append(graph.Nodes, node)
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].CreatedAt.Before(graph.Nodes[j].CreatedAt) })
	sort.Slice(graph.Edges, func(i, j int) bool { return graph.Edges[i].CreatedAt.Before(graph.Edges[j].CreatedAt) })
	sort.Slice(graph.Communications, func(i, j int) bool { return graph.Communications[i].Seq < graph.Communications[j].Seq })
	return graph, nil
}

func firstTextField(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := textField(raw, key); value != "" {
			return value
		}
	}
	return ""
}

// LoadCommunicationPage returns only new communication transitions for one
// manager. Cursor values are stable append-only communication sequence IDs;
// callers persist next_after and never reread prior history.
func LoadCommunicationPage(parentID, handoffID string, after int64, limit int) (*CommunicationPage, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	graph, err := LoadHistory()
	if err != nil {
		return nil, err
	}
	page := &CommunicationPage{Entries: []CommunicationEntry{}, NextAfter: after}
	for _, entry := range graph.Communications {
		if entry.Seq <= after {
			continue
		}
		if entry.ParentSessionID != parentID || (handoffID != "" && entry.HandoffID != handoffID) {
			if entry.Seq > page.NextAfter {
				page.NextAfter = entry.Seq
			}
			continue
		}
		if len(page.Entries) >= limit {
			page.HasMore = true
			break
		}
		page.Entries = append(page.Entries, CommunicationEntry{
			Seq: entry.Seq, MessageID: entry.MessageID, CorrelationID: entry.CorrelationID,
			ChildSession: entry.ChildSessionID, HandoffID: entry.HandoffID,
			Kind: entry.Kind, Action: entry.Action, Summary: entry.Summary,
			PolicyID: entry.PolicyID, AutoHandled: entry.AutoHandled,
		})
		page.NextAfter = entry.Seq
	}
	return page, nil
}

func textField(raw map[string]any, key string) string {
	v, _ := raw[key].(string)
	return v
}

func boolField(raw map[string]any, key string) bool {
	v, _ := raw[key].(bool)
	return v
}

func sessionLabel(n HistoryNode) string {
	label := n.HostID + "/" + n.PersistName
	if n.Agent != "" {
		label += " (" + n.Agent + ")"
	}
	return label
}

func FormatHistory(graph *HistoryGraph) string {
	if graph == nil || len(graph.Nodes) == 0 {
		return "No relay history.\n"
	}
	byID := map[string]HistoryNode{}
	incoming := map[string]bool{}
	children := map[string][]HistoryEdge{}
	for _, n := range graph.Nodes {
		byID[n.SessionID] = n
	}
	for _, e := range graph.Edges {
		if _, sourceKnown := byID[e.SourceSessionID]; sourceKnown {
			incoming[e.TargetSessionID] = true
			children[e.SourceSessionID] = append(children[e.SourceSessionID], e)
		}
	}
	var roots []HistoryNode
	for _, n := range graph.Nodes {
		if !incoming[n.SessionID] {
			roots = append(roots, n)
		}
	}
	if len(roots) == 0 {
		roots = graph.Nodes
	}
	var out strings.Builder
	var walk func(string, string, map[string]bool)
	walk = func(id, indent string, path map[string]bool) {
		if path[id] {
			fmt.Fprintf(&out, "%s%s (cycle)\n", indent, id)
			return
		}
		path[id] = true
		defer delete(path, id)
		for _, edge := range children[id] {
			target, ok := byID[edge.TargetSessionID]
			if !ok {
				continue
			}
			via := "relay"
			if edge.HandoffID != "" {
				via = edge.HandoffID
			}
			fmt.Fprintf(&out, "%s└─[%s]→ %s\n", indent, via, sessionLabel(target))
			walk(target.SessionID, indent+"  ", path)
		}
	}
	for _, root := range roots {
		fmt.Fprintln(&out, sessionLabel(root))
		walk(root.SessionID, "", map[string]bool{})
	}
	if len(graph.Communications) > 0 {
		fmt.Fprintln(&out, "Communications:")
		for _, message := range graph.Communications {
			policy := ""
			if message.PolicyID != "" {
				policy = " [" + message.PolicyID + "]"
			}
			fmt.Fprintf(&out, "  %s %s %s (%s)%s\n", message.MessageID, message.Action, message.Kind, message.CorrelationID, policy)
		}
	}
	return out.String()
}
