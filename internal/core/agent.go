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
	LastSeq   int64          `json:"last_seq,omitempty"`
	Event     *AgentEvent    `json:"event,omitempty"`
	TimedOut  bool           `json:"timed_out,omitempty"`
	Text      string         `json:"text,omitempty"`
	Next      string         `json:"next,omitempty"` // wait|send|capture|done|escalate|null
	Argv      []string       `json:"argv,omitempty"`
	Error     string         `json:"error,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// AgentEvent is the event projection an orchestrator needs for one decision.
// The durable event stream retains timestamps, session names, and full meta;
// the turn-level response avoids repeating those static fields and lifts text
// out of meta exactly once.
type AgentEvent struct {
	Seq  int64          `json:"seq"`
	Kind string         `json:"kind"`
	Text string         `json:"text,omitempty"`
	Meta map[string]any `json:"meta,omitempty"`
}

// DecideNext picks the single next verb after an event or timeout.
// Pure policy — unit-tested without SSH.
func DecideNext(kind HandoffKind, evKind string, timedOut bool) string {
	if timedOut {
		return "wait"
	}
	switch evKind {
	case "exit":
		return "done"
	case "needs_input", "permission_required":
		if kind == KindJob {
			return "escalate"
		}
		return "send"
	case "ask":
		// Explicit, declared question from the agent (meta.q/text) — no idle
		// guessing. Always actionable, even for jobs (a job that explicitly
		// asks is legitimately blocked on input).
		return "send"
	case "note", "progress":
		return "wait"
	case "result":
		return "wait"
	case "idle":
		if kind == KindJob {
			return "wait"
		}
		return "send"
	case "started":
		return "wait"
	default:
		return "wait"
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

func compactAgentEvent(ev *Event) *AgentEvent {
	if ev == nil {
		return nil
	}
	meta := make(map[string]any, len(ev.Meta))
	for key, value := range ev.Meta {
		switch key {
		case "text", "q", "question", "msg", "note":
			continue
		default:
			meta[key] = value
		}
	}
	if len(meta) == 0 {
		meta = nil
	}
	return &AgentEvent{Seq: ev.Seq, Kind: ev.Kind, Text: eventText(ev), Meta: meta}
}

func argvFor(next, handoffID string) []string {
	switch next {
	case "wait":
		return []string{"relay", "agent", "wait", handoffID}
	case "send":
		return []string{"relay", "agent", "send", handoffID, "--", "<text>"}
	case "capture":
		return []string{"relay", "agent", "capture", handoffID}
	case "done":
		return []string{"relay", "agent", "done", handoffID}
	case "escalate":
		return []string{"relay", "agent", "capture", handoffID}
	case "status":
		return []string{"relay", "agent", "status", handoffID}
	default:
		return nil
	}
}

func (h *HandoffService) agentBase(ho *Handoff) AgentResponse {
	return AgentResponse{
		OK:        true,
		V:         1,
		HandoffID: ho.ID,
		LastSeq:   ho.LastSeq,
	}
}

// AgentStart launches a handoff and returns next=wait (never suggests tail -f loops).
func (h *HandoffService) AgentStart(ctx context.Context, opts HandoffOpts) (*AgentResponse, error) {
	b, ho, err := h.Launch(ctx, opts)
	if err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error(), Next: ""}, err
	}
	resp := h.agentBase(ho)
	resp.SessionID = ho.SessionID
	resp.HostID = ho.HostID
	resp.Kind = string(ho.Kind)
	resp.Status = string(ho.Status)
	resp.Next = "wait"
	resp.Argv = argvFor("wait", ho.ID)
	if b != nil {
		resp.Extra = map[string]any{"pane": b.Pane}
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
	resp.Kind = string(ho.Kind)
	resp.Status = string(ho.Status)
	switch {
	case ho.Outcome != "" || ho.Status == StatusDone || ho.Status == StatusFailed || ho.Status == StatusAbandoned:
		resp.Next = "null"
		resp.Argv = nil
	case ho.Status == StatusNeedsInput && ho.Kind == KindAgent:
		resp.Next = "send"
		resp.Argv = argvFor("send", ho.ID)
	default:
		resp.Next = "wait"
		resp.Argv = append(argvFor("wait", ho.ID), "--from", fmt.Sprintf("%d", ho.LastSeq))
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
		case "needs_input", "permission_required", "ask":
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
		actionable := ev.Kind == "exit" || ev.Kind == "needs_input" || ev.Kind == "permission_required" ||
			ev.Kind == "ask" || ev.Kind == "note" || ev.Kind == "progress" || ev.Kind == "result" ||
			(ev.Kind == "idle" && ho.Kind == KindAgent)
		if !actionable {
			return true // keep waiting
		}
		cp := ev
		got = &cp
		if h.ParentRouter != nil {
			_, _ = h.ParentRouter.RouteChildEvent(wctx, ho, ev)
		}
		cancel() // stop subscribe
		return false
	})

	// reload for latest status
	if latest, err := h.Reg.GetHandoff(handoffID); err == nil {
		ho = latest
	}
	resp := h.agentBase(ho)
	if got != nil {
		resp.Event = compactAgentEvent(got)
		resp.LastSeq = got.Seq
		next := DecideNext(ho.Kind, got.Kind, false)
		resp.Next = next
		resp.Argv = argvFor(next, ho.ID)
		if next == "wait" {
			resp.Argv = append(resp.Argv, "--from", fmt.Sprintf("%d", got.Seq))
		}
		return &resp, nil
	}

	timedOut := wctx.Err() == context.DeadlineExceeded
	if timedOut || subErr != nil {
		next := DecideNext(ho.Kind, "", timedOut)
		resp.TimedOut = timedOut
		resp.Next = next
		resp.Argv = append(argvFor("wait", ho.ID), "--from", fmt.Sprintf("%d", ho.LastSeq))
		if !timedOut && subErr != nil && wctx.Err() == nil {
			resp.OK = false
			resp.Error = subErr.Error()
			return &resp, subErr
		}
		return &resp, nil
	}
	resp.Next = "wait"
	resp.Argv = append(argvFor("wait", ho.ID), "--from", fmt.Sprintf("%d", ho.LastSeq))
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
		resp.Argv = append(argvFor("wait", ho.ID), "--from", fmt.Sprintf("%d", ho.LastSeq))
		return &resp, fmt.Errorf("refuse send on job handoff")
	}
	if err := h.Sessions.Send(ctx, ho.SessionID, text, true); err != nil {
		return &AgentResponse{OK: false, V: 1, Error: err.Error(), HandoffID: ho.ID}, err
	}
	resp := h.agentBase(ho)
	resp.Next = "wait"
	resp.Argv = append(argvFor("wait", ho.ID), "--from", fmt.Sprintf("%d", ho.LastSeq))
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
		resp.Argv = argvFor("send", ho.ID)
	} else {
		resp.Next = "wait"
		resp.Argv = append(argvFor("wait", ho.ID), "--from", fmt.Sprintf("%d", ho.LastSeq))
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
	resp.Status = string(ho.Status)
	resp.Next = "null"
	resp.Argv = nil
	resp.Extra = map[string]any{"outcome": ho.Outcome, "exit_code": ho.ExitCode}
	return &resp, nil
}
