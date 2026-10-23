package core

import (
	"context"
	"strings"
	"time"

	"github.com/dostos/relay/internal/panetext"
)

// Activity values for ReadinessReport.Activity and `session wait` (#69).
const (
	ActivityBusy    = "busy"
	ActivityIdle    = "idle"
	ActivityBlocked = "blocked"
	ActivityUnknown = "unknown"
)

// activityTailLines is how far up from the bottom a working indicator still
// counts as the live status line rather than transcript scrollback.
const activityTailLines = 8

// ActivityOf derives busy/idle from a classified pane.
func ActivityOf(state AgentState, text string) string {
	switch state {
	case AgentBlocked:
		return ActivityBlocked
	case AgentAbsent:
		return ActivityIdle
	case AgentReady:
		if len(panetext.LiveWorkingLines(text, activityTailLines)) > 0 {
			return ActivityBusy
		}
		return ActivityIdle
	default:
		return ActivityUnknown
	}
}

// WaitOptions bounds `relay session wait --idle`.
type WaitOptions struct {
	Timeout time.Duration
	// Interval between pane samples.
	Interval time.Duration
	// Settle is how long the pane must stay idle before the wait succeeds,
	// so the gap between "message submitted" and "Working…" rendering, or
	// between two tool calls, is not mistaken for the end of the turn.
	Settle time.Duration
	// StaleAfter treats a working line whose text (and timer) has not
	// changed for this long as a leftover, not a live turn.
	StaleAfter time.Duration
	// TailLines, when > 0, returns that many pane lines with the result.
	TailLines int
}

// WaitResult is the bounded answer of WaitIdle.
type WaitResult struct {
	SessionID string        `json:"session_id"`
	HostID    string        `json:"host_id"`
	Target    string        `json:"target"`
	Outcome   string        `json:"outcome"` // idle | blocked | gone | timeout | error
	Activity  string        `json:"activity"`
	State     AgentState    `json:"state"`
	Reason    string        `json:"reason"`
	Gate      *SecurityGate `json:"gate"`
	Samples   int           `json:"samples"`
	SawBusy   bool          `json:"saw_busy"`
	WaitedMS  int64         `json:"waited_ms"`
	SampledAt time.Time     `json:"sampled_at"`
	Tail      string        `json:"tail,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// Wait outcomes.
const (
	WaitIdle    = "idle"
	WaitBlocked = "blocked"
	WaitGone    = "gone"
	WaitTimeout = "timeout"
	WaitError   = "error"
)

// WaitIdle samples the target until it has been idle for Settle, stops at a
// gate, disappears, or Timeout elapses. It never types anything.
func (s *SessionService) WaitIdle(ctx context.Context, id string, o WaitOptions) (WaitResult, error) {
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.Settle < 0 {
		o.Settle = 0
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 20 * time.Second
	}
	start := time.Now()
	res := WaitResult{SessionID: id, Outcome: WaitError, Activity: ActivityUnknown}
	sess, err := s.lookup(id)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	res.HostID = sess.HostID
	res.Target = sess.HostID + "/" + sess.Persist.Name
	deadline := start.Add(o.Timeout)
	var idleSince time.Time
	var lastWorking string
	var workingSince time.Time
	lines := DefaultReadinessLines
	if o.TailLines > lines {
		lines = o.TailLines
	}
	finish := func(outcome string, rep ReadinessReport) (WaitResult, error) {
		res.Outcome = outcome
		res.Activity = rep.Activity
		res.State = rep.State
		res.Reason = rep.Reason
		res.Gate = rep.Gate
		res.SampledAt = rep.SampledAt
		res.WaitedMS = time.Since(start).Milliseconds()
		if o.TailLines > 0 && rep.text != "" {
			res.Tail = lastLines(rep.text, o.TailLines)
		}
		return res, nil
	}
	for {
		rep := s.readinessOf(ctx, sess, lines)
		res.Samples++
		now := time.Now()
		switch rep.State {
		case AgentGone:
			return finish(WaitGone, rep)
		case AgentBlocked:
			return finish(WaitBlocked, rep)
		}
		activity := rep.Activity
		if activity == ActivityBusy {
			cur := strings.Join(panetext.LiveWorkingLines(rep.text, activityTailLines), "\n")
			if cur != lastWorking {
				lastWorking, workingSince = cur, now
			} else if now.Sub(workingSince) >= o.StaleAfter {
				// A live turn's timer ticks every second; a frozen line is
				// a leftover from a turn that already ended.
				activity = ActivityIdle
				rep.Reason = "working indicator unchanged for " + o.StaleAfter.String() + "; treated as stale"
			}
		} else {
			lastWorking = ""
		}
		rep.Activity = activity
		switch activity {
		case ActivityBusy:
			res.SawBusy = true
			idleSince = time.Time{}
		case ActivityIdle:
			if idleSince.IsZero() {
				idleSince = now
			}
			if now.Sub(idleSince) >= o.Settle {
				return finish(WaitIdle, rep)
			}
		default:
			idleSince = time.Time{}
		}
		if !now.Add(o.Interval).Before(deadline) {
			r, _ := finish(WaitTimeout, rep)
			return r, nil
		}
		select {
		case <-ctx.Done():
			r, _ := finish(WaitError, rep)
			r.Error = ctx.Err().Error()
			return r, ctx.Err()
		case <-time.After(o.Interval):
		}
	}
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
