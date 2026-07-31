package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

const parentTextLimit = 640

type ParentMessageState string

const (
	ParentMessagePending ParentMessageState = "pending"
	ParentMessageReplied ParentMessageState = "replied"
	ParentMessageAcked   ParentMessageState = "acked"
)

// ParentMessage is a compact, durable child-to-parent envelope. EventSeq plus
// the handoff id is its idempotency key; transcripts never enter this store.
type ParentMessage struct {
	V               int                `json:"v"`
	ID              string             `json:"id"`
	CorrelationID   string             `json:"correlation_id"`
	ParentSessionID string             `json:"parent_session_id"`
	ChildSessionID  string             `json:"child_session_id"`
	HandoffID       string             `json:"handoff_id"`
	EventSeq        int64              `json:"event_seq"`
	Kind            string             `json:"kind"`
	Text            string             `json:"text,omitempty"`
	State           ParentMessageState `json:"state"`
	CreatedAt       time.Time          `json:"created_at"`
	DeliveredAt     *time.Time         `json:"delivered_at,omitempty"`
	Reply           string             `json:"reply,omitempty"`
	RepliedAt       *time.Time         `json:"replied_at,omitempty"`
	AckedAt         *time.Time         `json:"acked_at,omitempty"`
}

// ParentInboxItem is the turn-level projection of a durable parent message.
// Full timestamps, routing identity, and event cursors remain on disk and in
// history; an orchestrator receives only what it needs for one decision.
type ParentInboxItem struct {
	ID             string             `json:"id"`
	HandoffID      string             `json:"handoff_id"`
	ChildSessionID string             `json:"child_session_id"`
	CorrelationID  string             `json:"correlation_id,omitempty"`
	Kind           string             `json:"kind"`
	Text           string             `json:"text,omitempty"`
	State          ParentMessageState `json:"state,omitempty"`
	Reply          string             `json:"reply,omitempty"`
	Next           string             `json:"next"`
	Argv           []string           `json:"argv"`
}

func CompactParentMessage(msg *ParentMessage, includeState bool) ParentInboxItem {
	next := "ack"
	argv := []string{"relay", "parent", "ack", msg.ID}
	if msg.Kind == "ask" || msg.Kind == "permission_required" {
		next = "reply"
		argv = []string{"relay", "parent", "reply", msg.ID, "--", "<decision>"}
	}
	item := ParentInboxItem{
		ID: msg.ID, HandoffID: msg.HandoffID, ChildSessionID: msg.ChildSessionID,
		CorrelationID: msg.CorrelationID, Kind: msg.Kind, Text: msg.Text,
		Next: next, Argv: argv,
	}
	if includeState {
		item.State = msg.State
		item.Reply = msg.Reply
	}
	return item
}

type ParentNotice struct {
	MessageID string
	Kind      string
	Child     string
	Text      string
	Action    string
}

// ParentNotifier is implemented by cmux without making core depend on it.
type ParentNotifier interface {
	BindLocalParent(context.Context, string, string) (string, error)
	NotifyParent(context.Context, string, ParentNotice) error
}

type ParentEventRouter interface {
	RouteChildEvent(context.Context, *Handoff, coord.Event) (*ParentMessage, error)
}

type ParentService struct {
	Reg          *Registry
	Sessions     *SessionService
	Coord        ports.Coord
	Viz          ports.Viz
	Notifier     ParentNotifier
	NewTransport TransportFactory
}

type RegisterParentOpts struct {
	Surface  string
	Name     string
	RepoRefs []string
	WakeMode string
}

func isLocalParent(sess *Session) bool {
	return sess != nil && sess.HostID == LocalHostID && sess.Persist.Kind == LocalPersistKind && sess.Labels["role"] == ParentRole
}

func normalizeRepoRefs(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if abs, err := filepath.Abs(value); err == nil {
			value = abs
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func (p *ParentService) RegisterLocal(ctx context.Context, opts RegisterParentOpts) (*Session, bool, error) {
	if p.Reg == nil || p.Notifier == nil {
		return nil, false, fmt.Errorf("parent registry and notifier required")
	}
	surface := strings.TrimSpace(opts.Surface)
	if surface == "" {
		var err error
		surface, err = CurrentSurface()
		if err != nil {
			return nil, false, err
		}
	}
	if !strings.HasPrefix(surface, "surface:") {
		surface = "surface:" + surface
	}
	refs := normalizeRepoRefs(opts.RepoRefs)
	if len(refs) == 0 {
		if root, err := gitRoot(""); err == nil {
			refs = []string{root}
		}
	}
	wakeMode := strings.TrimSpace(opts.WakeMode)
	if wakeMode == "" {
		wakeMode = "inject"
	}
	if wakeMode != "inject" && wakeMode != "notify" {
		return nil, false, fmt.Errorf("wake mode must be inject or notify")
	}
	if list, err := p.Reg.ListSessions(); err == nil {
		for _, sess := range list {
			if !isLocalParent(sess) || sess.VizSurfaceRef != surface {
				continue
			}
			if len(refs) > 0 {
				sess.RepoRefs = refs
				sess.RepoRef = refs[0]
			}
			if sess.Labels == nil {
				sess.Labels = map[string]string{}
			}
			sess.Labels["wake_mode"] = wakeMode
			if err := p.Reg.PutSession(sess); err != nil {
				return nil, false, err
			}
			if _, err := p.Notifier.BindLocalParent(ctx, sess.ID, surface); err != nil {
				return nil, false, err
			}
			return sess, false, nil
		}
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		base := "workspace"
		if len(refs) > 0 {
			base = filepath.Base(refs[0])
		}
		name = "local-" + base + "-" + strings.TrimPrefix(newID("p"), "p-")[:6]
	}
	name, err := shellquote.SanitizeSessionName(name)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	sess := &Session{
		ID: newID("sess"), HostID: LocalHostID,
		Persist:  ports.PersistHandle{Kind: LocalPersistKind, Name: name},
		Labels:   map[string]string{"role": ParentRole, "local": "true", "parent_state": "active", "wake_mode": wakeMode},
		RepoRefs: refs, CreatedAt: now, UpdatedAt: now, VizSurfaceRef: surface,
	}
	if len(refs) > 0 {
		sess.RepoRef, sess.RemoteCWD = refs[0], refs[0]
	}
	if err := p.Reg.PutSession(sess); err != nil {
		return nil, false, err
	}
	if _, err := p.Notifier.BindLocalParent(ctx, sess.ID, surface); err != nil {
		_ = p.Reg.DeleteSession(sess.ID)
		return nil, false, err
	}
	if err := AppendSessionStart(sess); err != nil {
		return nil, false, err
	}
	_ = AppendLedger(map[string]any{"v": 1, "type": "parent_register", "ts": now.Format(time.RFC3339), "session_id": sess.ID, "surface": surface, "repo_refs": refs})
	return sess, true, nil
}

// LinkChild adopts an already-running handoff into a local parent's durable
// goal tree. This is intentionally a one-time lineage operation: moving a
// child between parents would make request routing and history ambiguous.
func (p *ParentService) LinkChild(parentID, handoffID string) (*Handoff, error) {
	parent, err := p.Reg.GetSession(parentID)
	if err != nil {
		return nil, err
	}
	if !isLocalParent(parent) {
		return nil, fmt.Errorf("session %s is not a local parent", parentID)
	}
	ho, err := p.Reg.GetHandoff(handoffID)
	if err != nil {
		return nil, err
	}
	if ho.SourceSessionID != "" && ho.SourceSessionID != parentID {
		return nil, fmt.Errorf("handoff %s already belongs to parent %s", handoffID, ho.SourceSessionID)
	}
	if ho.SourceSessionID == parentID {
		return ho, nil
	}
	child, err := p.Reg.GetSession(ho.SessionID)
	if err != nil {
		return nil, err
	}
	ho.SourceSessionID = parent.ID
	ho.SourceHostID = parent.HostID
	ho.SourcePersistName = parent.Persist.Name
	child.SourceSessionID = parent.ID
	child.SourceHostID = parent.HostID
	child.SourcePersistName = parent.Persist.Name
	child.CreatedByHandoffID = ho.ID
	if err := p.Reg.PutSession(child); err != nil {
		return nil, err
	}
	if err := p.Reg.PutHandoff(ho); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_ = AppendRelayHandoffEdge(parent.ID, child.ID, ho.ID)
	_ = AppendLedger(map[string]any{
		"v": 1, "type": "parent_link", "ts": now.Format(time.RFC3339),
		"parent_session_id": parent.ID, "child_session_id": child.ID, "handoff_id": ho.ID,
	})
	return ho, nil
}

func gitRoot(dir string) (string, error) {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func parentMessageID(parentID, handoffID, kind string, seq int64) string {
	sum := sha256.Sum256([]byte(parentID + "\x00" + handoffID + "\x00" + kind + "\x00" + strconv.FormatInt(seq, 10)))
	return "pm-" + hex.EncodeToString(sum[:8])
}

func parentMessageDir(parentID string) string {
	return filepath.Join(ParentInboxDir(), sanitizeID(parentID))
}

func parentMessagePath(parentID, messageID string) string {
	return filepath.Join(parentMessageDir(parentID), sanitizeID(messageID)+".json")
}

func compactText(text string) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if len(text) > parentTextLimit {
		text = text[:parentTextLimit-3] + "..."
	}
	return text
}

func writeParentMessage(msg *ParentMessage, exclusive bool) error {
	if msg == nil || msg.ParentSessionID == "" || msg.ID == "" {
		return fmt.Errorf("parent message identity required")
	}
	dir := parentMessageDir(msg.ParentSessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return err
	}
	path := parentMessagePath(msg.ParentSessionID, msg.ID)
	if exclusive {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(raw)
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readParentMessage(path string) (*ParentMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var msg ParentMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (p *ParentService) ListMessages(parentID string, pendingOnly bool) ([]*ParentMessage, error) {
	dir := parentMessageDir(parentID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []*ParentMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]*ParentMessage, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		msg, err := readParentMessage(filepath.Join(dir, entry.Name()))
		if err != nil || pendingOnly && msg.State != ParentMessagePending {
			continue
		}
		out = append(out, msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (p *ParentService) FindMessage(id string) (*ParentMessage, error) {
	parents, err := os.ReadDir(ParentInboxDir())
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("parent message %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	for _, parent := range parents {
		if !parent.IsDir() {
			continue
		}
		path := filepath.Join(ParentInboxDir(), parent.Name(), sanitizeID(id)+".json")
		if msg, err := readParentMessage(path); err == nil {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("parent message %q not found", id)
}

func eventString(meta map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := meta[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func attentionKind(ev coord.Event) string {
	if ev.Kind == "permission_required" {
		return "permission_required"
	}
	reason := strings.ToLower(eventString(ev.Meta, "reason", "type", "category"))
	if strings.Contains(reason, "permission") || strings.Contains(reason, "approval") || strings.Contains(reason, "accept") {
		return "permission_required"
	}
	switch ev.Kind {
	case "ask", "needs_input", "idle":
		return "ask"
	case "result", "exit":
		return ev.Kind
	default:
		return ""
	}
}

func (p *ParentService) childEventText(ctx context.Context, ho *Handoff, ev coord.Event, kind string) string {
	if text := eventString(ev.Meta, "text", "q", "question", "msg", "note"); text != "" {
		return compactText(text)
	}
	if ev.Kind != "idle" && ev.Kind != "needs_input" {
		return compactText(kind + " from child")
	}
	if p.Sessions != nil {
		if capture, err := p.Sessions.Capture(ctx, ho.SessionID, 80); err == nil {
			excerpt := decisionExcerpt(capture)
			if excerpt != "" {
				return "child idle; manager decide blocked/completed: " + excerpt
			}
		}
	}
	return "child idle; manager decide whether it is blocked, complete, or should continue"
}

func decisionExcerpt(capture string) string {
	lines := strings.Split(capture, "\n")
	useful := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Trim(line, "─━═- ") == "" {
			continue
		}
		// Common tmux/agent status bars carry no decision context.
		if strings.HasPrefix(line, "|") && strings.Contains(line, "Auto") && strings.Contains(line, "~/") {
			continue
		}
		useful = append(useful, line)
	}
	if len(useful) > 6 {
		useful = useful[len(useful)-6:]
	}
	return compactText(strings.Join(useful, " | "))
}

func (p *ParentService) RouteChildEvent(ctx context.Context, ho *Handoff, ev coord.Event) (*ParentMessage, error) {
	if ho == nil || ho.SourceSessionID == "" {
		return nil, nil
	}
	kind := attentionKind(ev)
	if kind == "" {
		return nil, nil
	}
	parent, err := p.Reg.GetSession(ho.SourceSessionID)
	if err != nil {
		return nil, err
	}
	id := parentMessageID(parent.ID, ho.ID, kind, ev.Seq)
	correlationID := eventString(ev.Meta, "correlation_id", "request_id")
	if correlationID == "" {
		correlationID = id
	}
	msg := &ParentMessage{
		V: 1, ID: id, CorrelationID: compactText(correlationID),
		ParentSessionID: parent.ID, ChildSessionID: ho.SessionID, HandoffID: ho.ID,
		EventSeq: ev.Seq, Kind: kind, Text: p.childEventText(ctx, ho, ev, kind),
		State: ParentMessagePending, CreatedAt: time.Now().UTC(),
	}
	if err := writeParentMessage(msg, true); err != nil {
		if os.IsExist(err) {
			return p.FindMessage(id)
		}
		return nil, err
	}
	_ = AppendCommunication(msg, "request", "")
	childName := ho.HostID
	if child, err := p.Reg.GetSession(ho.SessionID); err == nil {
		childName = child.Persist.Name + "@" + ho.HostID
	}
	action := "ack"
	if kind == "ask" || kind == "permission_required" {
		action = "reply"
	}
	notice := ParentNotice{MessageID: msg.ID, Kind: kind, Child: childName, Text: msg.Text, Action: action}
	var notifyErr error
	// Delivery is strictly one edge up the tree. Only a local root owns a
	// human-facing cmux surface; every other parent is an agent manager and
	// receives the same compact envelope in its own session.
	if isLocalParent(parent) && p.Notifier != nil {
		notifyErr = p.Notifier.NotifyParent(ctx, parent.ID, notice)
	} else if !isLocalParent(parent) && p.Sessions != nil {
		notifyErr = p.Sessions.Send(ctx, parent.ID, FormatParentNotice(notice), true)
	} else {
		notifyErr = fmt.Errorf("no delivery path for parent %s", parent.ID)
	}
	if notifyErr == nil {
		now := time.Now().UTC()
		msg.DeliveredAt = &now
		_ = writeParentMessage(msg, false)
	}
	return msg, notifyErr
}

func FormatParentNotice(n ParentNotice) string {
	text := compactText(n.Text)
	suffix := ""
	if n.Action == "reply" {
		suffix = " <decision>"
	}
	return fmt.Sprintf("[relay %s %s child=%s] %s | relay parent %s %s%s", n.Kind, n.MessageID, n.Child, text, n.Action, n.MessageID, suffix)
}

func (p *ParentService) Watch(ctx context.Context, handoffID string) error {
	lock, err := acquireParentWatchLock(handoffID)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }()
	ho, err := p.Reg.GetHandoff(handoffID)
	if err != nil {
		return err
	}
	if ho.SourceSessionID == "" {
		return fmt.Errorf("handoff %s has no parent session", handoffID)
	}
	sess, err := p.Reg.GetSession(ho.SessionID)
	if err != nil {
		return err
	}
	t, err := p.NewTransport(ho.HostID)
	if err != nil {
		return err
	}
	if err := p.Coord.Ensure(ctx, t); err != nil {
		return err
	}
	attempts := 0
	windowStart := time.Now()
	for {
		ended := false
		from := ho.ParentSeq
		subErr := streamEvents(ctx, p.Coord, t, sess.Persist.Name, from, true, func(ev coord.Event) bool {
			latest, getErr := p.Reg.GetHandoff(handoffID)
			if getErr == nil {
				ho = latest
			}
			_, _ = p.RouteChildEvent(ctx, ho, ev)
			if ev.Seq > ho.ParentSeq {
				ho.ParentSeq = ev.Seq
				_ = p.Reg.PutHandoff(ho)
			}
			ended = ev.Kind == "exit"
			return !ended
		})
		if ended {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(windowStart) > 10*time.Minute {
			attempts, windowStart = 0, time.Now()
		}
		attempts++
		if attempts > 6 {
			return fmt.Errorf("parent watcher reconnect limit for %s: %w", handoffID, subErr)
		}
		delay := time.Duration(1<<uint(attempts-1)) * time.Second
		if delay > time.Minute {
			delay = time.Minute
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if latest, getErr := p.Reg.GetHandoff(handoffID); getErr == nil {
			ho = latest
		}
	}
}

func acquireParentWatchLock(handoffID string) (*os.File, error) {
	if err := EnsureStateDirs(); err != nil {
		return nil, err
	}
	path := filepath.Join(ParentWatchDir(), sanitizeID(handoffID)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("parent watcher already running for %s", handoffID)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}

func (p *ParentService) Reply(ctx context.Context, messageID, text string) (*ParentMessage, error) {
	msg, err := p.FindMessage(messageID)
	if err != nil {
		return nil, err
	}
	if msg.State != ParentMessagePending {
		return msg, nil
	}
	if msg.Kind != "ask" && msg.Kind != "permission_required" {
		return nil, fmt.Errorf("message %s is %s; acknowledge it instead", msg.ID, msg.Kind)
	}
	text = compactText(text)
	if text == "" {
		return nil, fmt.Errorf("reply text required")
	}
	ho, err := p.Reg.GetHandoff(msg.HandoffID)
	if err != nil {
		return nil, err
	}
	if ho.Kind == KindJob {
		return nil, fmt.Errorf("cannot inject a reply into job handoff %s", ho.ID)
	}
	if err := p.Sessions.Send(ctx, msg.ChildSessionID, text, true); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	msg.State, msg.Reply, msg.RepliedAt, msg.AckedAt = ParentMessageReplied, text, &now, &now
	if err := writeParentMessage(msg, false); err != nil {
		return nil, err
	}
	_ = AppendCommunication(msg, "reply", text)
	if p.Coord != nil && p.NewTransport != nil {
		if child, getErr := p.Reg.GetSession(msg.ChildSessionID); getErr == nil {
			if t, transportErr := p.NewTransport(child.HostID); transportErr == nil {
				_, _ = p.Coord.Emit(ctx, t, child.Persist.Name, "inject", map[string]any{"correlation_id": msg.CorrelationID, "message_id": msg.ID})
			}
		}
	}
	return msg, nil
}

func (p *ParentService) Ack(messageID string) (*ParentMessage, error) {
	msg, err := p.FindMessage(messageID)
	if err != nil {
		return nil, err
	}
	if msg.State == ParentMessageAcked || msg.State == ParentMessageReplied {
		return msg, nil
	}
	now := time.Now().UTC()
	msg.State, msg.AckedAt = ParentMessageAcked, &now
	if err := writeParentMessage(msg, false); err != nil {
		return nil, err
	}
	_ = AppendCommunication(msg, "ack", "")
	return msg, nil
}

func (p *ParentService) SetState(sessionID, state string) (*Session, error) {
	if state != "active" && state != "idle" && state != "complete" {
		return nil, fmt.Errorf("parent state must be active, idle, or complete")
	}
	sess, err := p.Reg.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if !isLocalParent(sess) {
		return nil, fmt.Errorf("session %s is not a local parent", sessionID)
	}
	if sess.Labels == nil {
		sess.Labels = map[string]string{}
	}
	sess.Labels["parent_state"] = state
	if err := p.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	_ = AppendLedger(map[string]any{"v": 1, "type": "parent_state", "ts": time.Now().UTC().Format(time.RFC3339), "session_id": sess.ID, "state": state})
	return sess, nil
}

type RepoGate struct {
	Path     string `json:"path"`
	Clean    bool   `json:"clean"`
	Upstream string `json:"upstream,omitempty"`
	Pushed   bool   `json:"pushed"`
	Error    string `json:"error,omitempty"`
}

type RetirementGate struct {
	SessionID      string     `json:"session_id"`
	Eligible       bool       `json:"eligible"`
	State          string     `json:"state"`
	ActiveChildren []string   `json:"active_children"`
	PendingInbox   []string   `json:"pending_inbox"`
	Repos          []RepoGate `json:"repos"`
	Reasons        []string   `json:"reasons"`
	Closed         bool       `json:"closed,omitempty"`
}

func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func checkRepoGate(ctx context.Context, repo string) RepoGate {
	gate := RepoGate{Path: repo}
	root, rootErr := gitRoot(repo)
	realRoot, _ := filepath.EvalSymlinks(root)
	realRepo, _ := filepath.EvalSymlinks(repo)
	if rootErr != nil || realRoot != realRepo {
		gate.Error = "not a git root"
		return gate
	}
	status, err := runGit(ctx, repo, "status", "--porcelain")
	if err != nil {
		gate.Error = compactText(status)
		return gate
	}
	gate.Clean = status == ""
	upstream, err := runGit(ctx, repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		gate.Error = "no upstream"
		return gate
	}
	gate.Upstream = upstream
	parts := strings.SplitN(upstream, "/", 2)
	if len(parts) != 2 {
		gate.Error = "invalid upstream"
		return gate
	}
	if _, err := runGit(ctx, repo, "fetch", "--quiet", "--no-tags", parts[0], parts[1]); err != nil {
		gate.Error = "upstream fetch failed"
		return gate
	}
	ahead, err := runGit(ctx, repo, "rev-list", "--count", "FETCH_HEAD..HEAD")
	if err != nil {
		gate.Error = compactText(ahead)
		return gate
	}
	gate.Pushed = ahead == "0"
	return gate
}

func (p *ParentService) RetirementStatus(ctx context.Context, sessionID string) (*RetirementGate, error) {
	sess, err := p.Reg.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if !isLocalParent(sess) {
		return nil, fmt.Errorf("session %s is not a local parent", sessionID)
	}
	gate := &RetirementGate{
		SessionID: sessionID, State: sess.Labels["parent_state"],
		ActiveChildren: []string{}, PendingInbox: []string{}, Repos: []RepoGate{}, Reasons: []string{},
	}
	if gate.State != "idle" && gate.State != "complete" {
		gate.Reasons = append(gate.Reasons, "parent is not explicitly idle/complete")
	}
	handoffs, _ := p.Reg.ListHandoffs()
	for _, ho := range handoffs {
		if ho.SourceSessionID != sessionID {
			continue
		}
		terminal := ho.Outcome != "" || ho.Status == StatusDone || ho.Status == StatusFailed || ho.Status == StatusAbandoned
		if !terminal {
			gate.ActiveChildren = append(gate.ActiveChildren, ho.ID)
		}
	}
	if len(gate.ActiveChildren) > 0 {
		gate.Reasons = append(gate.Reasons, "active child handoffs remain")
	}
	messages, _ := p.ListMessages(sessionID, true)
	for _, msg := range messages {
		gate.PendingInbox = append(gate.PendingInbox, msg.ID)
	}
	if len(gate.PendingInbox) > 0 {
		gate.Reasons = append(gate.Reasons, "parent inbox has unacknowledged messages")
	}
	refs := normalizeRepoRefs(sess.RepoRefs)
	if len(refs) == 0 && sess.RepoRef != "" {
		refs = []string{sess.RepoRef}
	}
	if len(refs) == 0 {
		gate.Reasons = append(gate.Reasons, "no scoped repositories")
	}
	for _, repo := range refs {
		check := checkRepoGate(ctx, repo)
		gate.Repos = append(gate.Repos, check)
		if !check.Clean || !check.Pushed || check.Error != "" {
			gate.Reasons = append(gate.Reasons, "repository is dirty, unpushed, or unverifiable: "+repo)
		}
	}
	gate.Eligible = len(gate.Reasons) == 0
	return gate, nil
}

func (p *ParentService) Retire(ctx context.Context, sessionID string, dryRun bool) (*RetirementGate, error) {
	gate, err := p.RetirementStatus(ctx, sessionID)
	if err != nil || !gate.Eligible || dryRun {
		return gate, err
	}
	if p.Viz != nil {
		if err := p.Viz.Close(ctx, sessionID); err != nil {
			return gate, err
		}
	}
	if err := p.Reg.DeleteSession(sessionID); err != nil {
		return gate, err
	}
	gate.Closed = true
	_ = AppendLedger(map[string]any{"v": 1, "type": "parent_retire", "ts": time.Now().UTC().Format(time.RFC3339), "session_id": sessionID})
	return gate, nil
}

func AppendCommunication(msg *ParentMessage, action, text string) error {
	if msg == nil {
		return nil
	}
	record := map[string]any{
		"v": 1, "type": "communication", "ts": time.Now().UTC().Format(time.RFC3339),
		"action": action, "message_id": msg.ID, "correlation_id": msg.CorrelationID,
		"parent_session_id": msg.ParentSessionID, "child_session_id": msg.ChildSessionID,
		"handoff_id": msg.HandoffID, "kind": msg.Kind, "event_seq": msg.EventSeq,
	}
	if text != "" {
		record["text"] = compactText(text)
	}
	return AppendLedger(record)
}
