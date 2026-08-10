package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// A headless root has no cmux surface. Registration must therefore never
// consult one — the pane-bound path fails outright outside a surface.
func TestRegisterHeadlessParentNeedsNoSurface(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	// Reproduce a container: no surface env, and no cmux to ask either.
	t.Setenv("CMUX_SURFACE_REF", "")
	t.Setenv("CMUX_SURFACE", "")
	previous := identifySurface
	identifySurface = func() (string, error) { return "", errors.New("no cmux in this process") }
	t.Cleanup(func() { identifySurface = previous })
	repo := t.TempDir()

	if _, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Name: "Apex", RepoRefs: []string{repo}}); err == nil {
		t.Fatalf("pane-bound registration outside a surface must fail")
	}

	sess, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{
		Headless: true, Name: "Apex", RepoRefs: []string{repo},
	})
	if err != nil || !created {
		t.Fatalf("headless register created=%v err=%v", created, err)
	}
	if !IsHeadlessParent(sess) {
		t.Fatalf("session is not a headless parent: %+v", sess)
	}
	if sess.VizSurfaceRef != "" {
		t.Fatalf("headless parent must not claim a surface: %q", sess.VizSurfaceRef)
	}
	if len(notifier.bound) != 0 {
		t.Fatalf("headless registration must not bind a cmux surface: %v", notifier.bound)
	}
	if sess.Labels["wake_mode"] != HeadlessWakeMode {
		t.Fatalf("wake mode = %q", sess.Labels["wake_mode"])
	}
	if sess.Labels[heartbeatAtLabel] == "" {
		t.Fatalf("registration must record a heartbeat: %+v", sess.Labels)
	}
	stored, err := reg.GetSession(sess.ID)
	if err != nil || stored.Labels["parent_state"] != "active" {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}
}

// The seed hook runs on every container start. Registration is keyed by name,
// not by a surface that does not exist, so re-running must converge.
func TestRegisterHeadlessParentIsIdempotentByName(t *testing.T) {
	service, _, _ := newParentTestService(t)
	first, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Apex"})
	if err != nil || !created {
		t.Fatalf("first register created=%v err=%v", created, err)
	}
	second, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Apex"})
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("re-register = %+v created=%v err=%v", second, created, err)
	}
	other, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Other"})
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("distinct name must be a distinct parent: %+v created=%v err=%v", other, created, err)
	}
}

// inject/notify both need a surface. Registration must not fabricate one, and
// must not fail the hook either: it degrades and says so.
func TestHeadlessRegistrationDegradesPaneWakeModes(t *testing.T) {
	service, _, _ := newParentTestService(t)
	for _, requested := range []string{"inject", "notify"} {
		sess, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Apex-" + requested, WakeMode: requested})
		if err != nil {
			t.Fatalf("wake %s: %v", requested, err)
		}
		if sess.Labels["wake_mode"] != HeadlessWakeMode {
			t.Fatalf("wake %s effective = %q", requested, sess.Labels["wake_mode"])
		}
		if got := sess.Labels[wakeDegradedLabel]; got != requested {
			t.Fatalf("wake %s degraded label = %q", requested, got)
		}
	}
}

func TestHeadlessHealthTracksHeartbeatTTL(t *testing.T) {
	service, _, _ := newParentTestService(t)
	sess, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Apex", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if health := HeadlessHealth(sess, now); health.State != HeadlessFresh {
		t.Fatalf("fresh health = %+v", health)
	}
	if health := HeadlessHealth(sess, now.Add(2*time.Minute)); health.State != HeadlessStale {
		t.Fatalf("stale health = %+v", health)
	}
	beat, err := service.Heartbeat(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if health := HeadlessHealth(beat, time.Now().UTC()); health.State != HeadlessFresh {
		t.Fatalf("post-heartbeat health = %+v", health)
	}
}

func headlessDeliveryFixture(t *testing.T, ttl time.Duration) (*ParentService, *Registry, *Session, *Handoff) {
	t.Helper()
	service, _, reg := newParentTestService(t)
	parent, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Apex", TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	child := &Session{ID: "sess-child", HostID: "madrid", Persist: ports.PersistHandle{Kind: "tmux", Name: "child"},
		Labels: map[string]string{"agent": "codex"}, SourceSessionID: parent.ID, CreatedAt: now, UpdatedAt: now}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	ho := &Handoff{ID: "ho-child", SessionID: child.ID, HostID: "madrid", Kind: KindAgent, Status: StatusRunning,
		SourceSessionID: parent.ID, CreatedAt: now, UpdatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	return service, reg, parent, ho
}

// The durable inbox IS the headless channel: there is no pane to capture and
// no desktop surface to flash, so a written envelope is a delivered envelope.
func TestHeadlessDeliveryUsesDurableInbox(t *testing.T) {
	service, _, parent, ho := headlessDeliveryFixture(t, time.Hour)
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Kind: "ask", Meta: map[string]any{"text": "which branch?"}, Seq: 1})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if msg == nil {
		t.Fatal("no envelope created")
	}
	stored, err := service.FindMessage(msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ParentSessionID != parent.ID {
		t.Fatalf("envelope owner = %s", stored.ParentSessionID)
	}
	if stored.DeliveredAt == nil || stored.DeliveryMethod != headlessDeliveryConfirmed {
		t.Fatalf("delivery = %q at=%v", stored.DeliveryMethod, stored.DeliveredAt)
	}
	items, err := service.ListMessages(parent.ID, true)
	if err != nil || len(items) != 1 || items[0].Kind != "ask" {
		t.Fatalf("inbox = %+v err=%v", items, err)
	}
}

// A stale root is a dead root. The envelope must stay pending and visible,
// never be marked delivered into a service that stopped answering.
func TestHeadlessDeliveryRefusesWhenHeartbeatIsStale(t *testing.T) {
	service, reg, parent, ho := headlessDeliveryFixture(t, time.Minute)
	parent.Labels[heartbeatAtLabel] = time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := reg.PutSession(parent); err != nil {
		t.Fatal(err)
	}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Kind: "ask", Meta: map[string]any{"text": "which branch?"}, Seq: 1})
	if msg == nil {
		t.Fatalf("envelope must exist even when the root is stale (err=%v)", err)
	}
	if err == nil {
		t.Fatal("stale headless delivery must surface an error, not report success")
	}
	if !strings.Contains(err.Error(), "heartbeat") {
		t.Fatalf("error must name the liveness reason: %v", err)
	}
	stored, findErr := service.FindMessage(msg.ID)
	if findErr != nil {
		t.Fatal(findErr)
	}
	if stored.DeliveredAt != nil || stored.State != ParentMessagePending {
		t.Fatalf("stale delivery must not be acknowledged: %+v", stored)
	}
	pending, err := service.ListMessages(parent.ID, true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("escalation must remain visible: %+v err=%v", pending, err)
	}
}

// A stale headless manager must not swallow an escalation that an ancestor
// could still answer.
func TestStaleHeadlessManagerFailsOverToAncestor(t *testing.T) {
	service, reg, headless, ho := headlessDeliveryFixture(t, time.Minute)
	now := time.Now().UTC()
	grandparent, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Root", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	headless.SourceSessionID = grandparent.ID
	headless.Labels[heartbeatAtLabel] = now.Add(-2 * time.Hour).Format(time.RFC3339)
	if err := reg.PutSession(headless); err != nil {
		t.Fatal(err)
	}
	msg, err := service.RouteChildEvent(context.Background(), ho, coord.Event{Kind: "ask", Meta: map[string]any{"text": "deploy?"}, Seq: 1})
	if err != nil {
		t.Fatalf("failover route: %v", err)
	}
	stored, err := service.FindMessage(msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ParentSessionID != grandparent.ID || stored.IntendedParentSessionID != headless.ID {
		t.Fatalf("envelope did not fail over: %+v", stored)
	}
}

func TestHeadlessParentSupportsStateAndRetirement(t *testing.T) {
	service, _, _ := newParentTestService(t)
	sess, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Apex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetState(sess.ID, "idle"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	gate, err := service.RetirementStatus(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("retirement status: %v", err)
	}
	if gate.Headless == nil || gate.Headless.State != HeadlessFresh {
		t.Fatalf("retirement gate must report headless health: %+v", gate.Headless)
	}
	if _, err := service.Retire(context.Background(), sess.ID, false, true, true); err != nil {
		t.Fatalf("forced retire: %v", err)
	}
	if _, err := service.Reg.GetSession(sess.ID); err == nil {
		t.Fatal("retired headless parent must be gone from the registry")
	}
}

// Adoption is the whole point: an orphaned handoff must be reparentable onto a
// root that has no pane to project the child into.
func TestHeadlessParentAdoptsAndReparentsHandoffs(t *testing.T) {
	service, reg, headless, ho := headlessDeliveryFixture(t, time.Hour)
	viz := &fakeRetirementViz{}
	service.Viz = viz
	other, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Other", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	moved, oldParent, err := service.ReparentChild(other.ID, ho.ID)
	if err != nil {
		t.Fatalf("reparent onto headless parent: %v", err)
	}
	if oldParent != headless.ID || moved.SourceSessionID != other.ID {
		t.Fatalf("reparent = %+v old=%s", moved, oldParent)
	}
	child, err := reg.GetSession(ho.SessionID)
	if err != nil || child.SourceSessionID != other.ID {
		t.Fatalf("child lineage = %+v err=%v", child, err)
	}
	if len(viz.presented) != 0 {
		t.Fatalf("a headless parent has no pane to nest a child under: %+v", viz.presented)
	}
}

// A headless root operates through the authenticated command boundary, because
// nothing ever injected an identity into it. The policy must confine it to its
// own lineage exactly as it confines a pane manager.
func TestHeadlessRootAuthorityIsLineageConfined(t *testing.T) {
	service, reg, headless, ho := headlessDeliveryFixture(t, time.Hour)
	stranger, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Stranger", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		actor *Session
		args  []string
		want  bool
	}{
		{"own inbox", headless, []string{"parent", "inbox", headless.ID}, true},
		{"own heartbeat", headless, []string{"parent", "heartbeat", headless.ID}, true},
		{"own child adoption", headless, []string{"parent", "move", headless.ID, ho.ID}, true},
		{"stranger inbox", stranger, []string{"parent", "inbox", headless.ID}, false},
		{"stranger heartbeat", stranger, []string{"parent", "heartbeat", headless.ID}, false},
		{"stranger child", stranger, []string{"parent", "move", stranger.ID, ho.ID}, false},
	} {
		allowed, reason := authorizeOperation(reg, tc.actor, tc.args)
		if allowed != tc.want {
			t.Fatalf("%s: allowed=%v (%s)", tc.name, allowed, reason)
		}
	}
}

// A manager that cannot name itself is not a manager. Its identity arrives from
// the authenticated boundary, not from an argument it was told to remember, so
// the verbs whose only subject is the manager itself must work with no subject
// written down. Confinement is unchanged: naming somebody else is still
// refused, and it is refused for saying so, not for saying nothing.
func TestManagerActsOnItselfWithoutNamingItself(t *testing.T) {
	_, reg, headless, _ := headlessDeliveryFixture(t, time.Hour)
	now := time.Now().UTC()
	stranger := &Session{ID: "sess-stranger", Persist: ports.PersistHandle{Name: "stranger"}, CreatedAt: now, UpdatedAt: now}
	if err := reg.PutSession(stranger); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		actor  *Session
		args   []string
		want   bool
		reason string
	}{
		{"own inbox unnamed", headless, []string{"parent", "inbox"}, true, "manager's own inbox"},
		{"own inbox unnamed with flag", headless, []string{"parent", "inbox", "--all"}, true, "manager's own inbox"},
		{"own log unnamed", headless, []string{"parent", "log"}, true, "manager's own log"},
		{"own status unnamed", headless, []string{"parent", "status"}, true, "manager's own status"},
		{"own sweep unnamed", headless, []string{"parent", "sweep"}, true, "manager's own sweep"},
		{"own heartbeat unnamed", headless, []string{"parent", "heartbeat"}, true, "manager's own heartbeat"},
		{"own inbox named", headless, []string{"parent", "inbox", headless.ID}, true, "manager lineage authority"},
		{"stranger inbox named", stranger, []string{"parent", "inbox", headless.ID}, false, "parent target is outside caller lineage"},
		// Destructive and two-positional verbs keep requiring the id, and say
		// so: "outside caller lineage" for an argument nobody wrote is a
		// refusal that describes the wrong problem.
		{"retire unnamed", headless, []string{"parent", "retire"}, false, "parent retire requires a PARENT"},
		{"state unnamed", headless, []string{"parent", "state"}, false, "parent state requires a PARENT"},
	} {
		allowed, reason := authorizeOperation(reg, tc.actor, tc.args)
		if allowed != tc.want || reason != tc.reason {
			t.Fatalf("%s: allowed=%v reason=%q want %v %q", tc.name, allowed, reason, tc.want, tc.reason)
		}
	}
}
