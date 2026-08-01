package core

import (
	"context"
	"os"
	"strconv"
	"time"
)

// Delivery treats "reachable" as "will decide". Those are the same thing for an
// agent manager and completely different for a human-attended cmux pane: a pane
// is alive whether or not anyone is reading it. So an escalation lands on a
// live-but-unattended manager and stops there, while an always-on apex one level
// up — perfectly able to rule on it — is never consulted.
//
// Ageing adds the missing clock, but it REPORTS staleness; it does not transfer
// the decision.
//
// An earlier version moved the envelope to the next ancestor, which looked like
// Part A's failover but is not the same thing. Failover bypasses a manager that
// CANNOT receive; there is no alternative. Taking a question from a manager that
// is merely slow strips authority from a manager that still exists, tells it
// nothing, and makes the ancestor answer its own grandchild directly — which
// breaks the one rule the whole tree rests on, that each level talks only to the
// next.
//
// So a stale question stays with its holder, and the holder's MANAGER is told
// that its own child has a stalled subtree. That is a fact about the manager's
// immediate child, which is squarely within its remit, and it leaves the
// decision where the tree says it belongs.

// DefaultEscalationMaxHold is how long one manager may sit on an unanswered
// attention envelope before its own manager is told. Long enough that a human
// reading their pane is not pre-empted mid-thought; short enough that unattended
// work does not stall for an afternoon.
const DefaultEscalationMaxHold = 15 * time.Minute

// EscalationMaxHold reads RELAY_ESCALATION_MAX_HOLD_MIN, else the default.
func EscalationMaxHold() time.Duration {
	if v := os.Getenv("RELAY_ESCALATION_MAX_HOLD_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return DefaultEscalationMaxHold
}

// StaleEscalation is an unanswered attention envelope and who holds it.
type StaleEscalation struct {
	Message *ParentMessage
	HeldFor time.Duration
}

// FindStaleEscalations lists attention envelopes that have gone unanswered past
// maxHold. Only delivered ones count: an undelivered envelope is a routing
// problem, which DeliverPending already owns.
func (p *ParentService) FindStaleEscalations(maxHold time.Duration, now time.Time) ([]StaleEscalation, error) {
	list, err := p.Reg.ListSessions()
	if err != nil {
		return nil, err
	}
	var out []StaleEscalation
	for _, sess := range list {
		msgs, err := p.ListMessages(sess.ID, true)
		if err != nil {
			continue
		}
		for _, msg := range msgs {
			if !attentionMessage(msg.Kind) || msg.State != ParentMessagePending {
				continue
			}
			if msg.DeliveredAt == nil {
				continue
			}
			// Measure from when the question was ASKED, not when it was last
			// delivered. deliverMessage stamps DeliveredAt on every successful
			// delivery, so a reconnect, a laptop wake, or a failover would reset
			// the clock — erasing the evidence in exactly the situations where a
			// stall most needs catching. The child has been waiting since it
			// asked, regardless of how many times the envelope was re-handed.
			held := now.Sub(msg.CreatedAt)
			if held >= maxHold {
				out = append(out, StaleEscalation{Message: msg, HeldFor: held})
			}
		}
	}
	return out, nil
}

// ReportStaleEscalations tells each stale holder's manager that one of its
// children has a question nobody has answered. It never moves the envelope and
// never answers on the holder's behalf; the decision stays where it was routed.
func (p *ParentService) ReportStaleEscalations(ctx context.Context, maxHold time.Duration) (int, error) {
	stale, err := p.FindStaleEscalations(maxHold, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	reported := 0
	for _, item := range stale {
		msg := item.Message
		holder, err := p.Reg.GetSession(msg.ParentSessionID)
		if err != nil || holder == nil {
			continue
		}
		chain := AncestorChain(p.Reg, holder.ID)
		if len(chain) == 0 {
			// The holder is the top of the tree; only the human is above it.
			continue
		}
		manager := chain[0]
		notice := ParentNotice{
			MessageID: msg.ID,
			HandoffID: msg.HandoffID,
			Kind:      "stalled",
			Child:     holder.ID,
			Text: "your child " + holder.ID + " has held an unanswered " + msg.Kind +
				" for " + strconv.Itoa(int(item.HeldFor.Minutes())) + "m (" + msg.ID +
				"); it still owns the decision",
			Action: "inspect",
		}
		if !stallDue(msg, item.HeldFor, maxHold, time.Now().UTC()) {
			continue
		}
		if p.notifyStalled(ctx, manager, notice) == nil {
			now := time.Now().UTC()
			msg.StallReportedAt = &now
			_ = writeParentMessage(msg, false)
			reported++
		}
	}
	return reported, nil
}

func (p *ParentService) notifyStalled(ctx context.Context, manager *Session, notice ParentNotice) error {
	attemptCtx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
	defer cancel()
	if isLocalParent(manager) && p.Notifier != nil {
		return p.Notifier.NotifyParent(attemptCtx, manager.ID, notice)
	}
	if p.Sessions != nil {
		return p.Sessions.Send(attemptCtx, manager.ID, FormatParentNotice(notice), true)
	}
	return nil
}

// stallDue decides whether a standing stall is worth re-announcing.
//
// Reporting every supervisor tick turns one stuck question into a notice twice
// a minute, forever — which trains the reader to ignore the channel and costs
// tokens for zero new information. Instead the interval doubles: told once at
// the threshold, then at 2x, 4x, 8x the wait. A stall stays visible, and gets
// louder the longer it lasts, without ever repeating itself minute to minute.
func stallDue(msg *ParentMessage, heldFor, maxHold time.Duration, now time.Time) bool {
	if msg.StallReportedAt == nil {
		return true
	}
	sinceReport := now.Sub(*msg.StallReportedAt)
	// Next report is due one full "held so far" later, i.e. geometric.
	next := heldFor / 2
	if next < maxHold {
		next = maxHold
	}
	return sinceReport >= next
}
