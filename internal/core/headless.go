package core

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// A headless root is a manager that is a long-lived service rather than a
// terminal pane.
//
// Every other parent in relay is a cmux surface: registration binds one,
// delivery types into its composer, and liveness is read straight off the pane
// (ClassifyAgentPane). A container-hosted coordinator has none of that. It has
// no surface to bind, no composer to type into, and no screen to classify — so
// the pane-bound path does not merely degrade for it, it cannot start: without
// CMUX_SURFACE_REF, `relay parent register` fails before it writes anything,
// and the hierarchy ends up with no root at all. Children then escalate past
// an empty root straight to the human.
//
// Two things have to be answered to make such a manager first class.
//
// Delivery: for a headless root the durable inbox IS the channel. There is no
// second effect to reserve — the envelope is written, and the root reads it
// with `relay parent inbox`. Delivery is therefore confirmed at the moment the
// durable write lands, not when a pane echoes it back.
//
// Liveness: relay cannot observe a service the way it observes a pane, and it
// must not pretend otherwise. Guessing "registered means alive" is the failure
// mode that matters here, because an escalation delivered into a dead root is
// silently swallowed — it looks delivered and nobody ever answers it. So a
// headless root is live only while it says so: it heartbeats, on a declared
// TTL, and every inbox read renews it (a root that is doing its job proves its
// liveness by doing its job). Past the TTL the root is treated exactly like an
// absent pane: delivery reports the target unavailable, attention envelopes
// fail over to an ancestor, and anything with nowhere to go stays pending and
// visible instead of being marked delivered.
const (
	// HeadlessPersistKind marks a parent session with no pane behind it. It is
	// deliberately not LocalPersistKind: everything keyed on "cmux" (capture,
	// send, rename, deletion teardown) must NOT try to drive this session.
	HeadlessPersistKind = "headless"
	// HeadlessLabel marks the session as a headless parent.
	HeadlessLabel = "headless"
	// HeadlessWakeMode is the only wake mode a headless root can honour: the
	// durable inbox. inject needs a composer and notify needs a desktop
	// surface; neither exists here.
	HeadlessWakeMode = "inbox"

	heartbeatAtLabel  = "heartbeat_at"
	heartbeatTTLLabel = "heartbeat_ttl_s"
	wakeDegradedLabel = "wake_mode_degraded_from"

	// headlessDeliveryConfirmed records that the durable inbox write was the
	// delivery effect, so a receipt is never confused with a pane echo.
	headlessDeliveryConfirmed = "headless_inbox_confirmed"
)

// DefaultHeadlessTTL is long enough that an ordinary coordinator tick renews
// it comfortably, short enough that a wedged container is noticed within one
// working pause rather than the next day.
const DefaultHeadlessTTL = 15 * time.Minute

// Headless liveness states. "unknown" is never treated as live: a root that
// cannot say when it last ran is not a root that can be trusted with an
// escalation.
const (
	HeadlessFresh   = "fresh"
	HeadlessStale   = "stale"
	HeadlessUnknown = "unknown"
)

// HeadlessStatus is the reported liveness of a headless root. It is declared
// (heartbeat + TTL), never inferred, and it says so.
type HeadlessStatus struct {
	State         string `json:"state"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
	AgeSeconds    int64  `json:"age_seconds,omitempty"`
	TTLSeconds    int64  `json:"ttl_seconds"`
	Reason        string `json:"reason,omitempty"`
	WakeMode      string `json:"wake_mode,omitempty"`
	DegradedFrom  string `json:"wake_mode_degraded_from,omitempty"`
}

// IsHeadlessParent reports whether a session is a headless parent.
func IsHeadlessParent(sess *Session) bool {
	return sess != nil && sess.HostID == LocalHostID &&
		sess.Persist.Kind == HeadlessPersistKind && sess.Labels["role"] == ParentRole
}

// IsLocalParentSession covers both parent shapes — pane-bound and headless.
// Guards that exist to protect a manager's identity (rename, destroy, retire,
// listing) apply to a headless root exactly as they do to a pane.
func IsLocalParentSession(sess *Session) bool {
	return isLocalParent(sess) || IsHeadlessParent(sess)
}

func headlessTTL(sess *Session) time.Duration {
	if sess == nil {
		return DefaultHeadlessTTL
	}
	raw := strings.TrimSpace(sess.Labels[heartbeatTTLLabel])
	if raw == "" {
		return DefaultHeadlessTTL
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return DefaultHeadlessTTL
	}
	return time.Duration(seconds) * time.Second
}

// HeadlessHealth answers "is this root still there?" from declared facts only.
func HeadlessHealth(sess *Session, now time.Time) HeadlessStatus {
	ttl := headlessTTL(sess)
	status := HeadlessStatus{
		State:      HeadlessUnknown,
		TTLSeconds: int64(ttl / time.Second),
	}
	if sess == nil {
		status.Reason = "no session"
		return status
	}
	status.WakeMode = sess.Labels["wake_mode"]
	status.DegradedFrom = sess.Labels[wakeDegradedLabel]
	raw := strings.TrimSpace(sess.Labels[heartbeatAtLabel])
	if raw == "" {
		status.Reason = "no heartbeat recorded since registration"
		return status
	}
	beat, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		status.Reason = "unparseable heartbeat " + strconv.Quote(raw)
		return status
	}
	status.LastHeartbeat = beat.UTC().Format(time.RFC3339)
	age := now.Sub(beat)
	if age < 0 {
		age = 0
	}
	status.AgeSeconds = int64(age / time.Second)
	if age > ttl {
		status.State = HeadlessStale
		status.Reason = fmt.Sprintf("last heartbeat %s ago exceeds the declared %s TTL", age.Truncate(time.Second), ttl)
		return status
	}
	status.State = HeadlessFresh
	return status
}

// registerHeadless is the surface-free registration path. It is idempotent on
// the parent's name because that is the only stable identity a service has —
// there is no surface to key on, and a container start must converge rather
// than pile up a new root per boot.
func (p *ParentService) registerHeadless(opts RegisterParentOpts) (*Session, bool, error) {
	if p.Reg == nil {
		return nil, false, fmt.Errorf("parent registry required")
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return nil, false, fmt.Errorf("headless parent requires --name: it is the durable identity a service re-registers under")
	}
	name, err := shellquote.SanitizeSessionName(name)
	if err != nil {
		return nil, false, err
	}
	refs := normalizeRepoRefs(opts.RepoRefs)
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultHeadlessTTL
	}
	// inject needs a composer; notify needs a desktop surface. Neither exists.
	// Refusing would break an idempotent seed hook for a reason the caller
	// cannot fix, so record the request, degrade, and report it back.
	degradedFrom := ""
	switch strings.TrimSpace(opts.WakeMode) {
	case "", HeadlessWakeMode:
	case "inject", "notify":
		degradedFrom = strings.TrimSpace(opts.WakeMode)
	default:
		return nil, false, fmt.Errorf("wake mode must be inject, notify, or inbox")
	}
	now := time.Now().UTC()
	if list, err := p.Reg.ListSessions(); err == nil {
		for _, sess := range list {
			if !IsHeadlessParent(sess) || sess.Persist.Name != name {
				continue
			}
			applyHeadlessLabels(sess, ttl, degradedFrom, now)
			if len(refs) > 0 {
				sess.RepoRefs, sess.RepoRef = refs, refs[0]
			}
			if err := p.Reg.PutSession(sess); err != nil {
				return nil, false, err
			}
			return sess, false, nil
		}
	}
	sess := &Session{
		ID: newID("sess"), HostID: LocalHostID,
		Persist:   ports.PersistHandle{Kind: HeadlessPersistKind, Name: name},
		Labels:    map[string]string{"role": ParentRole, "local": "true", HeadlessLabel: "true", "parent_state": "active"},
		RepoRefs:  refs,
		CreatedAt: now, UpdatedAt: now,
	}
	if len(refs) > 0 {
		sess.RepoRef, sess.RemoteCWD = refs[0], refs[0]
	}
	applyHeadlessLabels(sess, ttl, degradedFrom, now)
	if err := p.Reg.PutSession(sess); err != nil {
		return nil, false, err
	}
	if err := AppendSessionStart(sess); err != nil {
		return nil, false, err
	}
	_ = AppendLedger(map[string]any{
		"v": 1, "type": "parent_register_headless", "ts": now.Format(time.RFC3339),
		"session_id": sess.ID, "name": name, "ttl_s": int64(ttl / time.Second), "repo_refs": refs,
	})
	return sess, true, nil
}

func applyHeadlessLabels(sess *Session, ttl time.Duration, degradedFrom string, now time.Time) {
	if sess.Labels == nil {
		sess.Labels = map[string]string{}
	}
	sess.Labels["role"] = ParentRole
	sess.Labels[HeadlessLabel] = "true"
	sess.Labels["wake_mode"] = HeadlessWakeMode
	sess.Labels[heartbeatTTLLabel] = strconv.FormatInt(int64(ttl/time.Second), 10)
	sess.Labels[heartbeatAtLabel] = now.Format(time.RFC3339)
	if sess.Labels["parent_state"] == "" {
		sess.Labels["parent_state"] = "active"
	}
	if degradedFrom != "" {
		sess.Labels[wakeDegradedLabel] = degradedFrom
	} else {
		delete(sess.Labels, wakeDegradedLabel)
	}
}

// Heartbeat renews a headless root's declared liveness. It is the one thing a
// service must keep doing to stay a root, and it is cheap enough to run on
// every coordinator tick.
func (p *ParentService) Heartbeat(parentID string) (*Session, error) {
	if p == nil || p.Reg == nil {
		return nil, fmt.Errorf("parent registry required")
	}
	sess, err := p.Reg.GetSession(parentID)
	if err != nil {
		return nil, err
	}
	if !IsHeadlessParent(sess) {
		return nil, fmt.Errorf("session %s is not a headless parent; pane-bound managers prove liveness through their pane", parentID)
	}
	if sess.Labels == nil {
		sess.Labels = map[string]string{}
	}
	sess.Labels[heartbeatAtLabel] = time.Now().UTC().Format(time.RFC3339)
	if err := p.Reg.PutSession(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// TouchHeadless renews liveness as a side effect of the root actually working
// its inbox. It is best-effort by design: a read must never fail because the
// heartbeat could not be written.
func (p *ParentService) TouchHeadless(parentID string) {
	if p == nil || p.Reg == nil || parentID == "" {
		return
	}
	if sess, err := p.Reg.GetSession(parentID); err == nil && IsHeadlessParent(sess) {
		_, _ = p.Heartbeat(parentID)
	}
}

// deliverHeadless is the headless delivery effect: the durable envelope is the
// message. It refuses while the root is not demonstrably live, and reports that
// as target-unavailable so the escalation path fails over instead of parking a
// decision in a service nobody is running.
func (p *ParentService) deliverHeadless(parent *Session, msg *ParentMessage) error {
	health := HeadlessHealth(parent, time.Now().UTC())
	if health.State != HeadlessFresh {
		return &parentDeliveryError{
			err:         fmt.Errorf("headless parent %s is %s: %s", parent.ID, health.State, health.Reason),
			unavailable: true,
		}
	}
	claimed, err := claimParentDelivery(msg, parent.ID)
	if err != nil {
		return fmt.Errorf("reserve headless delivery: %w", err)
	}
	if !claimed {
		return nil
	}
	return finalizeParentDelivery(msg, headlessDeliveryConfirmed, "", true)
}

// HeadlessIdentity is the credential a headless root's holder process needs to
// operate through relay's authenticated command boundary.
//
// A pane parent gets this for free: relay injects RELAY_BRIDGE_SOCK and a
// per-session token into the tmux session it owns, so the agent inside can run
// `relay parent inbox` and have it executed against the authority. A service in
// another container has no such session — nothing ever injected anything into
// it — so it has to be handed the same two facts explicitly. This is the same
// identity, issued the same way and authorized by the same policy
// (authorizeOperation confines it to its own lineage); the only difference is
// who carries it.
type HeadlessIdentity struct {
	V         int    `json:"v"`
	SessionID string `json:"session_id"`
	HostID    string `json:"host_id"`
	Token     string `json:"token"`
	Socket    string `json:"socket"`
}

// EnsureHeadlessBridgeIdentity issues (or re-reads) the headless root's bridge
// token. It is idempotent: re-running the seed hook must not rotate a
// credential the running holder is already using.
func EnsureHeadlessBridgeIdentity(sessionID string) (*HeadlessIdentity, error) {
	sess, err := (&Registry{}).GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if !IsHeadlessParent(sess) {
		return nil, fmt.Errorf("session %s is not a headless parent", sessionID)
	}
	token := ""
	if raw, readErr := os.ReadFile(bridgeTokenPath(sess.ID)); readErr == nil {
		token = strings.TrimSpace(string(raw))
	}
	if token == "" {
		token = newID("br")
		if err := rememberBridgeToken(sess.ID, token); err != nil {
			return nil, err
		}
	}
	return &HeadlessIdentity{V: 1, SessionID: sess.ID, HostID: sess.HostID, Token: token, Socket: DesktopBridgeSocketPath()}, nil
}
