package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A board is the shared, categorized coordination surface for the children of
// one manager. Relay's escalation path is strictly vertical — a child talks
// only to its parent — so peers that need to coordinate (status, resources,
// artifacts, lateral requests) would otherwise round-trip through the manager,
// burdening it with traffic that carries no decision.
//
// Scope is enforced by DERIVATION, not by a permission check: a board id is
// always computed from the caller's own lineage, so a node cannot name another
// subtree's board in the first place. There is nothing to spoof.
//
// The board is a projection over the existing relayd channel log, so posting
// is append-only and querying is a fold — no new daemon, no poll loop.

// BoardEntry is one node's current value for a key. A query returns only the
// latest entry per (node, key): agents pay for current state, not history.
type BoardEntry struct {
	Node     string `json:"node"`
	Category string `json:"category"`
	Key      string `json:"key,omitempty"`
	Text     string `json:"text,omitempty"`
	Seq      int64  `json:"seq"`
	TS       string `json:"ts,omitempty"`
}

// BoardService reads and writes manager-scoped boards.
type BoardService struct {
	Reg *Registry
	Msg *MsgService
}

func boardChannel(managerID, category string) string {
	return "board." + sanitizeID(managerID) + "." + sanitizeID(category)
}

func normalizeCategory(category string) (string, error) {
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		return "", fmt.Errorf("category required")
	}
	for _, r := range category {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("category %q must be lowercase alphanumeric, dash or underscore", category)
	}
	return category, nil
}

// resolveBoard finds the board a session may use: the one owned by its
// manager. A root has no manager and therefore no peers to coordinate with.
func (b *BoardService) resolveBoard(sessionID, category string) (manager *Session, channel string, err error) {
	if b == nil || b.Reg == nil || b.Msg == nil {
		return nil, "", fmt.Errorf("board service not configured")
	}
	category, err = normalizeCategory(category)
	if err != nil {
		return nil, "", err
	}
	sess, err := b.Reg.GetSession(sessionID)
	if err != nil {
		return nil, "", err
	}
	if sess.SourceSessionID == "" {
		return nil, "", fmt.Errorf("session %s is a root and has no peer board", sessionID)
	}
	manager, err = b.Reg.GetSession(sess.SourceSessionID)
	if err != nil {
		return nil, "", err
	}
	return manager, boardChannel(manager.ID, category), nil
}

// Post publishes this session's current value for key. Re-posting the same key
// supersedes the previous value: a board holds state, not a conversation.
func (b *BoardService) Post(ctx context.Context, sessionID, category, key, text string) (int64, error) {
	manager, channel, err := b.resolveBoard(sessionID, category)
	if err != nil {
		return 0, err
	}
	meta := map[string]any{}
	if key = strings.TrimSpace(key); key != "" {
		meta["key"] = key
	}
	return b.Msg.Send(ctx, manager.HostID, channel, "board", sessionID, compactText(text), meta)
}

// Query folds the board to the latest entry per (node, key). Passing key
// narrows to one key; passing peersOnly drops the caller's own entries, which
// is what an agent asking "what is everyone else doing" actually wants.
func (b *BoardService) Query(ctx context.Context, sessionID, category, key string, peersOnly bool) ([]BoardEntry, error) {
	manager, channel, err := b.resolveBoard(sessionID, category)
	if err != nil {
		return nil, err
	}
	msgs, _, err := b.Msg.Read(ctx, manager.HostID, channel, 0, false, 0)
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	latest := map[string]BoardEntry{}
	for _, m := range msgs {
		entry := BoardEntry{Node: m.From, Category: category, Text: m.Text, Seq: m.Seq, TS: m.TS}
		if m.Meta != nil {
			if k, ok := m.Meta["key"].(string); ok {
				entry.Key = k
			}
		}
		if entry.Node == "" {
			continue
		}
		if key != "" && entry.Key != key {
			continue
		}
		if peersOnly && entry.Node == sessionID {
			continue
		}
		id := entry.Node + "\x00" + entry.Key
		if prev, ok := latest[id]; ok && prev.Seq > entry.Seq {
			continue
		}
		latest[id] = entry
	}
	out := make([]BoardEntry, 0, len(latest))
	for _, entry := range latest {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// QuerySubtree rolls up every board beneath this session — its own children's
// board plus those of each descendant manager — in one call.
//
// This is the multi-level view: a manager asking "what is my whole subtree
// doing" would otherwise issue one query per manager and pay for the fan-out
// in its own context. The tool does the walk; the agent gets one folded answer.
// Scope still needs no check: the walk starts at the caller and only descends.
func (b *BoardService) QuerySubtree(ctx context.Context, sessionID, category, key string) ([]BoardEntry, error) {
	if b == nil || b.Reg == nil || b.Msg == nil {
		return nil, fmt.Errorf("board service not configured")
	}
	category, err := normalizeCategory(category)
	if err != nil {
		return nil, err
	}
	if _, err := b.Reg.GetSession(sessionID); err != nil {
		return nil, err
	}
	all, err := b.Reg.ListSessions()
	if err != nil {
		return nil, err
	}
	children := map[string][]*Session{}
	for _, sess := range all {
		if sess.SourceSessionID != "" {
			children[sess.SourceSessionID] = append(children[sess.SourceSessionID], sess)
		}
	}
	key = strings.TrimSpace(key)
	var out []BoardEntry
	seen := map[string]bool{}
	// Breadth-first over managers, bounded like every other lineage walk here.
	frontier := []string{sessionID}
	for depth := 0; depth < maxAncestorDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, managerID := range frontier {
			kids := children[managerID]
			if len(kids) == 0 || seen[managerID] {
				continue
			}
			seen[managerID] = true
			manager, err := b.Reg.GetSession(managerID)
			if err != nil {
				continue
			}
			entries, err := b.readBoard(ctx, manager, category, key)
			if err == nil {
				out = append(out, entries...)
			}
			for _, kid := range kids {
				next = append(next, kid.ID)
			}
		}
		frontier = next
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// readBoard folds one manager's board to the latest entry per (node, key).
func (b *BoardService) readBoard(ctx context.Context, manager *Session, category, key string) ([]BoardEntry, error) {
	msgs, _, err := b.Msg.Read(ctx, manager.HostID, boardChannel(manager.ID, category), 0, false, 0)
	if err != nil {
		return nil, err
	}
	latest := map[string]BoardEntry{}
	for _, m := range msgs {
		entry := BoardEntry{Node: m.From, Category: category, Text: m.Text, Seq: m.Seq, TS: m.TS}
		if m.Meta != nil {
			if k, ok := m.Meta["key"].(string); ok {
				entry.Key = k
			}
		}
		if entry.Node == "" || (key != "" && entry.Key != key) {
			continue
		}
		id := entry.Node + "\x00" + entry.Key
		if prev, ok := latest[id]; ok && prev.Seq > entry.Seq {
			continue
		}
		latest[id] = entry
	}
	out := make([]BoardEntry, 0, len(latest))
	for _, entry := range latest {
		out = append(out, entry)
	}
	return out, nil
}

// Watch blocks until a peer posts to the board, then returns that entry. It is
// a zero-token wait on the existing relayd subscribe stream — never a poll.
func (b *BoardService) Watch(ctx context.Context, sessionID, category string, fromSeq int64, timeout time.Duration) (*BoardEntry, bool, error) {
	manager, channel, err := b.resolveBoard(sessionID, category)
	if err != nil {
		return nil, false, err
	}
	m, timedOut, err := b.Msg.WaitOne(ctx, manager.HostID, []string{channel}, map[string]int64{channel: fromSeq}, timeout)
	if err != nil || timedOut || m == nil {
		return nil, timedOut, err
	}
	entry := &BoardEntry{Node: m.From, Category: category, Text: m.Text, Seq: m.Seq, TS: m.TS}
	if m.Meta != nil {
		if k, ok := m.Meta["key"].(string); ok {
			entry.Key = k
		}
	}
	return entry, false, nil
}
