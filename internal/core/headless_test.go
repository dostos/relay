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

// State transitions survive the hierarchy retirement: a headless mailbox still
// reports whether it is working or idle, which is what liveness reads.
func TestHeadlessParentSupportsState(t *testing.T) {
	service, _, _ := newParentTestService(t)
	sess, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "Apex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetState(sess.ID, "idle"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	got, err := service.Reg.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !IsHeadlessParent(got) {
		t.Fatal("registered headless parent lost its headless marker")
	}
}

// A manager addresses itself by writing nothing. That rule outlived the
// authority policy it used to be enforced by; it is argument parsing now.
func TestParentVerbTargetSelfScoping(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"own inbox unnamed", []string{"inbox"}, ""},
		{"own inbox unnamed with flag", []string{"inbox", "--all"}, ""},
		{"named inbox", []string{"inbox", "sess-other"}, "sess-other"},
		{"flag first stays self", []string{"--all"}, ""},
		{"nothing at all", []string{}, ""},
	} {
		if got := ParentVerbTarget(tc.args[min(1, len(tc.args)):]); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
