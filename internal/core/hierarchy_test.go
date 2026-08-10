package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

// A channel agent is a managing PARENT, not a second root: it may resume,
// restart and direct its own children, while governance (holds, approvals)
// stays with the top root. Structurally that is one thing relay could not
// express — a manager creating another manager underneath itself — and one
// thing it could not repair — a session that was adopted from tmux and so has
// no handoff to move.

func TestRegisterHeadlessUnderPlacesAChildManagerInsideItsCallersSubtree(t *testing.T) {
	service, _, reg := newParentTestService(t)
	root, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "apex", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	channel, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{
		Headless: true, Name: "chan-gazer", TTL: time.Hour, Under: root.ID,
	})
	if err != nil || !created {
		t.Fatalf("register under root: created=%v err=%v", created, err)
	}
	if channel.SourceSessionID != root.ID {
		t.Fatalf("channel parent lineage = %q, want %q", channel.SourceSessionID, root.ID)
	}
	if !IsHeadlessParent(channel) {
		t.Fatalf("a child manager is still a headless parent: %+v", channel)
	}
	if !sessionAncestor(reg, root.ID, channel.ID) {
		t.Fatal("root must be an ancestor of the manager it created")
	}
	// The child manager is a manager in its own right, with its own inbox
	// identity — that is what lets it act as itself rather than as the root.
	if _, err := EnsureHeadlessBridgeIdentity(channel.ID); err != nil {
		t.Fatalf("child manager identity: %v", err)
	}
}

func TestRegisterHeadlessUnderIsIdempotentAndRefusesSilentRehoming(t *testing.T) {
	service, _, _ := newParentTestService(t)
	root, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "apex", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-gazer", Under: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	// A container start re-runs the seed hook. It must converge on the same
	// identity, not pile up a manager per boot.
	again, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-gazer", Under: root.ID})
	if err != nil || created || again.ID != first.ID || again.SourceSessionID != root.ID {
		t.Fatalf("re-register = %+v created=%v err=%v", again, created, err)
	}
	// Re-registering without --under must not silently orphan a manager that
	// already reports to somebody.
	kept, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-gazer"})
	if err != nil || kept.SourceSessionID != root.ID {
		t.Fatalf("bare re-register changed lineage: %+v err=%v", kept, err)
	}
	// Re-registering under a DIFFERENT manager is a move, and a move is never
	// a side effect of registration.
	rival, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "rival", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-gazer", Under: rival.ID}); err == nil {
		t.Fatal("re-registering under a different manager must be refused, not applied")
	}
}

// Registration is authorized on --under but acts on --name, and a name is the
// documented, guessable identity of every headless service here. If convergence
// could also SET lineage, `--name <someone else's root> --under <self>` would
// pull that root — and its whole tree, and its bridge token — into the caller's
// subtree. So convergence never changes lineage at all, including from "no
// manager" to "some manager".
func TestRegisterHeadlessNeverAdoptsAnExistingRootByName(t *testing.T) {
	service, _, reg := newParentTestService(t)
	victim, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "hermes", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	attacker, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "squatter", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sess, created, err := service.RegisterLocal(context.Background(), RegisterParentOpts{
		Headless: true, Name: "hermes", Under: attacker.ID,
	})
	if err == nil {
		t.Fatalf("name squatting must be refused; got %+v created=%v", sess, created)
	}
	if !strings.Contains(err.Error(), "parent adopt") {
		t.Fatalf("refusal must name the explicit move: %v", err)
	}
	stored, err := reg.GetSession(victim.ID)
	if err != nil || stored.SourceSessionID != "" {
		t.Fatalf("victim lineage was changed: %+v err=%v", stored, err)
	}
	if sessionAncestor(reg, attacker.ID, victim.ID) {
		t.Fatal("the caller became an ancestor of a root it has no authority over")
	}
}

func TestRegisterHeadlessUnderRefusesUnknownParentsSelfAndCycles(t *testing.T) {
	service, _, _ := newParentTestService(t)
	root, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "apex", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-x", Under: "sess-nope"}); err == nil {
		t.Fatal("registering under an unknown manager must fail")
	}
	child, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-y", Under: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Re-registering the root under its own descendant would invert the tree.
	if _, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "apex", Under: child.ID}); err == nil {
		t.Fatal("registering a manager under its own descendant must fail")
	}
	if _, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-y", Under: child.ID}); err == nil {
		t.Fatal("a manager cannot manage itself")
	}
	// --under is a headless-registration concept; the pane path has no story
	// for it yet, so it must refuse rather than quietly ignore the flag.
	if _, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Surface: "surface:9", Name: "pane", Under: root.ID}); err == nil {
		t.Fatal("--under on a pane registration must be refused, not ignored")
	}
}

func adoptionFixture(t *testing.T) (*ParentService, *Registry, *Session, *Session, *Session) {
	t.Helper()
	service, _, reg := newParentTestService(t)
	root, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "apex", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	channel, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-gazer", TTL: time.Hour, Under: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	orphan := &Session{ID: "sess-orphan", HostID: "hamburg", Persist: ports.PersistHandle{Kind: "tmux", Name: "gazer"},
		Labels: map[string]string{"adopted": "existing"}, CreatedAt: now, UpdatedAt: now}
	if err := reg.PutSession(orphan); err != nil {
		t.Fatal(err)
	}
	return service, reg, root, channel, orphan
}

// A session adopted from a running tmux has no handoff, so there was nothing
// for `parent link|move` to take. Lineage has to be expressible on the session.
func TestAdoptSessionGivesAnOrphanTmuxSessionAManager(t *testing.T) {
	service, reg, _, channel, orphan := adoptionFixture(t)
	adopted, oldParent, err := service.AdoptSession(channel.ID, orphan.ID, "", false)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if oldParent != "" || adopted.SourceSessionID != channel.ID {
		t.Fatalf("adopted = %+v old=%q", adopted, oldParent)
	}
	stored, err := reg.GetSession(orphan.ID)
	if err != nil || stored.SourceSessionID != channel.ID || stored.SourcePersistName != channel.Persist.Name {
		t.Fatalf("stored lineage = %+v err=%v", stored, err)
	}
	// Idempotent: re-running a converging hook must not fail or churn.
	if _, _, err := service.AdoptSession(channel.ID, orphan.ID, "", false); err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
}

func TestAdoptSessionRefusesAmbiguityRatherThanGuessing(t *testing.T) {
	service, reg, root, channel, orphan := adoptionFixture(t)
	other, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-engram", TTL: time.Hour, Under: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.AdoptSession(channel.ID, orphan.ID, "", false); err != nil {
		t.Fatal(err)
	}
	// A session that already has a manager is a MOVE, and a move must be
	// stated. Silently rehoming is exactly the class of bug that reads as
	// success and leaves the old manager still believing it owns the child.
	if _, _, err := service.AdoptSession(other.ID, orphan.ID, "", false); err == nil {
		t.Fatal("moving a managed session without --from must be refused")
	}
	if _, _, err := service.AdoptSession(other.ID, orphan.ID, root.ID, true); err == nil {
		t.Fatal("--from naming the wrong manager must be refused")
	}
	moved, oldParent, err := service.AdoptSession(other.ID, orphan.ID, channel.ID, true)
	if err != nil || oldParent != channel.ID || moved.SourceSessionID != other.ID {
		t.Fatalf("confirmed move = %+v old=%q err=%v", moved, oldParent, err)
	}
	// --from on a session that has no manager is a stale assumption, not a
	// no-op: the caller believes something about the tree that is not true.
	stray := &Session{ID: "sess-stray", HostID: "madrid", Persist: ports.PersistHandle{Kind: "tmux", Name: "stray"}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := reg.PutSession(stray); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.AdoptSession(channel.ID, stray.ID, root.ID, true); err == nil {
		t.Fatal("--from on an unmanaged session must be refused")
	}
	if _, _, err := service.AdoptSession(channel.ID, channel.ID, "", false); err == nil {
		t.Fatal("a manager cannot adopt itself")
	}
	if _, _, err := service.AdoptSession(channel.ID, root.ID, "", false); err == nil {
		t.Fatal("adopting an ancestor would invert the tree")
	}
}

// When a live handoff owns the child's lineage, moving only the session record
// would leave the handoff's event stream still escalating to the old manager —
// delivered, answered by nobody, and invisible. Refuse and name the verb that
// moves both.
func TestAdoptSessionRefusesWhenALiveHandoffOwnsTheChild(t *testing.T) {
	service, reg, _, channel, _ := adoptionFixture(t)
	now := time.Now().UTC()
	worker := &Session{ID: "sess-worker", HostID: "madrid", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}, CreatedAt: now, UpdatedAt: now}
	if err := reg.PutSession(worker); err != nil {
		t.Fatal(err)
	}
	ho := &Handoff{ID: "ho-live", SessionID: worker.ID, HostID: "madrid", Kind: KindAgent, Status: StatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	_, _, err := service.AdoptSession(channel.ID, worker.ID, "", false)
	if err == nil || !strings.Contains(err.Error(), ho.ID) {
		t.Fatalf("expected a refusal naming %s, got %v", ho.ID, err)
	}
	if !strings.Contains(err.Error(), "parent move") {
		t.Fatalf("refusal must name the verb that moves both: %v", err)
	}
}

// The whole point of scope A+B is that a channel parent may act on ITS OWN
// children and nowhere else. Creation, enumeration and adoption must all obey
// the same confinement the existing verbs already obey.
func TestChannelParentAuthorityIsSubtreeConfined(t *testing.T) {
	service, reg, root, channel, orphan := adoptionFixture(t)
	rival, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-engram", TTL: time.Hour, Under: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.AdoptSession(channel.ID, orphan.ID, "", false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		actor *Session
		args  []string
		want  bool
	}{
		{"root creates a manager under itself", root, []string{"parent", "register", "--headless", "--name", "chan-x", "--under", root.ID}, true},
		{"root creates a manager under its own channel parent", root, []string{"parent", "register", "--headless", "--name", "chan-x", "--under", channel.ID}, true},
		{"channel parent creates a manager under itself", channel, []string{"parent", "register", "--headless", "--name", "sub", "--under", channel.ID}, true},
		{"channel parent may not create a manager under the root", channel, []string{"parent", "register", "--headless", "--name", "sub", "--under", root.ID}, false},
		{"channel parent may not create a manager under a sibling", channel, []string{"parent", "register", "--headless", "--name", "sub", "--under", rival.ID}, false},
		{"registration without --under would be a new root", channel, []string{"parent", "register", "--headless", "--name", "sub"}, false},

		{"root adopts into its own subtree", root, []string{"parent", "adopt", channel.ID, orphan.ID, "--from", channel.ID}, true},
		{"channel parent keeps its own child", channel, []string{"parent", "adopt", channel.ID, orphan.ID, "--from", channel.ID}, true},
		{"sibling may not steal a managed session", rival, []string{"parent", "adopt", rival.ID, orphan.ID, "--from", channel.ID}, false},
		{"sibling may not adopt into another subtree", rival, []string{"parent", "adopt", channel.ID, orphan.ID}, false},

		{"manager enumerates its own subtree", root, []string{"parent", "list", "--under", root.ID}, true},
		{"channel parent enumerates itself", channel, []string{"parent", "list", "--under", channel.ID}, true},
		{"channel parent may not enumerate the root", channel, []string{"parent", "list", "--under", root.ID}, false},
		{"global enumeration stays refused", root, []string{"parent", "list"}, false},

		// A repeated flag is where the policy (first occurrence) and an
		// ordinary parse loop (last occurrence) disagree. Both sides refuse it,
		// so no argv can be authorized against one value and executed with
		// another — `--under <self> --under ""` would otherwise mint a ROOT.
		{"repeated --under on register", root, []string{"parent", "register", "--headless", "--name", "x", "--under", root.ID, "--under", ""}, false},
		{"repeated --under naming a stranger", channel, []string{"parent", "register", "--headless", "--name", "x", "--under", channel.ID, "--under", rival.ID}, false},
		{"blank --under on register", root, []string{"parent", "register", "--headless", "--name", "x", "--under", "  "}, false},
		{"repeated --under on list", channel, []string{"parent", "list", "--under", channel.ID, "--under", root.ID}, false},
		{"blank --under on list", root, []string{"parent", "list", "--under", ""}, false},
	} {
		allowed, reason := authorizeOperation(reg, tc.actor, tc.args)
		if allowed != tc.want {
			t.Fatalf("%s: allowed=%v want=%v (%s)", tc.name, allowed, tc.want, reason)
		}
	}
	_ = service
}

// A confirmed move takes the child's UNANSWERED escalations with it, the way
// ReparentChild already does for a handoff. An ask left in the old manager's
// inbox is addressed to somebody who no longer owns the child: delivered,
// answered by nobody, invisible. Answered history stays where it was handled.
func TestAdoptSessionMovesPendingEscalationsWithTheChild(t *testing.T) {
	service, reg, root, channel, orphan := adoptionFixture(t)
	other, _, err := service.RegisterLocal(context.Background(), RegisterParentOpts{Headless: true, Name: "chan-engram", TTL: time.Hour, Under: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.AdoptSession(channel.ID, orphan.ID, "", false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pending := &ParentMessage{V: 1, ID: "pm-pending", ParentSessionID: channel.ID, ChildSessionID: orphan.ID,
		HandoffID: "ho-done", Kind: "ask", Text: "which branch?", State: ParentMessagePending, CreatedAt: now}
	if err := writeParentMessage(pending, true); err != nil {
		t.Fatal(err)
	}
	answered := &ParentMessage{V: 1, ID: "pm-answered", ParentSessionID: channel.ID, ChildSessionID: orphan.ID,
		HandoffID: "ho-done", Kind: "ask", Text: "old", State: ParentMessageReplied, CreatedAt: now}
	if err := writeParentMessage(answered, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.AdoptSession(other.ID, orphan.ID, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	moved, err := service.FindMessage(pending.ID)
	if err != nil || moved.ParentSessionID != other.ID {
		t.Fatalf("pending envelope did not follow the child: %+v err=%v", moved, err)
	}
	stayed, err := service.FindMessage(answered.ID)
	if err != nil || stayed.ParentSessionID != channel.ID {
		t.Fatalf("answered history must stay with the manager that handled it: %+v err=%v", stayed, err)
	}
	_ = reg
}

// Most sessions in a registry have no manager, so "any manager may claim an
// unmanaged session" is a reach across the whole fleet — and it does not stop
// at reading: once a session is in the caller's lineage, authoritySessionTarget
// grants exec on it, i.e. arbitrary commands on that session's host. Which
// session belongs to which manager is a declared fact applied by the writer
// that owns the registry; over the bridge only the governing apex may claim
// one, and with no apex designated that fails closed.
func TestClaimingAnUnmanagedSessionIsNotAnOrdinaryManagerAuthority(t *testing.T) {
	service, reg, root, channel, orphan := adoptionFixture(t)

	if allowed, reason := authorizeOperation(reg, channel, []string{"parent", "adopt", channel.ID, orphan.ID}); allowed {
		t.Fatalf("an ordinary manager must not claim an unmanaged session: %s", reason)
	}
	if allowed, reason := authorizeOperation(reg, root, []string{"parent", "adopt", channel.ID, orphan.ID}); allowed {
		t.Fatalf("a root that is not the designated apex must not claim either: %s", reason)
	}
	// The escalation that makes it matter: a claim would hand the caller exec.
	if allowed, _ := authorizeOperation(reg, channel, []string{"session", "exec", orphan.ID}); allowed {
		t.Fatal("exec on an unclaimed session must be refused")
	}

	// The declared path — the writer that owns the registry — still applies it.
	if _, _, err := service.AdoptSession(channel.ID, orphan.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if allowed, reason := authorizeOperation(reg, channel, []string{"session", "exec", orphan.ID}); !allowed {
		t.Fatalf("a manager may act on its own child: %s", reason)
	}

	// The governing apex is the top of the tree by designation, with no
	// ancestor to be confined by, so it is the one caller that may claim.
	stray := &Session{ID: "sess-stray", HostID: "madrid", Persist: ports.PersistHandle{Kind: "tmux", Name: "stray"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := reg.PutSession(stray); err != nil {
		t.Fatal(err)
	}
	if allowed, _ := authorizeOperation(reg, root, []string{"parent", "adopt", channel.ID, stray.ID}); allowed {
		t.Fatal("claim must stay refused while no apex is designated")
	}
	if _, err := (&RootService{Reg: reg}).Adopt(root.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, reason := authorizeOperation(reg, root, []string{"parent", "adopt", channel.ID, stray.ID}); !allowed {
		t.Fatalf("the governing apex may claim an unmanaged session: %s", reason)
	}
	if allowed, _ := authorizeOperation(reg, channel, []string{"parent", "adopt", channel.ID, stray.ID}); allowed {
		t.Fatal("designating an apex must not widen anybody else")
	}
}
