package core

import (
	"context"
	"testing"
	"time"
)

func agedMessage(t *testing.T, id, holder, handoff string, deliveredAgo time.Duration, state ParentMessageState, kind string) *ParentMessage {
	t.Helper()
	now := time.Now().UTC()
	delivered := now.Add(-deliveredAgo)
	msg := &ParentMessage{
		V: 1, ID: id, ParentSessionID: holder, ChildSessionID: "sess-child",
		HandoffID: handoff, Kind: kind, State: state,
		CreatedAt: delivered, DeliveredAt: &delivered,
	}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestStaleEscalationsAreOnlyUnansweredAttention(t *testing.T) {
	service, _, reg := newParentTestService(t)
	_, manager, _, _ := failoverTree(t, reg)
	now := time.Now().UTC()

	agedMessage(t, "pm-old-ask", manager.ID, "ho-worker", 40*time.Minute, ParentMessagePending, "ask")
	agedMessage(t, "pm-old-answered", manager.ID, "ho-worker", 40*time.Minute, ParentMessageReplied, "ask")
	agedMessage(t, "pm-old-receipt", manager.ID, "ho-worker", 40*time.Minute, ParentMessagePending, "result")
	agedMessage(t, "pm-fresh-ask", manager.ID, "ho-worker", 2*time.Minute, ParentMessagePending, "ask")

	stale, err := service.FindStaleEscalations(15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		ids := []string{}
		for _, s := range stale {
			ids = append(ids, s.Message.ID)
		}
		t.Fatalf("only the old unanswered ask is stale, got %v", ids)
	}
	if stale[0].Message.ID != "pm-old-ask" {
		t.Fatalf("wrong message: %s", stale[0].Message.ID)
	}
}

// The manager keeps its authority. Taking a slow manager's decision away would
// make an ancestor answer its own grandchild, which breaks the one rule the tree
// rests on: each level talks only to the next.
func TestStaleEscalationStaysWithItsHolder(t *testing.T) {
	service, _, reg := newParentTestService(t)
	_, manager, _, ho := failoverTree(t, reg)
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	agedMessage(t, "pm-stuck", manager.ID, ho.ID, 40*time.Minute, ParentMessagePending, "ask")

	if _, err := service.ReportStaleEscalations(context.Background(), 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	held, err := service.ListMessages(manager.ID, true)
	if err != nil || len(held) != 1 {
		t.Fatalf("the holder must keep the decision: %d (%v)", len(held), err)
	}
	if held[0].SkippedSessionIDs != nil {
		t.Fatalf("nothing was skipped; this is a report, not a failover: %+v", held[0].SkippedSessionIDs)
	}
}

// The holder's own manager is told — a fact about its immediate child, which is
// within its remit — so a stall is visible without anyone being bypassed.
func TestStaleEscalationNotifiesTheHoldersManager(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	_, manager, _, ho := failoverTree(t, reg)
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	agedMessage(t, "pm-stuck2", manager.ID, ho.ID, 40*time.Minute, ParentMessagePending, "ask")

	n, err := service.ReportStaleEscalations(context.Background(), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want one report, got %d", n)
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("the holder's manager must be told, got %d notices", len(notifier.notices))
	}
	if notifier.notices[0].Child != manager.ID {
		t.Fatalf("the report must name the manager's own child, got %s", notifier.notices[0].Child)
	}
}

func TestFreshEscalationsAreLeftAlone(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	_, manager, _, ho := failoverTree(t, reg)
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	agedMessage(t, "pm-fresh2", manager.ID, ho.ID, 1*time.Minute, ParentMessagePending, "ask")

	n, err := service.ReportStaleEscalations(context.Background(), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(notifier.notices) != 0 {
		t.Fatalf("a fresh ask must not be reported, n=%d notices=%d", n, len(notifier.notices))
	}
}

// A standing stall must not be re-announced every tick. Reporting twice a
// minute forever trains the reader to ignore the channel and costs tokens for
// zero new information.
func TestStallIsNotReAnnouncedEveryTick(t *testing.T) {
	service, notifier, reg := newParentTestService(t)
	_, manager, _, ho := failoverTree(t, reg)
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	agedMessage(t, "pm-standing", manager.ID, ho.ID, 40*time.Minute, ParentMessagePending, "ask")

	for i := 0; i < 5; i++ {
		if _, err := service.ReportStaleEscalations(context.Background(), 15*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if len(notifier.notices) != 1 {
		t.Fatalf("five ticks must produce one notice, got %d", len(notifier.notices))
	}
}

// It must still get louder as it ages, so a long stall is not forgotten.
func TestStallIsReAnnouncedOnceItHasAgedFurther(t *testing.T) {
	now := time.Now().UTC()
	long := now.Add(-30 * time.Minute)
	msg := &ParentMessage{ID: "pm-x", StallReportedAt: &long}
	// Held 2h, last told 30m ago: next report is due at held/2 = 1h, so not yet.
	if stallDue(msg, 2*time.Hour, 15*time.Minute, now) {
		t.Fatal("must not re-announce before the interval doubles")
	}
	// Held 40m, last told 30m ago: due at held/2 = 20m, so now.
	if !stallDue(msg, 40*time.Minute, 15*time.Minute, now) {
		t.Fatal("must re-announce once the interval has passed")
	}
	// Never reported: always due.
	if !stallDue(&ParentMessage{ID: "pm-y"}, 20*time.Minute, 15*time.Minute, now) {
		t.Fatal("a first stall must always be reported")
	}
}

// A re-delivered envelope must not look brand new. deliverMessage stamps
// DeliveredAt on every delivery, so measuring from it would let a reconnect or
// a laptop wake erase a long-standing stall — exactly when detection matters.
func TestRedeliveryDoesNotResetTheStallClock(t *testing.T) {
	service, _, reg := newParentTestService(t)
	_, manager, _, ho := failoverTree(t, reg)
	if err := reg.PutHandoff(ho); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	asked := now.Add(-3 * time.Hour)      // the child asked 3h ago
	redelivered := now.Add(-2 * time.Minute) // but it was re-handed 2m ago
	msg := &ParentMessage{
		V: 1, ID: "pm-redelivered", ParentSessionID: manager.ID,
		ChildSessionID: "sess-child", HandoffID: ho.ID, Kind: "ask",
		State: ParentMessagePending, CreatedAt: asked, DeliveredAt: &redelivered,
	}
	if err := writeParentMessage(msg, true); err != nil {
		t.Fatal(err)
	}

	stale, err := service.FindStaleEscalations(15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("a 3h-old question re-delivered 2m ago is still stalled, got %d", len(stale))
	}
	if stale[0].HeldFor < 2*time.Hour {
		t.Fatalf("held time must reflect when it was asked, got %s", stale[0].HeldFor)
	}
}
