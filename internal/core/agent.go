package core

import (
	"context"
	"fmt"
	"time"
)

// AgentResponse is the token-efficient JSON contract for `relay agent`.
// Orchestrators follow Next/Argv — no skill rediscovery, no event poll loops.
type AgentResponse struct {
	OK        bool           `json:"ok"`
	V         int            `json:"v"`
	HandoffID string         `json:"handoff_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	HostID    string         `json:"host_id,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	Status    string         `json:"status,omitempty"`
	Goal      string         `json:"goal,omitempty"`
	LastSeq   int64          `json:"last_seq"`
	Event     *Event         `json:"event,omitempty"`
	TimedOut  bool           `json:"timed_out,omitempty"`
	Text      string         `json:"text,omitempty"`
	Next      string         `json:"next"` // wait|send|capture|done|escalate|null
	Argv      []string       `json:"argv,omitempty"`
	Hint      string         `json:"hint,omitempty"`
	Error     string         `json:"error,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// DecideNext picks the single next verb after an event or timeout.
// Pure policy — unit-tested without SSH.
func DecideNext(kind HandoffKind, evKind string, timedOut bool) (next string, hint string) {
	if timedOut {
		return "wait", "no actionable event before timeout; call wait again on a new turn (do not spin)"
	}
	switch evKind {
	case "exit":
		return "done", "remote exited; finalize with done"
	case "needs_input":
		if kind == KindJob {
			return "escalate", "job needs_input — do not inject; escalate to human"
		}
		return "send", "agent needs input; send a reply or escalate"
	case "ask":
		// Explicit, declared question from the agent (meta.q/text) — no idle
		// guessing. Always actionable, even for jobs (a job that explicitly
		// asks is legitimately blocked on input).
		return "send", "agent asked a question (see text); send an answer or escalate"
	case "note", "progress":
		return "wait", "agent posted a note (see text); informational — keep waiting"
	case "result":
		return "wait", "agent posted a result (see text/meta); wait for exit to finalize"
	case "idle":
		if kind == KindJob {
			return "wait", "job idle is informational — do not send; wait for exit"
		}
		return "send", "agent idle; capture if unsure, then send or escalate"
	case "started":
		return "wait", "started; wait for idle/exit"
	default:
		return "wait", "continue waiting"
	}
}

// eventText lifts a human-readable string out of an event's meta, for the
// explicit-signaling kinds (ask/note/progress/result). Falls back to "".
func eventText(ev *Event) string {
	if ev == nil || ev.Meta == nil {
		return ""
	}
	for _, k := range []string{"text", "q", "question", "msg", "note"} {
		if v, ok := ev.Meta[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func argvFor(next, handoffID, sessionID string) []string {
	switch next {
	case "wait":
		return []string{"relay", "agent", "wait", "--handoff", handoffID}
	case "send":
		return []string{"relay", "agent", "send", "--handoff", handoffID, "--", "<text>"}
	case "capture":
		return []string{"relay", "agent", "capture", "--handoff", handoffID}
	case "done":
		return []string{"relay", "agent", "done", "--handoff", handoffID, "--outcome", "done"}
	case "escalate":
		return []string{"relay", "agent", "capture", "--handoff", handoffID}
	case "status":
		return []string{"relay", "agent", "status", "--handoff", handoffID}
	default:
		_ = sessionID
		return nil
	}
}

func (h *HandoffService) agentBase(ho *Handoff) AgentResponse {
	return AgentResponse{
		OK:        true,
		V:         1,
		HandoffID: ho.ID,
		SessionID: ho.SessionID,
		HostID:    ho.HostID,
		Kind:      string(ho.Kind),
		Status:    string(ho.Status),
		Goal:      ho.Goal,
		LastSeq:   ho.LastSeq,
	}
}

// AgentStart launches a handoff and returns next=wait (never suggests tail -f loops).
func (h *HandoffService) AgentStart(ctx context.Context, opts HandoffOpts) (*AgentResponse, error) {
	b, ho, err := h.Launch(ctx, opts)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error(), Next: ""}, err
	}
	next, hint := "wait", "block on wait until idle/exit (no poll loop)"
	resp := h.agentBase(ho)
	resp.Next = next
	resp.Hint = hint
	resp.Argv = argvFor(next, ho.ID, ho.SessionID)
	if b != nil {
		resp.Extra = map[string]any{"pane": b.Pane, "events": b.Events}
	}
	return &resp, nil
}

// AgentStatus is a one-shot snapshot with a suggested next verb.
func (h *HandoffService) AgentStatus(ctx context.Context, handoffID string) (*AgentResponse, error) {
	_ = ctx
	ho, err := h.Reg.GetHandoff(handoffID)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error()}, err
	}
	resp := h.agentBase(ho)
	switch {
	case ho.Outcome != "" || ho.Status == StatusDone || ho.Status == StatusFailed || ho.Status == StatusAbandoned:
		resp.Next = "null"
		resp.Hint = "already finalized"
		resp.Argv = nil
	case ho.Status == StatusNeedsInput && ho.Kind == KindAgent:
		resp.Next = "send"
		resp.Hint = "handoff marked needs_input"
		resp.Argv = argvFor("send", ho.ID, ho.SessionID)
	default:
		resp.Next = "wait"
		resp.Hint = "call wait (blocking) for the next actionable event"
		resp.Argv = append(argvFor("wait", ho.ID, ho.SessionID), "--from", fmt.Sprintf("%d", ho.LastSeq))
	}
	return &resp, nil
}

// AgentWait blocks until an actionable event or timeout. One-shot — no client loop.
// Actionable: exit, needs_input; idle only for agent handoffs (job idle is skipped).
func (h *HandoffService) AgentWait(ctx context.Context, handoffID string, fromSeq int64, timeout time.Duration) (*AgentResponse, error) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ho, err := h.Reg.GetHandoff(handoffID)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error()}, err
	}
	sess, err := h.Sessions.Get(ho.SessionID)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error()}, err
	}
	if h.Coord == nil {
		return &AgentResponse{OK: false, V: 1, Error: "coord adapter not configured"}, fmt.Errorf("coord adapter not configured")
	}
	t, err := h.NewTransport(ho.HostID)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error()}, err
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var got *Event
	// Shared subscribe/decode loop (see streamEvents). fn declares the agent
	// status transitions and stops at the first actionable event.
	subErr := streamEvents(wctx, h.Coord, t, sess.Persist.Name, fromSeq, true, func(ev Event) bool {
		ho.LastSeq = ev.Seq
		switch ev.Kind {
		case "needs_input", "ask":
			ho.Status = StatusNeedsInput
		case "idle":
			if ho.Kind == KindAgent {
				ho.Status = StatusNeedsInput
			}
		case "started":
			ho.Status = StatusRunning
		}
		_ = h.Reg.PutHandoff(ho)

		// Explicit-signaling kinds are actionable regardless of handoff kind:
		// the agent declared its state instead of us inferring it from silence.
		actionable := ev.Kind == "exit" || ev.Kind == "needs_input" ||
			ev.Kind == "ask" || ev.Kind == "note" || ev.Kind == "progress" || ev.Kind == "result" ||
			(ev.Kind == "idle" && ho.Kind == KindAgent)
		if !actionable {
			return true // keep waiting
		}
		cp := ev
		got = &cp
		cancel() // stop subscribe
		return false
	})

	// reload for latest status
	if latest, err := h.Reg.GetHandoff(handoffID); err == nil {
		ho = latest
	}
	resp := h.agentBase(ho)
	if got != nil {
		resp.Event = got
		resp.LastSeq = got.Seq
		resp.Text = eventText(got) // surface declared ask/note/result text
		next, hint := DecideNext(ho.Kind, got.Kind, false)
		resp.Next = next
		resp.Hint = hint
		resp.Argv = argvFor(next, ho.ID, ho.SessionID)
		if next == "wait" {
			resp.Argv = append(resp.Argv, "--from", fmt.Sprintf("%d", got.Seq))
		}
		return &resp, nil
	}

	timedOut := wctx.Err() == context.DeadlineExceeded
	if timedOut || subErr != nil {
		next, hint := DecideNext(ho.Kind, "", timedOut)
		resp.TimedOut = timedOut
		resp.Next = next
		resp.Hint = hint
		resp.Argv = append(argvFor("wait", ho.ID, ho.SessionID), "--from", fmt.Sprintf("%d", ho.LastSeq))
		if !timedOut && subErr != nil && wctx.Err() == nil {
			resp.OK = false
			resp.Error = subErr.Error()
			return &resp, subErr
		}
		return &resp, nil
	}
	resp.Next = "wait"
	resp.Hint = "stream ended without actionable event"
	resp.Argv = append(argvFor("wait", ho.ID, ho.SessionID), "--from", fmt.Sprintf("%d", ho.LastSeq))
	return &resp, nil
}

// AgentSend injects text (agent handoffs only).
func (h *HandoffService) AgentSend(ctx context.Context, handoffID, text string) (*AgentResponse, error) {
	ho, err := h.Reg.GetHandoff(handoffID)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error()}, err
	}
	if ho.Kind == KindJob {
		resp := h.agentBase(ho)
		resp.OK = false
		resp.Error = "refuse send on job handoff"
		resp.Next = "wait"
		resp.Hint = "jobs are exit-driven; do not inject on idle"
		resp.Argv = append(argvFor("wait", ho.ID, ho.SessionID), "--from", fmt.Sprintf("%d", ho.LastSeq))
		return &resp, fmt.Errorf("refuse send on job handoff")
	}
	if err := h.Sessions.Send(ctx, ho.SessionID, text, true); err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error(), HandoffID: ho.ID}, err
	}
	resp := h.agentBase(ho)
	resp.Next = "wait"
	resp.Hint = "injected; wait for next idle/exit"
	resp.Argv = append(argvFor("wait", ho.ID, ho.SessionID), "--from", fmt.Sprintf("%d", ho.LastSeq))
	return &resp, nil
}

// AgentCapture snapshots the pane.
func (h *HandoffService) AgentCapture(ctx context.Context, handoffID string, lines int) (*AgentResponse, error) {
	if lines <= 0 {
		lines = 80
	}
	ho, err := h.Reg.GetHandoff(handoffID)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error()}, err
	}
	text, err := h.Sessions.Capture(ctx, ho.SessionID, lines)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error(), HandoffID: ho.ID}, err
	}
	resp := h.agentBase(ho)
	resp.Text = text
	if ho.Kind == KindAgent && (ho.Status == StatusNeedsInput || ho.Status == StatusRunning) {
		resp.Next = "send"
		resp.Hint = "review text; send a reply or escalate to human"
		resp.Argv = argvFor("send", ho.ID, ho.SessionID)
	} else {
		resp.Next = "wait"
		resp.Hint = "snapshot only; wait for actionable event"
		resp.Argv = append(argvFor("wait", ho.ID, ho.SessionID), "--from", fmt.Sprintf("%d", ho.LastSeq))
	}
	return &resp, nil
}

// AgentDone finalizes and optionally closes viz.
func (h *HandoffService) AgentDone(ctx context.Context, handoffID string, outcome FinalizeOutcome, keepSession, closeViz bool) (*AgentResponse, error) {
	ho, err := h.Finalize(ctx, handoffID, outcome, keepSession)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error()}, err
	}
	if closeViz && h.Viz != nil {
		_ = h.Viz.Close(ctx, ho.SessionID)
	}
	resp := h.agentBase(ho)
	resp.Next = "null"
	resp.Hint = "terminal — stop"
	resp.Argv = nil
	resp.Extra = map[string]any{"outcome": ho.Outcome, "exit_code": ho.ExitCode}
	return &resp, nil
}
