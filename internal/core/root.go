package core

import (
	"context"
	"encoding/json"
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
	Reg      *Registry
	Sessions *SessionService
}

// ControlPlane describes where governance actually runs. Enrolling a root
// makes its subtree governed, but governance only happens while the machine
// holding the registry, the inboxes, and the watcher processes is awake — a
// laptop that sleeps takes the router with it, and an escalation raised in the
// meantime is not routed until it wakes. Relay cannot detect that on its own,
// so it never claims always-on without being told.
type ControlPlane struct {
	Host       string `json:"host"`
	HostID     string `json:"host_id,omitempty"`
	AlwaysOn   bool   `json:"always_on"`
	DeclaredBy string `json:"declared_by,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

type controlPlaneDeclaration struct {
	V        int    `json:"v"`
	HostID   string `json:"host_id"`
	AlwaysOn bool   `json:"always_on"`
}

func controlPlaneDeclarationPath() string {
	return filepath.Join(ConfigRoot(), "control-plane.json")
}

// DescribeControlPlane reports the locality caveat honestly. Declare
// RELAY_CONTROL_PLANE_ALWAYS_ON=1 only on a host that does not sleep.
func DescribeControlPlane() ControlPlane {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "this machine"
	}
	cp := ControlPlane{Host: host}
	if raw, err := os.ReadFile(filepath.Join(ConfigRoot(), "host.yaml")); err == nil {
		if profile, parseErr := ParseHostProfileYAML(raw); parseErr == nil {
			cp.HostID = strings.TrimSpace(profile.HostID)
		}
	}
	if raw, err := os.ReadFile(controlPlaneDeclarationPath()); err == nil {
		var declaration controlPlaneDeclaration
		if json.Unmarshal(raw, &declaration) == nil && declaration.V == 1 && cp.HostID != "" && declaration.HostID == cp.HostID && declaration.AlwaysOn {
			cp.AlwaysOn = true
			cp.DeclaredBy = "host_config"
		}
	}
	switch strings.TrimSpace(os.Getenv("RELAY_CONTROL_PLANE_ALWAYS_ON")) {
	case "1", "true", "yes":
		cp.AlwaysOn = true
		cp.DeclaredBy = "environment"
	}
	if !cp.AlwaysOn {
		cp.Warning = "governance runs on " + host +
			"; enrolled subtrees are governed only while it is awake. An escalation raised" +
			" while it sleeps is not routed until it wakes. Run relay root control-plane --always-on" +
			" only on a host that does not sleep."
	}
	return cp
}

// SetLocalControlPlaneAlwaysOn persists the human's availability policy in a
// host-identity-bound config so shells, relayd, the supervisor and restarts all
// report the same value. This is a declaration, not a liveness inference.
func SetLocalControlPlaneAlwaysOn(alwaysOn bool) (*ControlPlane, error) {
	hostPath := filepath.Join(ConfigRoot(), "host.yaml")
	raw, err := os.ReadFile(hostPath)
	if err != nil {
		return nil, fmt.Errorf("read local host profile: %w", err)
	}
	profile, err := ParseHostProfileYAML(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(profile.HostID) == "" {
		return nil, fmt.Errorf("local host profile has no host_id")
	}
	next, err := json.MarshalIndent(controlPlaneDeclaration{V: 1, HostID: strings.TrimSpace(profile.HostID), AlwaysOn: alwaysOn}, "", "  ")
	if err != nil {
		return nil, err
	}
	path := controlPlaneDeclarationPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := writeOwnerFile(tmp, append(next, '\n')); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(tmp, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return nil, err
	}
	if err := dir.Close(); err != nil {
		return nil, err
	}
	cp := DescribeControlPlane()
	return &cp, nil
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
		return nil, fmt.Errorf("apex already designated (%s); run: relay root release %s", existing.ID, existing.ID)
	}
	// An apex whose agent is inert is worse than no apex: escalations arrive
	// into a pane that will never answer them, and nothing says so.
	if r.Sessions != nil {
		ready := r.AgentReadinessFor(context.Background(), r.Sessions, sess.ID)
		switch ready.State {
		case AgentBlocked:
			return nil, fmt.Errorf(
				"apex agent is blocked (%s) — answer it yourself in the pane; relay will not answer a security gate for you",
				ready.Reason)
		case AgentAbsent:
			return nil, fmt.Errorf(
				"no agent is running in session %s (%s); start the conductor before adopting it as the apex",
				sess.ID, ready.Reason)
		}
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

// Release un-designates the apex so a different session can take over — for
// example moving governance from a laptop cmux pane to an always-on host.
//
// It refuses while roots are still enrolled. Clearing the label with roots
// attached would leave them reporting to a session that is no longer an apex:
// still governed by the lineage, but invisible to `root status`, which is a
// worse state than either end of the swap.
func (r *RootService) Release(sessionID string) (*Session, error) {
	apex, err := r.Apex()
	if err != nil {
		return nil, err
	}
	if apex.ID != sessionID {
		return nil, fmt.Errorf("session %s is not the apex (%s is)", sessionID, apex.ID)
	}
	governed, err := r.Governed()
	if err != nil {
		return nil, err
	}
	if len(governed) > 0 {
		ids := make([]string, 0, len(governed))
		for _, sess := range governed {
			ids = append(ids, sess.ID)
		}
		return nil, fmt.Errorf(
			"%d root(s) still enrolled (%s); unenroll them first",
			len(ids), strings.Join(ids, ", "))
	}
	delete(apex.Labels, ApexLabel)
	if err := r.Reg.PutSession(apex); err != nil {
		return nil, err
	}
	return apex, nil
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
	// A parent edge without a live handoff is only a drawable line. Handoffs
	// own the event stream, sensors and watcher cursor; marking a bare pane as
	// governed would let composer delivery report success with no response
	// path. Direct human panes remain usable through explicit delivery-only
	// sends, but cannot advertise autonomous governance.
	handoffs, err := r.Reg.ListHandoffs()
	if err != nil {
		return nil, err
	}
	var channel *Handoff
	for _, ho := range handoffs {
		if ho.SessionID == sess.ID && !handoffTerminal(ho) {
			if channel != nil {
				return nil, fmt.Errorf("session %s has multiple live handoff event channels", sess.ID)
			}
			channel = ho
		}
	}
	if channel == nil {
		return nil, fmt.Errorf("session %s has no live handoff event channel; relaunch it as a managed handoff before enrollment", sess.ID)
	}
	if channel.SourceSessionID != "" && channel.SourceSessionID != apex.ID {
		return nil, fmt.Errorf("handoff %s already reports to %s", channel.ID, channel.SourceSessionID)
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
