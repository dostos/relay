package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// parentTextLimit caps an escalation body.
//
// It is deliberately generous. The alternative to a fuller excerpt is not a
// cheaper message — it is the manager running `relay agent capture -n 260` to
// recover the context the notice dropped, which costs orders of magnitude more
// than the characters saved here.
const ParentTextLimit = 600

// deliveryAttemptTimeout bounds ONE delivery hop. SessionService.Send passes
// the caller's context straight to the transport with no timeout of its own,
// so without this a dead SSH host would stall the whole ancestor walk.
const deliveryAttemptTimeout = 5 * time.Second

// A failed, definitely-unapplied delivery is safe to retry, but not on every
// supervisor tick. The schedule is derived from durable attempt metadata so a
// service restart cannot reset it into a burst. Explicit redelivery bypasses
// this scheduler; automatic reminders converge to at most one per 15 minutes.
const (
	deliveryRetryBase = 5 * time.Second
	deliveryRetryCap  = 15 * time.Minute
)

type ParentMessageState string

const (
	ParentMessagePending ParentMessageState = "pending"
	ParentMessageReplied ParentMessageState = "replied"
	ParentMessageAcked   ParentMessageState = "acked"
)

// ParentMessage is a compact, durable child-to-parent envelope. EventSeq plus
// the handoff id is its idempotency key; transcripts never enter this store.
type ParentMessage struct {
	V               int    `json:"v"`
	ID              string `json:"id"`
	CorrelationID   string `json:"correlation_id"`
	ParentSessionID string `json:"parent_session_id"`
	ChildSessionID  string `json:"child_session_id"`
	// Failover attribution. Empty on the common path where the immediate
	// parent received the escalation directly.
	IntendedParentSessionID string   `json:"intended_parent_session_id,omitempty"`
	SkippedSessionIDs       []string `json:"skipped_session_ids,omitempty"`
	ResolvedBySessionID     string   `json:"resolved_by_session_id,omitempty"`
	// StallReportedAt records when this envelope's holder-manager was last told
	// it was stuck, so a standing stall is not re-announced every tick.
	StallAttemptedAt   *time.Time         `json:"stall_attempted_at,omitempty"`
	StallReportedAt    *time.Time         `json:"stall_reported_at,omitempty"`
	HandoffID          string             `json:"handoff_id"`
	EventSeq           int64              `json:"event_seq"`
	Kind               string             `json:"kind"`
	Text               string             `json:"text,omitempty"`
	Gate               *SecurityGate      `json:"gate,omitempty"`
	State              ParentMessageState `json:"state"`
	CreatedAt          time.Time          `json:"created_at"`
	DeliveredAt        *time.Time         `json:"delivered_at,omitempty"`
	Reply              string             `json:"reply,omitempty"`
	RepliedAt          *time.Time         `json:"replied_at,omitempty"`
	AckedAt            *time.Time         `json:"acked_at,omitempty"`
	PolicyID           string             `json:"policy_id,omitempty"`
	PolicyAction       string             `json:"policy_action,omitempty"`
	AutoHandled        bool               `json:"auto_handled,omitempty"`
	PolicyError        string             `json:"policy_error,omitempty"`
	DeliveryMethod     string             `json:"delivery_method,omitempty"`
	DeliveryBuild      string             `json:"delivery_build,omitempty"`
	DeliveryError      string             `json:"delivery_error,omitempty"`
	DeliveryAttempts   int                `json:"delivery_attempts,omitempty"`
	LastAttemptAt      *time.Time         `json:"last_attempt_at,omitempty"`
	LastAttemptTarget  string             `json:"last_attempt_target,omitempty"`
	LastAttemptOutcome string             `json:"last_attempt_outcome,omitempty"`
}

const (
	deliveryAttempting = "session_send_attempting"
	deliveryUncertain  = "session_send_uncertain"
)

type parentDeliveryError struct {
	err         error
	unavailable bool
}

func (e *parentDeliveryError) Error() string { return e.err.Error() }
func (e *parentDeliveryError) Unwrap() error { return e.err }

func deliveryUnavailable(err error) bool {
	var deliveryErr *parentDeliveryError
	return errors.As(err, &deliveryErr) && deliveryErr.unavailable
}

func deliveryInDoubt(msg *ParentMessage) bool {
	return msg != nil && (msg.DeliveryMethod == deliveryAttempting || msg.DeliveryMethod == deliveryUncertain)
}

func deliveryRetryDue(msg *ParentMessage, now time.Time) bool {
	if msg == nil || msg.LastAttemptAt == nil || msg.LastAttemptOutcome != "retryable" || msg.DeliveryAttempts <= 0 {
		return true
	}
	shift := msg.DeliveryAttempts - 1
	if shift > 8 {
		shift = 8
	}
	delay := deliveryRetryBase * time.Duration(1<<uint(shift))
	if delay > deliveryRetryCap {
		delay = deliveryRetryCap
	}
	return !now.Before(msg.LastAttemptAt.Add(delay))
}

// ParentInboxItem is the turn-level projection of a durable parent message.
// Full timestamps, routing identity, and event cursors remain on disk and in
// history; an orchestrator receives only what it needs for one decision.
type ParentInboxItem struct {
	ID            string             `json:"id"`
	Kind          string             `json:"kind"`
	Text          string             `json:"text,omitempty"`
	Gate          *SecurityGate      `json:"gate,omitempty"`
	State         ParentMessageState `json:"state,omitempty"`
	Reply         string             `json:"reply,omitempty"`
	PolicyID      string             `json:"policy_id,omitempty"`
	AutoHandled   bool               `json:"auto_handled,omitempty"`
	PolicyError   string             `json:"policy_error,omitempty"`
	Delivery      string             `json:"delivery,omitempty"`
	DeliveryError string             `json:"delivery_error,omitempty"`
	Next          string             `json:"next"`
	Argv          []string           `json:"argv"`
}

func CompactParentMessage(msg *ParentMessage, includeState bool) ParentInboxItem {
	next := ""
	var argv []string
	if msg.Kind == "ask" || msg.Kind == "permission_required" {
		next = "resolve"
		argv = []string{"relay", "resolve", msg.ID, "--", "<decision>"}
	} else if deliveryInDoubt(msg) {
		next = "redeliver"
		argv = []string{"relay", "parent", "redeliver", msg.ID}
	}
	text := msg.Text
	if msg.Gate != nil {
		// Recognized gates already carry the exact reason, directory, and
		// choices structurally. Their formatted Text is retained on the durable
		// envelope and composer notice, but repeating it in the inbox projection
		// makes the decision surface appear twice to the manager.
		text = ""
	}
	item := ParentInboxItem{
		ID: msg.ID, Kind: msg.Kind, Text: text, Gate: msg.Gate,
		Next: next, Argv: argv,
	}
	if deliveryInDoubt(msg) {
		item.Delivery = msg.DeliveryMethod
		item.DeliveryError = msg.DeliveryError
	}
	if includeState {
		item.State = msg.State
		item.Reply = msg.Reply
		item.PolicyID = msg.PolicyID
		item.AutoHandled = msg.AutoHandled
		item.PolicyError = msg.PolicyError
		item.Delivery = msg.DeliveryMethod
		item.DeliveryError = msg.DeliveryError
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
	RouteLaunchFailure(context.Context, *Handoff, coord.Event) (*ParentMessage, error)
}

type ParentService struct {
	Reg          *Registry
	Sessions     *SessionService
	Coord        ports.Coord
	Viz          ports.Viz
	Notifier     ParentNotifier
	Policies     *PolicyService
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
			_, _ = p.DeliverPending(ctx, sess.ID)
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

// BindLocal moves an existing local parent's cmux binding to a restarted
// surface while preserving its durable identity, inbox, children, and history.
func (p *ParentService) BindLocal(ctx context.Context, parentID, surface string) (*Session, error) {
	if p.Reg == nil || p.Notifier == nil {
		return nil, fmt.Errorf("parent registry and notifier required")
	}
	sess, err := p.Reg.GetSession(parentID)
	if err != nil {
		return nil, err
	}
	if !isLocalParent(sess) {
		return nil, fmt.Errorf("session %s is not a local parent", parentID)
	}
	surface = strings.TrimSpace(surface)
	if surface == "" {
		surface, err = CurrentSurface()
		if err != nil {
			return nil, err
		}
	}
	if !strings.HasPrefix(surface, "surface:") {
		surface = "surface:" + surface
	}
	oldSurface := sess.VizSurfaceRef
	if _, err := p.Notifier.BindLocalParent(ctx, sess.ID, surface); err != nil {
		return nil, err
	}
	if sess.Labels == nil {
		sess.Labels = map[string]string{}
	}
	sess.Labels["parent_state"] = "active"
	sess.VizSurfaceRef, sess.UpdatedAt = surface, time.Now().UTC()
	if err := p.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	_, _ = p.DeliverPending(ctx, sess.ID)
	_ = AppendLedger(map[string]any{
		"v": 1, "type": "parent_bind", "ts": sess.UpdatedAt.Format(time.RFC3339),
		"session_id": sess.ID, "old_surface": oldSurface, "surface": surface,
	})
	return sess, nil
}

// LinkChild adopts an already-running handoff into a manager's durable
// goal tree. This is intentionally a one-time lineage operation: moving a
// child between parents would make request routing and history ambiguous.
func (p *ParentService) LinkChild(parentID, handoffID string) (*Handoff, error) {
	parent, err := p.Reg.GetSession(parentID)
	if err != nil {
		return nil, err
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
	if err := validateManagerEdge(p.Reg, parent, child); err != nil {
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

// ReparentChild explicitly repairs an incorrect management edge. Unlike
// LinkChild, this operation is intentionally named and audited: pending inbox
// items move with the child, while answered history remains with the manager
// that actually handled it.
func (p *ParentService) ReparentChild(parentID, handoffID string) (*Handoff, string, error) {
	parent, err := p.Reg.GetSession(parentID)
	if err != nil {
		return nil, "", err
	}
	ho, err := p.Reg.GetHandoff(handoffID)
	if err != nil {
		return nil, "", err
	}
	oldParentID := ho.SourceSessionID
	if oldParentID == "" {
		linked, linkErr := p.LinkChild(parentID, handoffID)
		if linkErr == nil {
			_ = p.reparentPaneBinding(linked.SessionID, parentID)
		}
		return linked, "", linkErr
	}
	if oldParentID == parentID {
		_ = p.reparentPaneBinding(ho.SessionID, parentID)
		return ho, oldParentID, nil
	}
	child, err := p.Reg.GetSession(ho.SessionID)
	if err != nil {
		return nil, oldParentID, err
	}
	if err := validateManagerEdge(p.Reg, parent, child); err != nil {
		return nil, oldParentID, err
	}
	ho.SourceSessionID, ho.SourceHostID, ho.SourcePersistName = parent.ID, parent.HostID, parent.Persist.Name
	child.SourceSessionID, child.SourceHostID, child.SourcePersistName = parent.ID, parent.HostID, parent.Persist.Name
	if err := p.Reg.PutSession(child); err != nil {
		return nil, oldParentID, err
	}
	if err := p.Reg.PutHandoff(ho); err != nil {
		return nil, oldParentID, err
	}
	if err := p.reparentPaneBinding(child.ID, parentID); err != nil {
		return ho, oldParentID, err
	}
	if messages, listErr := p.ListMessages(oldParentID, true); listErr == nil {
		for _, msg := range messages {
			if msg.HandoffID != handoffID {
				continue
			}
			oldPath := parentMessagePath(oldParentID, msg.ID)
			msg.ParentSessionID = parentID
			if err := writeParentMessage(msg, false); err != nil {
				return ho, oldParentID, err
			}
			if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
				return ho, oldParentID, err
			}
		}
	}
	now := time.Now().UTC()
	_ = AppendLedger(map[string]any{
		"v": 1, "type": "parent_reparent", "ts": now.Format(time.RFC3339),
		"old_parent_session_id": oldParentID, "parent_session_id": parent.ID,
		"child_session_id": child.ID, "handoff_id": ho.ID,
	})
	return ho, oldParentID, nil
}

func validateManagerEdge(reg *Registry, parent, child *Session) error {
	if parent == nil || child == nil {
		return fmt.Errorf("parent and child sessions are required")
	}
	if parent.ID == child.ID {
		return fmt.Errorf("session %s cannot manage itself", child.ID)
	}
	for _, ancestor := range AncestorChain(reg, parent.ID) {
		if ancestor.ID == child.ID {
			return fmt.Errorf("parenting %s under %s would create a cycle", child.ID, parent.ID)
		}
	}
	return nil
}

func (p *ParentService) reparentPaneBinding(childSessionID, parentSessionID string) error {
	if p.Viz == nil || p.Reg == nil {
		return nil
	}
	child, err := p.Reg.GetSession(childSessionID)
	if err != nil {
		return err
	}
	ref, err := PresentSession(context.Background(), p.Viz, child, ResumeLaunchCmd(child.Persist.Name), ports.Layout{Mode: "remote", SourceSessionID: parentSessionID})
	if err != nil {
		return err
	}
	child.VizSurfaceRef = ref
	return p.Reg.PutSession(child)
}

func (p *ParentService) projectPane(item ports.Presentation) error {
	if p.Viz == nil {
		return nil
	}
	if sink, ok := p.Viz.(ports.ProjectionSink); ok {
		_, err := sink.ApplyProjection(context.Background(), ports.ProjectionEvent{V: 1, Op: ports.ProjectionUpsert, Item: item})
		return err
	}
	sess := &Session{ID: item.SessionID, HostID: item.Target, SourceSessionID: item.ParentSessionID, Persist: ports.PersistHandle{Name: item.TmuxName}}
	_, err := PresentSession(context.Background(), p.Viz, sess, ResumeLaunchCmd(item.TmuxName), ports.Layout{Mode: "remote", SourceSessionID: item.ParentSessionID})
	return err
}

func handoffTerminal(ho *Handoff) bool {
	return ho != nil && (ho.Outcome != "" || ho.Status == StatusDone || ho.Status == StatusFailed || ho.Status == StatusAbandoned)
}

// SweepTerminal acknowledges stale pending messages for children whose
// handoffs are already terminal. It never guesses about live or missing
// handoffs; those remain visible for a manager decision.
func (p *ParentService) SweepTerminal(parentID string) (int, map[string]int, error) {
	if _, err := p.Reg.GetSession(parentID); err != nil {
		return 0, nil, err
	}
	messages, err := p.ListMessages(parentID, true)
	if err != nil {
		return 0, nil, err
	}
	byHandoff := map[string]int{}
	acked := 0
	for _, msg := range messages {
		ho, getErr := p.Reg.GetHandoff(msg.HandoffID)
		if getErr != nil || !handoffTerminal(ho) {
			continue
		}
		if deliveryInDoubt(msg) {
			continue
		}
		if _, err := p.Ack(msg.ID); err != nil {
			return acked, byHandoff, err
		}
		acked++
		byHandoff[msg.HandoffID]++
	}
	return acked, byHandoff, nil
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

// parentMessageID is deliberately independent of the delivery target. An
// escalation can change hands when a manager is unreachable, and if identity
// were derived from the holder the same logical question would mint a second
// id in a second inbox — defeating the replay guard and letting a re-routed
// event overwrite a decision that was already recorded.
func parentMessageID(handoffID, kind string, seq int64) string {
	sum := sha256.Sum256([]byte(handoffID + "\x00" + kind + "\x00" + strconv.FormatInt(seq, 10)))
	return "pm-" + hex.EncodeToString(sum[:8])
}

func correlatedParentMessageID(handoffID, kind, correlationID string) string {
	sum := sha256.Sum256([]byte(handoffID + "\x00" + kind + "\x00correlation\x00" + correlationID))
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
	if len(text) > ParentTextLimit {
		text = text[:ParentTextLimit-3] + "..."
	}
	return text
}

func writeParentMessage(msg *ParentMessage, exclusive bool) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	return writeParentMessageLocked(msg, exclusive)
}

func writeParentMessageLocked(msg *ParentMessage, exclusive bool) error {
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

func claimParentDelivery(msg *ParentMessage, targetID string) (bool, error) {
	if err := EnsureAuthorityWritable(); err != nil {
		return false, err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return false, err
	}
	defer unlock()
	stored, err := readParentMessage(parentMessagePath(msg.ParentSessionID, msg.ID))
	if err != nil {
		return false, err
	}
	*msg = *stored
	if msg.State != ParentMessagePending || msg.DeliveredAt != nil {
		return false, nil
	}
	if deliveryInDoubt(msg) {
		return false, fmt.Errorf("delivery %s for %s requires reconciliation: %s", msg.DeliveryMethod, msg.ID, msg.DeliveryError)
	}
	msg.DeliveryMethod, msg.DeliveryBuild, msg.DeliveryError = deliveryAttempting, coord.Build, ""
	now := time.Now().UTC()
	msg.DeliveryAttempts++
	msg.LastAttemptAt = &now
	msg.LastAttemptTarget = targetID
	msg.LastAttemptOutcome = "attempting"
	if err := writeParentMessageLocked(msg, false); err != nil {
		return false, err
	}
	return true, nil
}

func finalizeParentDelivery(msg *ParentMessage, method, deliveryErr string, confirmed bool) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	stored, err := readParentMessage(parentMessagePath(msg.ParentSessionID, msg.ID))
	if err != nil {
		return err
	}
	if stored.DeliveryMethod != deliveryAttempting {
		return fmt.Errorf("delivery claim for %s changed to %q before finalization", msg.ID, stored.DeliveryMethod)
	}
	stored.DeliveryMethod, stored.DeliveryBuild, stored.DeliveryError = method, coord.Build, compactText(deliveryErr)
	stored.LastAttemptOutcome = method
	if method == "" {
		stored.LastAttemptOutcome = "retryable"
	}
	if method == "" {
		stored.DeliveryBuild = ""
	}
	if confirmed {
		now := time.Now().UTC()
		stored.DeliveredAt = &now
		if !attentionMessage(stored.Kind) && stored.State == ParentMessagePending {
			stored.State, stored.AckedAt = ParentMessageAcked, &now
		}
	}
	if err := writeParentMessageLocked(stored, false); err != nil {
		return err
	}
	*msg = *stored
	return nil
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
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	dir := parentMessageDir(parentID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		if err := EnsureAuthorityReadable(); err != nil {
			return nil, err
		}
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
		if err != nil {
			return nil, fmt.Errorf("read parent message %s/%s: %w", parentID, entry.Name(), err)
		}
		if pendingOnly && msg.State != ParentMessagePending {
			continue
		}
		out = append(out, msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *ParentService) FindMessage(id string) (*ParentMessage, error) {
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	parents, err := os.ReadDir(ParentInboxDir())
	if os.IsNotExist(err) {
		if err := EnsureAuthorityReadable(); err != nil {
			return nil, err
		}
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
			if err := EnsureAuthorityReadable(); err != nil {
				return nil, err
			}
			return msg, nil
		}
	}
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
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
	switch ev.Kind {
	case "ask", "needs_input", "idle":
		return "ask"
	case "note", "progress", "result", "exit":
		return ev.Kind
	default:
		return ""
	}
}

// eventWakesManager is the semantic boundary between durable child telemetry
// and an interruption. Hooks, idle samples, notes, and progress already live
// in relayd; copying them into a manager composer adds a turn, not state.
func eventWakesManager(ev coord.Event) bool {
	switch ev.Kind {
	case "ask", "needs_input", "permission_required", "exit":
		return true
	case "result":
		return eventString(ev.Meta, "source") != "hook"
	default:
		return false
	}
}

func (p *ParentService) classifySecurityEvent(ctx context.Context, ho *Handoff, ev coord.Event) (coord.Event, bool, bool) {
	// Event kind and metadata are child-controlled. Inspect every event that
	// could trigger a policy reply; an agent must not bypass a visible trust,
	// login, or tool-permission gate by labeling it as an ordinary ask.
	switch ev.Kind {
	case "idle", "ask", "needs_input", "permission_required":
	default:
		return ev, ev.Kind != "idle", false
	}
	if p.Sessions == nil {
		return ev, ev.Kind != "idle", false
	}
	capture, err := p.Sessions.Capture(ctx, ho.SessionID, 40)
	if err != nil {
		return ev, ev.Kind != "idle", false
	}
	readiness := ClassifyAgentPane(capture)
	if readiness.State != AgentBlocked || readiness.Gate == nil {
		return ev, ev.Kind != "idle", false
	}
	if child, getErr := p.Reg.GetSession(ho.SessionID); getErr == nil && readiness.Gate.Directory == "" && child.RemoteCWD != "" {
		readiness.Gate.Directory = child.RemoteCWD
		if readiness.Gate.Subject == "" {
			readiness.Gate.Subject = child.RemoteCWD
		}
	}
	ho.Status, ho.PendingGate = StatusNeedsInput, readiness.Gate
	if ho.DeliveryState == EffectPending {
		ho.DeliveryState = EffectBlocked
	}
	_ = p.Reg.PutHandoff(ho)
	ev.Kind = "permission_required"
	if ev.Meta == nil {
		ev.Meta = map[string]any{}
	}
	ev.Meta["gate"] = readiness.Gate
	ev.Meta["text"] = formatSecurityGate(readiness.Gate)
	return ev, true, true
}

func (p *ParentService) childEventText(ctx context.Context, ho *Handoff, ev coord.Event, kind string) (string, string, bool) {
	if text := eventString(ev.Meta, "text", "q", "question", "msg", "note"); text != "" {
		return compactText(text), kind, true
	}
	if ev.Kind != "idle" && ev.Kind != "needs_input" {
		return compactText(kind + " from child on " + ho.HostID), kind, true
	}
	if p.Sessions != nil {
		if capture, err := p.Sessions.Capture(ctx, ho.SessionID, 80); err == nil {
			if paneStillActive(capture) || !p.paneSettled(ctx, ho, capture) {
				return "", kind, false
			}
			excerpt := decisionExcerpt(capture)
			if excerpt != "" {
				return "child idle on " + ho.HostID + " (use the handoff, not local paths); decide: " + excerpt, kind, true
			}
		}
	}
	return "child idle on " + ho.HostID + " (use the handoff, not local paths); decide blocked/completed/continue", kind, true
}

func attentionMessage(kind string) bool {
	return kind == "ask" || kind == "permission_required"
}

func sameGateDecision(a, b *SecurityGate) bool {
	if a == nil || b == nil || a.Reason != b.Reason || a.Directory != b.Directory || a.Subject != b.Subject || len(a.Choices) != len(b.Choices) {
		return false
	}
	for i := range a.Choices {
		if a.Choices[i].Index != b.Choices[i].Index || a.Choices[i].Label != b.Choices[i].Label {
			return false
		}
	}
	return true
}

// pendingAttention finds an existing unresolved ask for this handoff. It scans
// the given parent AND its ancestors, because an escalation raised while this
// parent was disconnected is held by whichever ancestor received it. Without
// the chain scan a reconnecting parent would raise a second ask for one
// question, breaking the one-unresolved-ask-per-handoff invariant.
func (p *ParentService) pendingAttention(parentID, handoffID string) (*ParentMessage, error) {
	holders := []string{parentID}
	for _, ancestor := range AncestorChain(p.Reg, parentID) {
		holders = append(holders, ancestor.ID)
	}
	for _, holder := range holders {
		messages, err := p.ListMessages(holder, true)
		if err != nil {
			return nil, err
		}
		for _, msg := range messages {
			if msg.HandoffID == handoffID && attentionMessage(msg.Kind) {
				return msg, nil
			}
		}
	}
	return nil, nil
}

// createParentMessage serializes semantic deduplication with creation. The old
// read-then-exclusive-create sequence only protected identical event IDs: two
// concurrent frames with different relayd sequences could both observe an
// empty inbox and wake the manager twice for one question.
func (p *ParentService) createParentMessage(msg *ParentMessage) (*ParentMessage, bool, error) {
	if err := EnsureAuthorityWritable(); err != nil {
		return nil, false, err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return nil, false, err
	}
	defer unlock()

	owners, readOwnersErr := os.ReadDir(ParentInboxDir())
	if readOwnersErr != nil && !os.IsNotExist(readOwnersErr) {
		return nil, false, readOwnersErr
	}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		if existing, readErr := readParentMessage(parentMessagePath(owner.Name(), msg.ID)); readErr == nil {
			return existing, false, nil
		}
	}
	if attentionMessage(msg.Kind) {
		holders := []string{msg.ParentSessionID}
		for _, ancestor := range AncestorChain(p.Reg, msg.ParentSessionID) {
			holders = append(holders, ancestor.ID)
		}
		for _, holder := range holders {
			entries, readErr := os.ReadDir(parentMessageDir(holder))
			if readErr != nil && !os.IsNotExist(readErr) {
				return nil, false, readErr
			}
			for _, entry := range entries {
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
					continue
				}
				existing, readErr := readParentMessage(filepath.Join(parentMessageDir(holder), entry.Name()))
				if readErr != nil || existing.State != ParentMessagePending || existing.HandoffID != msg.HandoffID || !attentionMessage(existing.Kind) {
					continue
				}
				if existing.Kind == msg.Kind && (msg.Kind != "permission_required" || sameGateDecision(existing.Gate, msg.Gate) || (existing.Gate == nil && msg.Gate == nil && existing.Text == msg.Text)) {
					return existing, false, nil
				}
			}
		}
	}
	if err := writeParentMessageLocked(msg, true); err != nil {
		return nil, false, err
	}
	return msg, true, nil
}

func (p *ParentService) deliverMessage(ctx context.Context, parent *Session, ho *Handoff, msg *ParentMessage) error {
	if msg == nil || msg.State != ParentMessagePending || msg.DeliveredAt != nil {
		return nil
	}
	if deliveryInDoubt(msg) {
		return fmt.Errorf("delivery %s for %s requires reconciliation: %s", msg.DeliveryMethod, msg.ID, msg.DeliveryError)
	}
	childName := ho.HostID
	if child, err := p.Reg.GetSession(ho.SessionID); err == nil {
		childName = child.Persist.Name + "@" + ho.HostID
	}
	action := ""
	if attentionMessage(msg.Kind) {
		action = "reply"
	}
	notice := ParentNotice{MessageID: msg.ID, Kind: msg.Kind, Child: childName, Text: msg.Text, Action: action}
	var err error
	claimed := false
	rootToHuman := parent.ID == ho.SessionID && parent.Labels[ApexLabel] == "true"
	if rootToHuman {
		if p.Notifier == nil {
			return &parentDeliveryError{err: fmt.Errorf("no human notification path for apex %s", parent.ID)}
		}
		var writeErr error
		claimed, writeErr = claimParentDelivery(msg, parent.ID)
		if writeErr != nil {
			err = fmt.Errorf("reserve apex notification: %w", writeErr)
		} else if !claimed {
			return nil
		} else {
			notifyCtx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
			err = p.Notifier.NotifyParent(notifyCtx, parent.ID, notice)
			cancel()
		}
	} else if p.Sessions != nil {
		attemptCtx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
		capture, captureErr := p.Sessions.Capture(attemptCtx, parent.ID, 40)
		if captureErr != nil {
			err = fmt.Errorf("verify parent readiness: %w", captureErr)
		} else if readiness := ClassifyAgentPane(capture); readiness.State != AgentReady {
			err = &parentDeliveryError{err: fmt.Errorf("parent is %s: %s", readiness.State, readiness.Reason), unavailable: readiness.State == AgentAbsent}
		} else {
			// Reserve the side effect before any adapter mutates the composer.
			// A crash from here onward is uncertain, never a safe automatic retry.
			var writeErr error
			claimed, writeErr = claimParentDelivery(msg, parent.ID)
			if writeErr != nil {
				err = fmt.Errorf("reserve parent delivery: %w", writeErr)
			} else if !claimed {
				cancel()
				return nil
			} else {
				err = p.Sessions.Send(attemptCtx, parent.ID, FormatParentNotice(notice), true)
			}
		}
		cancel()
	} else {
		err = &parentDeliveryError{err: fmt.Errorf("no delivery path for parent %s", parent.ID)}
	}
	if err != nil {
		var uncertain *ports.DeliveryUncertainError
		var unavailable *ports.TargetUnavailableError
		if claimed && errors.As(err, &uncertain) {
			if writeErr := finalizeParentDelivery(msg, deliveryUncertain, err.Error(), false); writeErr != nil {
				return fmt.Errorf("%v; persist uncertain delivery: %w", err, writeErr)
			}
		} else if claimed {
			if writeErr := finalizeParentDelivery(msg, "", "", false); writeErr != nil {
				return fmt.Errorf("%v; clear unused delivery reservation: %w", err, writeErr)
			}
		}
		if errors.As(err, &unavailable) {
			return &parentDeliveryError{err: err, unavailable: true}
		}
		return err
	}
	if rootToHuman {
		return finalizeParentDelivery(msg, "human_notification_confirmed", "", true)
	}
	// Desktop presentation is supplementary. It can flash a bound surface, but
	// it never owns or acknowledges the parent communication.
	// Agent managers registered by managed launch use composer injection as
	// their wake effect. Flashing the same surface as well is a second
	// interruption carrying no state. Explicit notify parents (and legacy
	// registrations without a mode) retain desktop presentation.
	if isLocalParent(parent) && parent.Labels["wake_mode"] != "inject" && p.Notifier != nil {
		notifyCtx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
		_ = p.Notifier.NotifyParent(notifyCtx, parent.ID, notice)
		cancel()
	}
	// Finalization reloads under the authority lock so a manager reply that
	// races pane acknowledgement cannot be overwritten by this stale copy.
	return finalizeParentDelivery(msg, "session_send_confirmed", "", true)
}

// RedeliverReceipt repairs an informational envelope whose historical
// delivery acknowledgement was not backed by pane-level evidence.
func (p *ParentService) RedeliverReceipt(ctx context.Context, messageID string) (*ParentMessage, error) {
	msg, err := p.FindMessage(messageID)
	if err != nil {
		return nil, err
	}
	if attentionMessage(msg.Kind) {
		return nil, fmt.Errorf("attention message %s requires resolve/ack, not redelivery", msg.ID)
	}
	parent, err := p.Reg.GetSession(msg.ParentSessionID)
	if err != nil {
		return nil, err
	}
	ho, err := p.Reg.GetHandoff(msg.HandoffID)
	if err != nil {
		return nil, err
	}
	msg.State, msg.DeliveredAt, msg.AckedAt = ParentMessagePending, nil, nil
	msg.DeliveryMethod, msg.DeliveryBuild, msg.DeliveryError = "", "", ""
	if err := writeParentMessage(msg, false); err != nil {
		return nil, err
	}
	if err := p.deliverMessage(ctx, parent, ho, msg); err != nil {
		return msg, err
	}
	return msg, nil
}

// DeliverPending retries durable envelopes after a parent pane is rebound.
// Repeated idle samples never create additional attention messages, so this
// produces at most one unresolved ask per child handoff.
func (p *ParentService) DeliverPending(ctx context.Context, parentID string) (int, error) {
	parent, err := p.Reg.GetSession(parentID)
	if err != nil {
		return 0, err
	}
	messages, err := p.ListMessages(parentID, true)
	if err != nil {
		return 0, err
	}
	delivered := 0
	var deliveryErrs []error
	now := time.Now().UTC()
	for _, msg := range messages {
		if msg.DeliveredAt != nil || deliveryInDoubt(msg) || !deliveryRetryDue(msg, now) {
			continue
		}
		ho, getErr := p.Reg.GetHandoff(msg.HandoffID)
		launchFailure := strings.HasPrefix(msg.CorrelationID, "launch-failure:")
		if getErr != nil || (handoffTerminal(ho) && !launchFailure) {
			continue
		}
		if err := p.deliverMessage(ctx, parent, ho, msg); err != nil {
			// One stuck envelope must not head-of-line block unrelated due
			// messages for the same manager. Each failure carries its own durable
			// retry schedule, while the caller still receives an honest aggregate.
			deliveryErrs = append(deliveryErrs, fmt.Errorf("%s: %w", msg.ID, err))
			continue
		}
		if launchFailure {
			ho.FailureNoticeState, ho.FailureNoticeError = EffectAcknowledged, ""
			_ = p.Reg.PutHandoff(ho)
		}
		delivered++
	}
	return delivered, errors.Join(deliveryErrs...)
}

// UncertainDeliveries returns post-reservation effects that cannot be retried
// safely without an explicit reconciliation decision.
func (p *ParentService) UncertainDeliveries() ([]*ParentMessage, error) {
	if err := EnsureAuthorityReadable(); err != nil {
		return nil, err
	}
	owners, err := os.ReadDir(ParentInboxDir())
	if os.IsNotExist(err) {
		return []*ParentMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*ParentMessage
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		messages, listErr := p.ListMessages(owner.Name(), true)
		if listErr != nil {
			return nil, fmt.Errorf("inspect delivery effects for %s: %w", owner.Name(), listErr)
		}
		for _, msg := range messages {
			if msg.State == ParentMessagePending && deliveryInDoubt(msg) {
				out = append(out, msg)
			}
		}
	}
	return out, nil
}

// paneSettleDelay is how long to wait between the two samples that decide
// whether a pane is genuinely waiting. Short enough to stay responsive, long
// enough that a rendering agent visibly moves.
const paneSettleDelay = 900 * time.Millisecond

// paneSettled reports whether the pane has stopped changing.
//
// paneStillActive recognises "still working" by matching known UI strings
// ("cerebrating", "· thinking", " running "+"tokens"), which only covers the
// agents whose UI was studied. cursor-agent's status line matches none of
// them, so string matching alone would call a busy pane idle — which is why
// the silence threshold had to be set so high to compensate.
//
// Comparing two samples is UI-agnostic: a working pane redraws, a waiting one
// does not. That lets the silence threshold drop without inventing false asks
// for whichever agent is next to have an unfamiliar UI.
func (p *ParentService) paneSettled(ctx context.Context, ho *Handoff, first string) bool {
	if p.Sessions == nil {
		return true
	}
	select {
	case <-ctx.Done():
		return true
	case <-time.After(paneSettleDelay):
	}
	second, err := p.Sessions.Capture(ctx, ho.SessionID, 80)
	if err != nil {
		// Cannot re-sample; fall back to the string heuristics alone rather
		// than swallowing a real ask.
		return true
	}
	return strings.TrimRight(second, " \t\n") == strings.TrimRight(first, " \t\n")
}

func paneStillActive(capture string) bool {
	lines := strings.Split(capture, "\n")
	if len(lines) > 14 {
		lines = lines[len(lines)-14:]
	}
	tail := strings.ToLower(strings.Join(lines, "\n"))
	for _, prompt := range []string{"run this command?", "not in allowlist", "what should", "permission", "approve", "confirm or esc", "timed out waiting for input"} {
		if strings.Contains(tail, prompt) {
			return false
		}
	}
	return (strings.Contains(tail, " running ") && strings.Contains(tail, "tokens")) ||
		strings.Contains(tail, "cerebrating") || strings.Contains(tail, "· thinking")
}

// chromeMarkers are fragments that only ever appear in an agent's UI furniture:
// spinners, keybinding hints, transcript pointers, model/status footers, and the
// empty composer placeholder.
var chromeMarkers = []string{
	"esc to interrupt",
	"shift + tab",
	"esc dismiss",
	"Context left",
}

// isFrameLine reports whether a line is mostly box-drawing glyphs — a table
// border, a panel edge, a rule. Such a line has no prose in it.
//
// The old filter only dropped lines made *entirely* of "─━═-", so any real
// table border survived: "└─────────┴──────────┘" contains corner and tee
// glyphs and passed straight through to the manager.
func isFrameLine(line string) bool {
	frame, visible := 0, 0
	for _, r := range line {
		if r == ' ' || r == '\t' {
			continue
		}
		visible++
		// U+2500–U+257F box drawing, U+2580–U+259F block elements.
		if (r >= 0x2500 && r <= 0x259F) || r == '-' || r == '=' || r == '_' {
			frame++
		}
	}
	if visible == 0 {
		return true
	}
	return frame*2 >= visible
}

func isChromeLine(line string) bool {
	for _, marker := range chromeMarkers {
		if strings.Contains(line, marker) {
			return true
		}
	}
	// Braille glyphs are spinner frames in every TUI that uses them.
	for _, r := range line {
		if r >= 0x2800 && r <= 0x28FF {
			return true
		}
	}
	lower := strings.ToLower(line)
	// Keybinding hints: "ctrl+c to stop", "ctrl+r to review".
	if strings.Contains(lower, "ctrl+") || strings.Contains(lower, "ctrl +") {
		return true
	}
	// Token/usage counters: a number next to the word tokens.
	if strings.Contains(lower, "tokens") && strings.ContainsAny(line, "0123456789") {
		return true
	}
	// Product nudges the agent prints between turns.
	if strings.HasPrefix(line, "Tip:") {
		return true
	}
	// Status bars: middle-dot separated with a percentage.
	if strings.Contains(line, "·") && strings.Contains(line, "%") {
		return true
	}
	// A bare working-directory line under a status bar.
	if strings.HasPrefix(line, "~/") && !strings.ContainsAny(line, " \t") {
		return true
	}
	// The composer is an input line, never decision content — whatever it
	// holds is either a placeholder or text the child has not sent.
	if strings.HasPrefix(line, "›") || strings.HasPrefix(line, "❯") {
		return true
	}
	// Scrollback pointers: "… truncated (164 more lines)".
	if strings.Contains(lower, "truncated (") && strings.Contains(lower, "more lines") {
		return true
	}
	// Status/footer bars: a model or path breadcrumb rather than a sentence.
	if strings.HasPrefix(line, "|") && strings.Contains(line, "Auto") && strings.Contains(line, "~/") {
		return true
	}
	// Agent footers are middle-dot separated breadcrumbs carrying a working
	// directory — "<model> · ~/path · Main [branch]". Matched structurally
	// rather than by model name, which changes with every release.
	if strings.Contains(line, "·") && strings.Contains(line, "~/") {
		return true
	}
	return false
}

// stripGutter removes a diff/code-block gutter prefix ("▎+ ", "▎  ") so the
// prose inside a diff reads as prose and does not spend characters on markers
// the manager cannot act on.
func stripGutter(line string) string {
	trimmed := strings.TrimLeft(line, "▎▏│┃ ")
	if trimmed == "" {
		return trimmed
	}
	// Only drop a +/- that was acting as a diff marker, i.e. followed by space.
	if len(trimmed) > 1 && (trimmed[0] == '+' || trimmed[0] == '-') && trimmed[1] == ' ' {
		trimmed = strings.TrimSpace(trimmed[1:])
	}
	return trimmed
}

// decisionExcerpt pulls the part of a child's screen a manager can actually
// decide on.
//
// The tail of an agent pane is almost always furniture — a table the child
// printed, a spinner, a hint bar, the composer. Sending that verbatim gave the
// manager nothing to act on, so it answered by capturing a few hundred lines of
// transcript instead, on every single escalation. Filtering the furniture is
// what makes the escalation self-sufficient, and skipping that follow-up
// capture is where the tokens are.
func decisionExcerpt(capture string) string {
	lines := strings.Split(capture, "\n")
	useful := make([]string, 0, len(lines))
	// A wrapped product nudge continues onto the next line with no marker of
	// its own, so the tip's second half would otherwise read as prose.
	afterTip := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		wasAfterTip := afterTip
		afterTip = strings.HasPrefix(line, "Tip:")
		if line == "" || isFrameLine(line) || isChromeLine(line) {
			continue
		}
		if wasAfterTip {
			continue
		}
		// A table row is data without its header; the prose around it carries
		// the meaning, and the manager can capture the table if it needs it.
		if strings.HasPrefix(line, "│") || strings.HasPrefix(line, "|") {
			continue
		}
		useful = append(useful, stripGutter(line))
	}
	if len(useful) > 6 {
		useful = useful[len(useful)-6:]
	}
	return compactText(strings.Join(useful, " "))
}

// deliveryCandidates lists the sessions that may receive an escalation, in
// lineage order: the immediate manager first, then its ancestors. Liveness is
// the OUTCOME of attempting delivery rather than a separate presence oracle,
// so the caller walks this list and stops at the first hop that succeeds. A
// live manager is therefore never skipped — only ancestors that genuinely
// cannot receive the envelope are passed over.
func (p *ParentService) deliveryCandidates(ho *Handoff) []*Session {
	// An apex is an intentional hierarchy root, but its own ask/result still
	// has one boundary destination: the human surface bound to that apex. Using
	// the apex session as the durable inbox holder does not invent a parent edge;
	// deliverMessage recognizes this case and performs notification only.
	if ho.SourceSessionID == "" {
		root, err := p.Reg.GetSession(ho.SessionID)
		if err == nil && root != nil && root.Labels[ApexLabel] == "true" {
			return []*Session{root}
		}
		return nil
	}
	immediate, err := p.Reg.GetSession(ho.SourceSessionID)
	if err != nil || immediate == nil {
		return nil
	}
	return append([]*Session{immediate}, AncestorChain(p.Reg, immediate.ID)...)
}

// promoteMessage hands an escalation to the ancestor that has just received
// it. It runs only AFTER a successful delivery, so an envelope nobody could
// reach stays in its intended manager's inbox rather than being parked on a
// session that never saw it.
func (p *ParentService) promoteMessage(msg *ParentMessage, from *Session, to *Session, skipped []string) error {
	if msg.IntendedParentSessionID == "" {
		msg.IntendedParentSessionID = from.ID
	}
	msg.SkippedSessionIDs = append(msg.SkippedSessionIDs, skipped...)
	oldPath := parentMessagePath(from.ID, msg.ID)
	msg.ParentSessionID = to.ID
	if err := writeParentMessage(msg, false); err != nil {
		return err
	}
	_ = os.Remove(oldPath)
	return nil
}

// deliverEscalation delivers an envelope, failing over to the nearest ancestor
// that can actually receive it when the intended manager cannot.
//
// Three rules keep the management tree intact:
//   - Only attention envelopes fail over. A routine result/exit receipt whose
//     manager is asleep must never walk up and interrupt a human.
//   - The intended manager gets a second attempt before anyone is skipped, so
//     a transient hiccup cannot bypass a manager that is actually live.
//   - The envelope changes hands only after an ancestor has received it.
func (p *ParentService) deliverEscalation(ctx context.Context, candidates []*Session, ho *Handoff, msg *ParentMessage) error {
	immediate := candidates[0]
	err := p.deliverMessage(ctx, immediate, ho, msg)
	if err == nil || !attentionMessage(msg.Kind) || len(candidates) == 1 {
		// With no ancestor to fail over to there is nothing to protect the
		// manager from, so the envelope simply stays pending for DeliverPending.
		return err
	}
	if deliveryInDoubt(msg) {
		return err
	}
	// Give the intended manager a second chance before anyone is skipped, so a
	// transient hiccup cannot bypass a manager that is actually live.
	if retryErr := p.deliverMessage(ctx, immediate, ho, msg); retryErr == nil {
		return nil
	} else if !deliveryUnavailable(retryErr) {
		return retryErr
	}
	var skipped []string
	for _, ancestor := range candidates[1:] {
		skipped = append(skipped, candidates[len(skipped)].ID)
		if deliverErr := p.deliverMessage(ctx, ancestor, ho, msg); deliverErr == nil {
			return p.promoteMessage(msg, immediate, ancestor, skipped)
		} else if !deliveryUnavailable(deliverErr) {
			return deliverErr
		}
	}
	// Nothing in the chain was reachable. The envelope stays pending with its
	// intended manager so DeliverPending retries it there on reconnect.
	return err
}

func (p *ParentService) RouteChildEvent(ctx context.Context, ho *Handoff, ev coord.Event) (*ParentMessage, error) {
	return p.routeChildEvent(ctx, ho, ev, false)
}

func (p *ParentService) RouteLaunchFailure(ctx context.Context, ho *Handoff, ev coord.Event) (*ParentMessage, error) {
	return p.routeChildEvent(ctx, ho, ev, true)
}

func (p *ParentService) routeChildEvent(ctx context.Context, ho *Handoff, ev coord.Event, allowTerminal bool) (*ParentMessage, error) {
	if ho == nil || (handoffTerminal(ho) && !allowTerminal) {
		return nil, nil
	}
	var keep, freshStructuredGate bool
	ev, keep, freshStructuredGate = p.classifySecurityEvent(ctx, ho, ev)
	if !keep || !eventWakesManager(ev) {
		return nil, nil
	}
	kind := attentionKind(ev)
	if kind == "" {
		return nil, nil
	}
	candidates := p.deliveryCandidates(ho)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("handoff %s has no reachable parent lineage", ho.ID)
	}
	parent := candidates[0]
	correlationID := eventString(ev.Meta, "correlation_id", "request_id")
	id := parentMessageID(ho.ID, kind, ev.Seq)
	if correlationID != "" && kind != "permission_required" {
		// A producer retry may receive a new relayd sequence. Its explicit
		// correlation ID is the semantic idempotency key; deriving the envelope
		// ID from it makes replay an exclusive-create lookup rather than another
		// manager wake. Handoff and kind remain in the hash boundary.
		id = correlatedParentMessageID(ho.ID, kind, correlationID)
	}
	// Replay guard. The envelope may live in any ancestor's inbox after a
	// failover, and FindMessage scans them all, so this catches a re-routed
	// event that the per-directory exclusive create would now miss.
	if existing, findErr := p.FindMessage(id); findErr == nil && existing != nil {
		return existing, nil
	}
	if correlationID == "" {
		correlationID = id
	}
	text, detectedKind, actionable := p.childEventText(ctx, ho, ev, kind)
	if !actionable {
		return nil, nil
	}
	kind = detectedKind
	// Sensors and runtime hooks can report the same blocked pane repeatedly.
	// Preserve one durable attention envelope until it is replied/acked,
	// independent of event sequence. If delivery failed, retry that envelope
	// instead of allocating and injecting another one.
	if attentionMessage(kind) {
		pending, pendingErr := p.pendingAttention(parent.ID, ho.ID)
		if pendingErr != nil {
			return nil, fmt.Errorf("inspect existing attention for %s: %w", ho.ID, pendingErr)
		}
		if pending != nil && (ev.Kind == "idle" || pending.Kind == kind) {
			samePermission := (freshStructuredGate && sameGateDecision(pending.Gate, ho.PendingGate)) || (!freshStructuredGate && pending.Gate == nil && pending.Text == text)
			if kind == "permission_required" && !samePermission {
				// A changed structured decision is not a replay even when the
				// previous decision is still pending. Preserve both audit records
				// and surface the new authority boundary.
			} else {
				// Event replay is not a delivery scheduler. The supervisor owns
				// pending-envelope retries, so repeated child frames cannot turn into
				// repeated pane injections or manager wake attempts.
				return pending, nil
			}
		}
	}
	msg := &ParentMessage{
		V: 1, ID: id, CorrelationID: compactText(correlationID),
		ParentSessionID: parent.ID, ChildSessionID: ho.SessionID, HandoffID: ho.ID,
		EventSeq: ev.Seq, Kind: kind, Text: text,
		State: ParentMessagePending, CreatedAt: time.Now().UTC(),
	}
	if kind == "permission_required" && freshStructuredGate {
		msg.Gate = ho.PendingGate
	}
	stored, created, err := p.createParentMessage(msg)
	if err != nil {
		return nil, err
	}
	if !created {
		return stored, nil
	}
	msg = stored
	logAction := "event"
	if attentionMessage(msg.Kind) {
		logAction = "request"
	}
	_ = AppendCommunication(msg, logAction, "")
	if handled, decided := p.applyPolicy(ctx, ho, ev, msg); decided {
		return handled, nil
	}
	// Delivery walks UP to the nearest ancestor that can actually receive the
	// envelope. A live manager is never skipped. Only a local root owns a
	// human-facing cmux surface; every other ancestor is an agent manager.
	return msg, p.deliverEscalation(ctx, candidates, ho, msg)
}

func (p *ParentService) applyPolicy(ctx context.Context, ho *Handoff, ev coord.Event, msg *ParentMessage) (*ParentMessage, bool) {
	if handled, decided := p.applyAgentChildWorkspaceTrust(ctx, ho, msg); decided {
		return handled, true
	}
	if p.Policies == nil || msg == nil {
		return msg, false
	}
	if msg.Kind == "permission_required" {
		return msg, false
	}
	seen, pending := map[string]bool{}, map[string]bool{}
	if messages, err := p.ListMessages(msg.ParentSessionID, false); err == nil {
		for _, other := range messages {
			if other.ID == msg.ID || other.HandoffID != msg.HandoffID {
				continue
			}
			if time.Since(other.CreatedAt) <= 2*time.Minute {
				seen[other.Kind] = true
			}
			if other.State == ParentMessagePending {
				pending[other.Kind] = true
			}
		}
	}
	decision, err := p.Policies.Decide(PolicyContext{
		Kind: msg.Kind, SourceKind: ev.Kind, Agent: ho.Agent, Host: ho.HostID, Text: msg.Text,
		Command: eventString(ev.Meta, "command", "cmd"), SeenKinds: seen, PendingKinds: pending,
	})
	if err != nil {
		msg.PolicyError = compactText(err.Error())
		_ = writeParentMessage(msg, false)
		return msg, false
	}
	if !decision.Matched {
		return msg, false
	}
	msg.PolicyID, msg.PolicyAction = decision.RuleID, decision.Action
	_ = writeParentMessage(msg, false)
	var handled *ParentMessage
	switch decision.Action {
	case "reply":
		handled, err = p.Reply(ctx, msg.ID, decision.Reply)
	case "ack":
		handled, err = p.Ack(msg.ID)
	default:
		err = fmt.Errorf("unsupported policy action %q", decision.Action)
	}
	if err != nil {
		msg.PolicyError = compactText(err.Error())
		_ = writeParentMessage(msg, false)
		return msg, false
	}
	handled.PolicyID, handled.PolicyAction, handled.AutoHandled = decision.RuleID, decision.Action, true
	_ = writeParentMessage(handled, false)
	return handled, true
}

const agentChildWorkspaceTrustPolicyID = "builtin.agent-child-workspace-trust"

// applyAgentChildWorkspaceTrust accepts the one authority boundary already
// implied by a parent launching a direct child into a declared workspace. It
// deliberately does not cover tool permissions, login/authentication, theme
// selection, descendant gates that skipped their manager, or a prompt whose
// directory was inferred rather than observed in the child pane.
func (p *ParentService) applyAgentChildWorkspaceTrust(ctx context.Context, ho *Handoff, msg *ParentMessage) (*ParentMessage, bool) {
	if p == nil || p.Reg == nil || msg == nil || ho == nil || msg.Kind != "permission_required" || msg.Gate == nil || ho.Kind != KindAgent {
		return msg, false
	}
	gate := msg.Gate
	if !strings.HasPrefix(gate.Reason, "waiting for folder-trust approval") || !gate.DirectoryObserved || len(gate.Choices) == 0 {
		return msg, false
	}
	if ho.SourceSessionID == "" || msg.ParentSessionID != ho.SourceSessionID {
		return msg, false
	}
	parent, err := p.Reg.GetSession(ho.SourceSessionID)
	if err != nil || parent == nil || (parent.Labels["agent"] == "" && parent.Labels[ApexLabel] != "true") {
		return msg, false
	}
	child, err := p.Reg.GetSession(ho.SessionID)
	if err != nil || child == nil || child.ID != msg.ChildSessionID || child.SourceSessionID != parent.ID || child.CreatedByHandoffID != ho.ID {
		return msg, false
	}
	if !sameRemoteWorkspace(gate.Directory, child.RemoteCWD) {
		return msg, false
	}
	if _, approved, decisionErr := resolveGateDecision(gate, "approve"); decisionErr != nil || !approved {
		return msg, false
	}

	msg.PolicyID, msg.PolicyAction = agentChildWorkspaceTrustPolicyID, "reply"
	_ = writeParentMessage(msg, false)
	handled, err := p.Reply(ctx, msg.ID, "approve")
	if err != nil {
		msg.PolicyError = compactText(err.Error())
		_ = writeParentMessage(msg, false)
		return msg, false
	}
	handled.PolicyID, handled.PolicyAction, handled.AutoHandled = agentChildWorkspaceTrustPolicyID, "reply", true
	_ = writeParentMessage(handled, false)
	return handled, true
}

// sameRemoteWorkspace compares a prompt path with the child's declared remote
// cwd without expanding '~' on the relay host. Absolute paths must match
// exactly; the only accepted shorthand equivalence is ~/x to a conventional
// Unix or macOS user home path ending in /x.
func sameRemoteWorkspace(observed, declared string) bool {
	clean := func(value string) string {
		value = strings.TrimSpace(value)
		if value == "" {
			return ""
		}
		return filepath.Clean(value)
	}
	observed, declared = clean(observed), clean(declared)
	if observed == "" || declared == "" {
		return false
	}
	if observed == declared {
		return true
	}
	homeRelative := func(value string) (string, bool) {
		if value == "~" {
			return "", true
		}
		if strings.HasPrefix(value, "~/") {
			return strings.TrimPrefix(value, "~/"), true
		}
		return "", false
	}
	absoluteHomeRelative := func(value string) (string, bool) {
		parts := strings.Split(strings.TrimPrefix(value, "/"), "/")
		if len(parts) < 2 || (parts[0] != "home" && parts[0] != "Users") || parts[1] == "" {
			return "", false
		}
		return strings.Join(parts[2:], "/"), true
	}
	if relative, ok := homeRelative(declared); ok {
		actual, absolute := absoluteHomeRelative(observed)
		return absolute && actual == relative
	}
	if relative, ok := homeRelative(observed); ok {
		actual, absolute := absoluteHomeRelative(declared)
		return absolute && actual == relative
	}
	return false
}

func FormatParentNotice(n ParentNotice) string {
	text := compactText(n.Text)
	if n.Action == "reply" {
		// The message ID owns handoff, lineage, child identity, and the durable
		// decision cursor. Retyping the handoff ID gives the manager no action it
		// could not already perform with this one key.
		return fmt.Sprintf("[relay %s %s] %s; relay resolve %s -- <decision>", n.Kind, n.Child, text, n.MessageID)
	}
	// Receipts need their durable ID too. It is the idempotency key used by
	// interactive delivery across supervisor and bridge restarts; omitting it
	// makes two identical terminal results indistinguishable on the pane.
	return fmt.Sprintf("[relay %s %s %s] %s", n.Kind, n.Child, n.MessageID, text)
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
	ho, err = effectiveLiveHandoff(p.Reg, ho)
	if err != nil {
		return err
	}
	sess, err := p.Reg.GetSession(ho.SessionID)
	if err != nil {
		return err
	}
	if ho.SourceSessionID == "" {
		if sess.Labels[ApexLabel] == "true" {
			// Intentional roots route only to their bound human authority surface.
		} else {
			return fmt.Errorf("handoff %s has no parent session", handoffID)
		}
	}
	if handoffTerminal(ho) {
		return nil
	}
	if p.NewTransport == nil {
		return fmt.Errorf("watch handoff %s: transport adapter unavailable", handoffID)
	}
	t, err := p.NewTransport(ho.HostID)
	if err != nil {
		return err
	}
	attempts := 0
	windowStart := time.Now()
	for {
		ended := false
		var routeFailure error
		from := ho.ParentSeq
		var subErr error
		if subErr = p.Coord.Ensure(ctx, t); subErr == nil {
			subErr = streamEvents(ctx, p.Coord, t, sess.Persist.Name, from, true, func(ev coord.Event) bool {
				latest, getErr := p.Reg.GetHandoff(handoffID)
				if getErr == nil {
					if effective, effectiveErr := effectiveLiveHandoff(p.Reg, latest); effectiveErr == nil {
						ho = effective
					}
				}
				if handoffTerminal(ho) {
					ended = true
					return false
				}
				msg, routeErr := p.RouteChildEvent(ctx, ho, ev)
				if routeErr != nil && msg == nil {
					routeFailure = routeErr
					return false
				}
				// A delivery error after envelope creation is retryable durable
				// state, so consuming the event is safe. An error before an
				// envelope exists must replay from the same cursor.
				if ev.Seq > ho.ParentSeq {
					ho.ParentSeq = ev.Seq
					if putErr := p.Reg.PutHandoff(ho); putErr != nil {
						ho.ParentSeq = from
						routeFailure = putErr
						return false
					}
				}
				ended = ev.Kind == "exit"
				return !ended
			})
		}
		if routeFailure != nil {
			subErr = routeFailure
		}
		if ended {
			return nil
		}
		if routeFailure != nil {
			// Local durability failures are not transport reconnects. Return them
			// to the supervisor so its single backoff/retry owner can restart from
			// the unchanged cursor.
			return routeFailure
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(windowStart) > 10*time.Minute {
			attempts, windowStart = 0, time.Now()
		}
		attempts++
		delay := time.Duration(1<<uint(min(attempts-1, 5))) * time.Second
		if attempts > 6 {
			// Keep the watcher durable without exceeding six SSH reconnects per
			// ten-minute window. A transient startup failure must not silently
			// sever the child's only path to its manager.
			delay = 10*time.Minute - time.Since(windowStart)
			if delay < time.Second {
				delay = time.Second
			}
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
	path := ParentWatchLockPath(handoffID)
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
	if msg.Kind == "permission_required" && ho.DeliveryState == EffectBlocked {
		if ho.PendingGate == nil {
			return nil, fmt.Errorf("blocked security gate has no durable decision surface; sent no keys")
		}
		choice, approved, err := resolveGateDecision(ho.PendingGate, text)
		if err != nil {
			return nil, err
		}
		if err := p.Sessions.ResolveGateChoice(ctx, msg.ChildSessionID, ho.PendingGate, choice); err != nil {
			return nil, err
		}
		if !approved {
			now := time.Now().UTC()
			ho.DeliveryState, ho.Status, ho.Outcome, ho.EndedAt = EffectDenied, StatusAbandoned, string(OutcomeAbandoned), &now
			ho.PendingGate = nil
			if err := p.Reg.PutHandoff(ho); err != nil {
				return nil, err
			}
			if err := p.Sessions.Destroy(ctx, msg.ChildSessionID, false); err != nil {
				ho.CleanupError = err.Error()
				_ = p.Reg.PutHandoff(ho)
				return nil, fmt.Errorf("gate denial recorded; cleanup failed: %w", err)
			}
		} else if ho.DeliveryState == EffectBlocked {
			if err := p.deliverPendingGoalAfterGate(ctx, ho, msg.ChildSessionID); err != nil {
				return nil, err
			}
		} else {
			ho.PendingGate, ho.Status = nil, StatusRunning
			if err := p.Reg.PutHandoff(ho); err != nil {
				return nil, err
			}
		}
	} else if err := p.Sessions.Send(ctx, msg.ChildSessionID, text, true); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	msg.State, msg.Reply, msg.RepliedAt, msg.AckedAt = ParentMessageReplied, text, &now, &now
	// The holder of the envelope is the session that ruled on it, which after
	// a failover is an ancestor rather than the intended manager.
	msg.ResolvedBySessionID = msg.ParentSessionID
	if err := writeParentMessage(msg, false); err != nil {
		return nil, err
	}
	if !handoffTerminal(ho) {
		ho.Status = StatusRunning
	}
	ho.UpdatedAt = now
	_ = p.Reg.PutHandoff(ho)
	_ = AppendCommunication(msg, "resolve", text)
	if p.Coord != nil && p.NewTransport != nil {
		if child, getErr := p.Reg.GetSession(msg.ChildSessionID); getErr == nil {
			if t, transportErr := p.NewTransport(child.HostID); transportErr == nil {
				_, _ = p.Coord.Emit(ctx, t, child.Persist.Name, "inject", map[string]any{"correlation_id": msg.CorrelationID, "message_id": msg.ID})
			}
		}
	}
	return msg, nil
}

func resolveGateDecision(gate *SecurityGate, decision string) (choice int, approved bool, err error) {
	decision = strings.TrimSpace(strings.ToLower(decision))
	wantPositive, semantic := false, false
	switch decision {
	case "approve", "approved", "allow", "yes", "trust", "continue":
		wantPositive, semantic = true, true
	case "deny", "denied", "reject", "no", "quit", "exit", "cancel":
		semantic = true
	}
	if index, parseErr := strconv.Atoi(decision); parseErr == nil {
		for _, candidate := range gate.Choices {
			if candidate.Index == index {
				label := strings.ToLower(candidate.Label)
				return index, !strings.Contains(label, "no") && !strings.Contains(label, "deny") && !strings.Contains(label, "quit") && !strings.Contains(label, "exit") && !strings.Contains(label, "cancel"), nil
			}
		}
	}
	if semantic {
		matches := []GateChoice{}
		for _, candidate := range gate.Choices {
			label := strings.ToLower(candidate.Label)
			negative := strings.Contains(label, "no") || strings.Contains(label, "deny") || strings.Contains(label, "reject") || strings.Contains(label, "quit") || strings.Contains(label, "exit") || strings.Contains(label, "cancel")
			positive := strings.Contains(label, "yes") || strings.Contains(label, "allow") || strings.Contains(label, "approve") || strings.Contains(label, "trust") || strings.Contains(label, "continue")
			if (wantPositive && positive && !negative) || (!wantPositive && negative) {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 1 {
			return matches[0].Index, wantPositive, nil
		}
	}
	return 0, false, fmt.Errorf("decision must select exactly one displayed gate choice by number or unambiguous approve/deny")
}

func (p *ParentService) deliverPendingGoalAfterGate(ctx context.Context, ho *Handoff, sessionID string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		capture, err := p.Sessions.Capture(ctx, sessionID, 40)
		if err != nil {
			return err
		}
		readiness := ClassifyAgentPane(capture)
		switch readiness.State {
		case AgentBlocked:
			return fmt.Errorf("security gate remains blocked after explicit decision; sent no goal")
		case AgentAbsent:
			return fmt.Errorf("agent exited after security-gate decision; sent no goal")
		case AgentReady:
			if err := p.Sessions.Send(ctx, sessionID, agentGoalPrompt(ho.Goal), true); err != nil {
				return err
			}
			ho.DeliveryState, ho.DeliveryError, ho.PendingGate = EffectAcknowledged, "", nil
			return p.Reg.PutHandoff(ho)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("agent readiness was not observable after explicit gate decision; sent no goal")
}

func (p *ParentService) Ack(messageID string) (*ParentMessage, error) {
	msg, err := p.FindMessage(messageID)
	if err != nil {
		return nil, err
	}
	if msg.State == ParentMessageAcked || msg.State == ParentMessageReplied {
		return msg, nil
	}
	if msg.Kind == "permission_required" {
		ho, getErr := p.Reg.GetHandoff(msg.HandoffID)
		if getErr != nil || !handoffTerminal(ho) {
			return nil, fmt.Errorf("permission decision %s requires explicit relay resolve", msg.ID)
		}
	}
	now := time.Now().UTC()
	msg.State, msg.AckedAt = ParentMessageAcked, &now
	msg.ResolvedBySessionID = msg.ParentSessionID
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

func (p *ParentService) Retire(ctx context.Context, sessionID string, dryRun, force, keepViz bool) (*RetirementGate, error) {
	gate, err := p.RetirementStatus(ctx, sessionID)
	if err != nil || (!gate.Eligible && !force) || dryRun {
		return gate, err
	}
	children, childErr := p.Reg.DirectChildren(sessionID)
	if childErr != nil {
		return gate, childErr
	}
	if len(children) > 0 {
		return gate, fmt.Errorf("session %s still manages %d direct child session(s); replace or reparent it first", sessionID, len(children))
	}
	handoffs, handoffErr := p.Reg.ListHandoffs()
	if handoffErr != nil {
		return gate, handoffErr
	}
	for _, handoff := range handoffs {
		if handoff.SourceSessionID == sessionID && !handoffTerminal(handoff) {
			return gate, fmt.Errorf("session %s still owns nonterminal handoff %s", sessionID, handoff.ID)
		}
	}
	var deletionViz ports.Viz
	if !keepViz {
		deletionViz = p.Viz
	}
	sess, sessErr := p.Reg.GetSession(sessionID)
	if sessErr != nil {
		return gate, sessErr
	}
	if err := DeleteSessionProjected(ctx, p.Reg, deletionViz, sess, keepViz); err != nil {
		return gate, err
	}
	gate.Closed = true
	_ = AppendLedger(map[string]any{"v": 1, "type": "parent_retire", "ts": time.Now().UTC().Format(time.RFC3339), "session_id": sessionID, "forced": force, "keep_viz": keepViz, "bypassed_reasons": gate.Reasons})
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
	if text == "" && (action == "request" || action == "event") {
		text = msg.Text
	}
	if summary := communicationSummary(text); summary != "" {
		record["summary"] = summary
	}
	if msg.PolicyID != "" {
		record["policy_id"] = msg.PolicyID
		record["auto_handled"] = true
	}
	return AppendLedger(record)
}

func communicationSummary(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 240 {
		text = text[:239] + "…"
	}
	return text
}
