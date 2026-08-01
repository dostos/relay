package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Autonomous mode is not a flag — it is a shape. A subtree is governed when it
// has an agent-root ancestor, so turning it on means enrolling a root under an
// always-on apex, and turning it off means unenrolling. Nothing else changes.
//
// Relay stays model-free here: it owns enrollment, where the rules live, and
// the audit. The judgment lives in the apex agent (share/roles/relay-conductor.md).

const (
	// ApexLabel marks the always-on session that governs enrolled roots.
	ApexLabel = "apex"
	// GovernedLabel marks a root that has been placed under the apex.
	GovernedLabel = "governed"
)

// RootService manages the apex and the roots enrolled under it.
type RootService struct {
	Reg *Registry
}

// RulesDir is where human-authored per-project rules live. It defaults inside
// the relay config root, but RELAY_RULES_DIR lets it point at a versioned
// workspace directory — rules are the human's delegation envelope, and keeping
// them in version control is the point.
func RulesDir() string {
	if v := strings.TrimSpace(os.Getenv("RELAY_RULES_DIR")); v != "" {
		return v
	}
	return filepath.Join(ConfigRoot(), "rules")
}

// RulesPath resolves one project's rule file.
func RulesPath(project string) (string, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", fmt.Errorf("project required")
	}
	if strings.ContainsAny(project, "/\\") || project == ".." {
		return "", fmt.Errorf("project %q must be a bare name", project)
	}
	return filepath.Join(RulesDir(), project+".md"), nil
}

// Apex returns the current apex session, or an error when none is designated.
func (r *RootService) Apex() (*Session, error) {
	if r == nil || r.Reg == nil {
		return nil, fmt.Errorf("root service not configured")
	}
	list, err := r.Reg.ListSessions()
	if err != nil {
		return nil, err
	}
	for _, sess := range list {
		if sess.Labels[ApexLabel] == "true" {
			return sess, nil
		}
	}
	return nil, fmt.Errorf("no apex designated; run relay root adopt SESSION")
}

// Adopt designates a session as the apex. The apex must itself be a root: an
// apex with a manager above it would mean the human is no longer the last
// escalation stop, which is the one thing autonomous mode must not do.
func (r *RootService) Adopt(sessionID string) (*Session, error) {
	if r == nil || r.Reg == nil {
		return nil, fmt.Errorf("root service not configured")
	}
	sess, err := r.Reg.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.SourceSessionID != "" {
		return nil, fmt.Errorf("session %s has a manager and cannot be the apex", sessionID)
	}
	if existing, err := r.Apex(); err == nil && existing.ID != sess.ID {
		return nil, fmt.Errorf("apex already designated (%s); retire it first", existing.ID)
	}
	if sess.Labels == nil {
		sess.Labels = map[string]string{}
	}
	sess.Labels[ApexLabel] = "true"
	if err := r.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// Enroll places a root under the apex, which is what makes its subtree
// governed. Escalation then reaches the apex whenever the enrolled root cannot
// receive it — including while the human's laptop is asleep.
func (r *RootService) Enroll(sessionID string) (*Session, error) {
	apex, err := r.Apex()
	if err != nil {
		return nil, err
	}
	sess, err := r.Reg.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.ID == apex.ID {
		return nil, fmt.Errorf("the apex cannot be enrolled under itself")
	}
	if sess.SourceSessionID != "" && sess.SourceSessionID != apex.ID {
		return nil, fmt.Errorf("session %s already reports to %s", sess.ID, sess.SourceSessionID)
	}
	// Guard the one cycle this command could create.
	for _, ancestor := range AncestorChain(r.Reg, apex.ID) {
		if ancestor.ID == sess.ID {
			return nil, fmt.Errorf("enrolling %s would place the apex under it", sess.ID)
		}
	}
	sess.SourceSessionID = apex.ID
	sess.SourceHostID = apex.HostID
	sess.SourcePersistName = apex.Persist.Name
	if sess.Labels == nil {
		sess.Labels = map[string]string{}
	}
	sess.Labels[GovernedLabel] = "true"
	if err := r.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// Unenroll detaches a root from the apex, returning it to reporting directly
// to the human. Autonomous mode is structural, so this is the whole off switch.
func (r *RootService) Unenroll(sessionID string) (*Session, error) {
	apex, err := r.Apex()
	if err != nil {
		return nil, err
	}
	sess, err := r.Reg.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	// Enrollment is what GovernedLabel records. Without checking it, unenroll
	// would also detach workers the apex spawned itself — they report to the
	// apex without ever having been enrolled — silently orphaning live work.
	if sess.SourceSessionID != apex.ID || sess.Labels[GovernedLabel] != "true" {
		return nil, fmt.Errorf("session %s is not enrolled", sessionID)
	}
	sess.SourceSessionID, sess.SourceHostID, sess.SourcePersistName = "", "", ""
	delete(sess.Labels, GovernedLabel)
	if err := r.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// Governed lists the roots currently enrolled under the apex.
func (r *RootService) Governed() ([]*Session, error) {
	apex, err := r.Apex()
	if err != nil {
		return nil, err
	}
	list, err := r.Reg.ListSessions()
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, sess := range list {
		// Enrolled roots only — not every worker the apex happens to manage.
		if sess.SourceSessionID == apex.ID && sess.Labels[GovernedLabel] == "true" {
			out = append(out, sess)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RootDigest is the "while you were away" answer. It leads with what still
// needs the human and reports everything else as counts: an agent returning to
// its desk should pay for the decisions it must make, not for a replay.
type RootDigest struct {
	Held       []ParentInboxItem `json:"held"`
	HeldCount  int               `json:"held_count"`
	Ruled      int               `json:"ruled"`
	AutoGate   int               `json:"auto_gate"`
	FailedOver int               `json:"failed_over"`
	NextAfter  int64             `json:"next_after"`
}

// Digest summarises what happened under the apex since a cursor. Pending
// attention envelopes are returned in full because they are the human's actual
// work; resolved ones collapse to counts.
func (r *RootService) Digest(parents *ParentService, after int64) (*RootDigest, error) {
	apex, err := r.Apex()
	if err != nil {
		return nil, err
	}
	if parents == nil {
		return nil, fmt.Errorf("parent service required")
	}
	digest := &RootDigest{Held: []ParentInboxItem{}, NextAfter: after}
	messages, err := parents.ListMessages(apex.ID, false)
	if err != nil {
		return nil, err
	}
	for _, msg := range messages {
		switch {
		case msg.State == ParentMessagePending && attentionMessage(msg.Kind):
			digest.Held = append(digest.Held, CompactParentMessage(msg, true))
		case msg.AutoHandled:
			digest.AutoGate++
		case msg.State == ParentMessageReplied:
			digest.Ruled++
		}
		if msg.IntendedParentSessionID != "" {
			digest.FailedOver++
		}
	}
	digest.HeldCount = len(digest.Held)
	page, err := LoadCommunicationPage(apex.ID, "", after, 100)
	if err == nil {
		digest.NextAfter = page.NextAfter
	}
	return digest, nil
}
