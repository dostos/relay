package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type replacementProjectionViz struct {
	fail  bool
	items []ports.Presentation
}

func (*replacementProjectionViz) Kind() string                   { return "test" }
func (*replacementProjectionViz) Available(context.Context) bool { return true }
func (*replacementProjectionViz) Present(context.Context, string, string, ports.Layout) (string, error) {
	return "", nil
}
func (*replacementProjectionViz) Focus(context.Context, string) error                  { return nil }
func (*replacementProjectionViz) Close(context.Context, string) error                  { return nil }
func (*replacementProjectionViz) Layout(context.Context) (string, error)               { return "", nil }
func (*replacementProjectionViz) SaveRestorable(context.Context) (int, error)          { return 0, nil }
func (*replacementProjectionViz) RestoreSaved(context.Context) (int, error)            { return 0, nil }
func (*replacementProjectionViz) BrandLabels(context.Context, map[string]string) error { return nil }
func (v *replacementProjectionViz) ApplyProjection(_ context.Context, event ports.ProjectionEvent) (string, error) {
	if v.fail {
		return "", errors.New("viz offline")
	}
	v.items = append(v.items, event.Item)
	return "queued", nil
}

func newRootTestService(t *testing.T) (*RootService, *Registry) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	now := time.Now().UTC()
	// An always-on apex candidate and two project roots, all currently roots.
	for _, id := range []string{"sess-apex", "sess-proj-a", "sess-proj-b"} {
		sess := &Session{
			ID: id, HostID: "home",
			Persist:   ports.PersistHandle{Kind: "tmux", Name: id},
			CreatedAt: now,
		}
		if err := reg.PutSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	return &RootService{Reg: reg}, reg
}

func TestEnrollPlacesARootUnderTheApex(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	got, err := reg.GetSession("sess-proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceSessionID != "sess-apex" {
		t.Fatalf("want the apex as manager, got %q", got.SourceSessionID)
	}
	if got.Labels[GovernedLabel] != "true" {
		t.Fatalf("want the root marked governed, labels=%v", got.Labels)
	}
	// Escalation from the enrolled root now has the apex as a live ancestor.
	chain := AncestorChain(reg, "sess-proj-a")
	if len(chain) != 1 || chain[0].ID != "sess-apex" {
		t.Fatalf("apex must be the escalation ancestor, got %+v", chain)
	}
}

// Autonomous mode is structural, so unenrolling is the whole off switch.
func TestUnenrollReturnsARootToTheHuman(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Unenroll("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.GetSession("sess-proj-a")
	if got.SourceSessionID != "" {
		t.Fatalf("want a root again, got manager %q", got.SourceSessionID)
	}
	if got.Labels[GovernedLabel] != "" {
		t.Fatalf("governed label must be cleared, labels=%v", got.Labels)
	}
	if len(AncestorChain(reg, "sess-proj-a")) != 0 {
		t.Fatal("an unenrolled root must escalate to the human directly")
	}
}

// The apex must be the last escalation stop before the human.
func TestApexMustItselfBeARoot(t *testing.T) {
	root, reg := newRootTestService(t)
	managed, _ := reg.GetSession("sess-proj-a")
	managed.SourceSessionID = "sess-proj-b"
	if err := reg.PutSession(managed); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Adopt("sess-proj-a"); err == nil {
		t.Fatal("a session with a manager must not become the apex")
	}
}

func TestEnrollRefusesToCreateACycleOrStealAManagedRoot(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-apex"); err == nil {
		t.Fatal("the apex must not be enrolled under itself")
	}
	// A root that already reports elsewhere is not silently re-parented.
	managed, _ := reg.GetSession("sess-proj-b")
	managed.SourceSessionID = "sess-somebody-else"
	if err := reg.PutSession(managed); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-proj-b"); err == nil {
		t.Fatal("an already-managed root must not be stolen")
	}
}

func TestGovernedListsOnlyEnrolledRoots(t *testing.T) {
	root, _ := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	governed, err := root.Governed()
	if err != nil {
		t.Fatal(err)
	}
	if len(governed) != 1 || governed[0].ID != "sess-proj-a" {
		t.Fatalf("want only the enrolled root, got %+v", governed)
	}
}

// The digest leads with what needs the human and collapses the rest to counts.
func TestDigestReturnsHeldWorkAndCountsTheRest(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	parents := &ParentService{Reg: reg}
	now := time.Now().UTC()
	held := &ParentMessage{
		V: 1, ID: "pm-held", ParentSessionID: "sess-apex", ChildSessionID: "sess-child",
		HandoffID: "ho-1", Kind: "ask", State: ParentMessagePending, CreatedAt: now,
		IntendedParentSessionID: "sess-proj-a",
	}
	ruled := &ParentMessage{
		V: 1, ID: "pm-ruled", ParentSessionID: "sess-apex", ChildSessionID: "sess-child",
		HandoffID: "ho-2", Kind: "ask", State: ParentMessageReplied, Reply: "approved", CreatedAt: now,
	}
	gated := &ParentMessage{
		V: 1, ID: "pm-gated", ParentSessionID: "sess-apex", ChildSessionID: "sess-child",
		HandoffID: "ho-3", Kind: "ask", State: ParentMessageAcked, AutoHandled: true, CreatedAt: now,
	}
	for _, m := range []*ParentMessage{held, ruled, gated} {
		if err := writeParentMessage(m, true); err != nil {
			t.Fatal(err)
		}
	}

	digest, err := root.Digest(parents, 0)
	if err != nil {
		t.Fatal(err)
	}
	if digest.HeldCount != 1 || len(digest.Held) != 1 || digest.Held[0].ID != "pm-held" {
		t.Fatalf("want the pending ask surfaced in full: %+v", digest)
	}
	if digest.Ruled != 1 {
		t.Fatalf("want one autonomous ruling counted, got %d", digest.Ruled)
	}
	if digest.AutoGate != 1 {
		t.Fatalf("want one policy-gate action counted, got %d", digest.AutoGate)
	}
	if digest.FailedOver != 1 {
		t.Fatalf("want the failover counted, got %d", digest.FailedOver)
	}
}

func TestRulesPathIsProjectScopedAndRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_RULES_DIR", dir)
	got, err := RulesPath("beholder")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "beholder.md") {
		t.Fatalf("unexpected rules path %q", got)
	}
	for _, bad := range []string{"", "../etc/passwd", "a/b"} {
		if _, err := RulesPath(bad); err == nil {
			t.Fatalf("project %q must be rejected", bad)
		}
	}
}

// The apex's own workers report to it without being enrolled; unenroll must
// not detach them, or live work is silently orphaned.
func TestUnenrollLeavesTheApexOwnWorkersAlone(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	worker := &Session{
		ID: "sess-worker", HostID: "home",
		Persist:         ports.PersistHandle{Kind: "tmux", Name: "worker"},
		SourceSessionID: "sess-apex", CreatedAt: now,
	}
	if err := reg.PutSession(worker); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Unenroll("sess-worker"); err == nil {
		t.Fatal("a worker the apex spawned was never enrolled and must not be detached")
	}
	got, _ := reg.GetSession("sess-worker")
	if got.SourceSessionID != "sess-apex" {
		t.Fatalf("worker was orphaned: manager=%q", got.SourceSessionID)
	}
	governed, err := root.Governed()
	if err != nil {
		t.Fatal(err)
	}
	if len(governed) != 0 {
		t.Fatalf("an unenrolled worker must not appear as governed: %+v", governed)
	}
}

// Enrolling must never imply autonomy the deployment cannot deliver: when the
// control plane can sleep, governance pauses with it.
func TestControlPlaneDisclosesWhenGovernancePauses(t *testing.T) {
	t.Setenv("RELAY_CONTROL_PLANE_ALWAYS_ON", "")
	cp := DescribeControlPlane()
	if cp.AlwaysOn {
		t.Fatal("must not claim always-on without an explicit declaration")
	}
	if cp.Warning == "" {
		t.Fatal("a sleepable control plane must carry a warning")
	}
	if cp.Host == "" {
		t.Fatal("the control plane must name its host")
	}

	t.Setenv("RELAY_CONTROL_PLANE_ALWAYS_ON", "1")
	cp = DescribeControlPlane()
	if !cp.AlwaysOn {
		t.Fatal("an explicit declaration must be honoured")
	}
	if cp.Warning != "" {
		t.Fatalf("a declared always-on plane needs no warning, got %q", cp.Warning)
	}
}

// Swapping the apex — e.g. moving governance off a laptop pane onto an
// always-on host — must be possible. Adopt used to refuse and point at a
// command that did not exist.
func TestApexCanBeReleasedAndReplaced(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Adopt("sess-proj-a"); err == nil {
		t.Fatal("a second apex must be refused while one is designated")
	}
	if _, err := root.Release("sess-apex"); err != nil {
		t.Fatalf("the apex must be releasable: %v", err)
	}
	if _, err := root.Apex(); err == nil {
		t.Fatal("no apex should remain after release")
	}
	if _, err := root.Adopt("sess-proj-a"); err != nil {
		t.Fatalf("a new apex must be adoptable after release: %v", err)
	}
	got, _ := reg.GetSession("sess-proj-a")
	if got.Labels[ApexLabel] != "true" {
		t.Fatalf("new apex not labelled: %v", got.Labels)
	}
}

func TestReplaceApexMovesDirectChildrenAndHandoffs(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	worker := &Session{ID: "sess-worker", SourceSessionID: "sess-apex", CreatedByHandoffID: "ho-worker", CreatedAt: now}
	handoff := &Handoff{ID: "ho-worker", SessionID: worker.ID, SourceSessionID: "sess-apex", Status: StatusRunning, CreatedAt: now}
	if err := reg.PutSession(worker); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	result, err := root.Replace(context.Background(), &ParentService{Reg: reg}, "sess-apex", "sess-proj-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Reparented) != 2 {
		t.Fatalf("replacement=%+v", result)
	}
	for _, id := range []string{"sess-proj-a", "sess-worker"} {
		got, _ := reg.GetSession(id)
		if got.SourceSessionID != "sess-proj-b" {
			t.Fatalf("%s parent=%s", id, got.SourceSessionID)
		}
	}
	gotHandoff, _ := reg.GetHandoff("ho-worker")
	if gotHandoff.SourceSessionID != "sess-proj-b" {
		t.Fatalf("handoff parent=%s", gotHandoff.SourceSessionID)
	}
	old, _ := reg.GetSession("sess-apex")
	next, _ := reg.GetSession("sess-proj-b")
	if old.Labels[ApexLabel] != "" || next.Labels[ApexLabel] != "true" {
		t.Fatalf("old=%v next=%v", old.Labels, next.Labels)
	}
	if _, err := os.Stat(replacementIntentPath()); !os.IsNotExist(err) {
		t.Fatalf("replacement intent remains: %v", err)
	}
}

func TestReplaceApexRejectsSameSession(t *testing.T) {
	root, _ := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Replace(context.Background(), &ParentService{Reg: root.Reg}, "sess-apex", "sess-apex"); err == nil {
		t.Fatal("same-session replacement must be rejected")
	}
}

func TestRecoverReplacementConvergesSessionAndHandoffIndependently(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	child := &Session{ID: "sess-worker", SourceSessionID: "sess-proj-b", CreatedByHandoffID: "ho-worker", CreatedAt: now}
	handoff := &Handoff{ID: "ho-worker", SessionID: child.ID, SourceSessionID: "sess-apex", Status: StatusRunning, CreatedAt: now}
	if err := reg.PutSession(child); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	intent := ManagerReplacement{V: 1, ID: "replace-crash", OldID: "sess-apex", NewID: "sess-proj-b", Children: []string{child.ID}, Created: now.Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(intent)
	if err := os.WriteFile(replacementIntentPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.RecoverReplacement(context.Background(), &ParentService{Reg: reg}); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.GetHandoff(handoff.ID)
	if got.SourceSessionID != "sess-proj-b" {
		t.Fatalf("handoff parent=%s", got.SourceSessionID)
	}
}

func TestReplacementRecoveryUsesConvergedProjectionSnapshot(t *testing.T) {
	root, reg := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	viz := &replacementProjectionViz{fail: true}
	parents := &ParentService{Reg: reg, Viz: viz}
	result, err := root.Replace(context.Background(), parents, "sess-apex", "sess-proj-b")
	if err != nil || !result.ProjectionPending {
		t.Fatalf("replace=%+v err=%v", result, err)
	}
	raw, err := os.ReadFile(replacementIntentPath())
	if err != nil {
		t.Fatal(err)
	}
	var intent ManagerReplacement
	if json.Unmarshal(raw, &intent) != nil || !intent.AuthorityConverged || len(intent.Projections) == 0 {
		t.Fatalf("replacement not durably converged: %+v", intent)
	}
	if err := reg.DeleteSession("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	viz.fail = false
	result, err = root.RecoverReplacement(context.Background(), parents)
	if err != nil || result.ProjectionPending {
		t.Fatalf("recover=%+v err=%v", result, err)
	}
	found := false
	for _, item := range viz.items {
		if item.SessionID == "sess-proj-a" && item.ParentSessionID == "sess-proj-b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stored child projection not replayed: %+v", viz.items)
	}
	if _, err := os.Stat(replacementIntentPath()); !os.IsNotExist(err) {
		t.Fatalf("completed intent remains: %v", err)
	}
}

// Releasing while roots are attached would leave them reporting to a session
// that is no longer an apex — governed by lineage but invisible to status.
func TestReleaseRefusesWhileRootsAreEnrolled(t *testing.T) {
	root, _ := newRootTestService(t)
	if _, err := root.Adopt("sess-apex"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Enroll("sess-proj-a"); err != nil {
		t.Fatal(err)
	}
	_, err := root.Release("sess-apex")
	if err == nil {
		t.Fatal("release must refuse while a root is enrolled")
	}
	if !strings.Contains(err.Error(), "sess-proj-a") {
		t.Fatalf("the error must name what is still enrolled, got: %v", err)
	}
}
