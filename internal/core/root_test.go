package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

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
