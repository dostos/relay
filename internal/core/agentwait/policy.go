// Package agentwait encodes the supervision wait contract for issue #49.
//
// The `relay agent wait` CLI surface was retired with the delegation handshake
// (5e23690, 2026-09-13). These types and pure functions keep the correctness
// invariants that bug was about, so a successor does not reintroduce the
// parent/child deadlock or the empty-stdout interim wake.
//
// Invariants:
//  1. Wait MUST wake when the child enters a state whose next verb is "send"
//     (needs_input, permission_required, ask, and agent idle) — not only on
//     the narrower manager-delivery wake set that skipped idle.
//  2. Every wake emits a JSON envelope with handoff_id, status, from_seq,
//     last_seq, next, and argv — never empty stdout.
//  3. Bare wait (UntilEvent) is not run-to-completion; use UntilNeedsInput or
//     UntilTerminal to wait until the child needs the parent or is finished.
package agentwait

import (
	"encoding/json"
	"fmt"
)

// Kind is the handoff kind that DecideNext branches on.
type Kind string

const (
	KindAgent Kind = "agent"
	KindJob   Kind = "job"
)

// Status is the durable handoff status projected into the wake envelope.
type Status string

const (
	StatusPending    Status = "pending"
	StatusRunning    Status = "running"
	StatusNeedsInput Status = "needs_input"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
	StatusAbandoned  Status = "abandoned"
)

// Until selects how long a waiter blocks before returning a wake envelope.
type Until string

const (
	// UntilEvent wakes on the first event whose next verb is not "wait"
	// (or on timeout). This is bare `wait` — not run-to-completion.
	UntilEvent Until = "event"
	// UntilNeedsInput wakes when next is "send" (child needs the parent) or
	// the handoff is terminal. Interim telemetry stays silent to the caller.
	UntilNeedsInput Until = "needs-input"
	// UntilTerminal wakes only on done/failed/abandoned/exit.
	UntilTerminal Until = "terminal"
)

// Envelope is the JSON body every wait wake must print — including interim
// wakes where next is still "wait" and the caller should re-arm.
type Envelope struct {
	OK        bool     `json:"ok"`
	HandoffID string   `json:"handoff_id"`
	Status    string   `json:"status"`
	FromSeq   int64    `json:"from_seq"`
	LastSeq   int64    `json:"last_seq"`
	Next      string   `json:"next"`
	Argv      []string `json:"argv"`
	EventKind string   `json:"event_kind,omitempty"`
	TimedOut  bool     `json:"timed_out,omitempty"`
	Until     string   `json:"until,omitempty"`
}

// DecideNext picks the single next verb after an event or timeout.
// Pure policy — unit-tested without SSH.
func DecideNext(kind Kind, evKind string, timedOut bool) string {
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
		// Explicit question — always actionable, including for jobs.
		return "send"
	case "note", "progress", "result", "started":
		return "wait"
	case "idle":
		if kind == KindJob {
			return "wait"
		}
		return "send"
	default:
		return "wait"
	}
}

// StatusAfterEvent projects the durable status a wait envelope should report
// after observing evKind (mirrors the pre-retirement applyHandoffEventStatus
// rules that mattered for supervision).
func StatusAfterEvent(kind Kind, prev Status, evKind string) Status {
	switch evKind {
	case "needs_input", "permission_required", "ask":
		return StatusNeedsInput
	case "idle":
		if kind == KindAgent {
			return StatusNeedsInput
		}
		return prev
	case "exit":
		return StatusDone
	case "started":
		if prev == StatusNeedsInput {
			return prev
		}
		return StatusRunning
	default:
		return prev
	}
}

// eventWakesManager is the pre-#49 delivery boundary: it deliberately skipped
// idle so manager composers were not interrupted by telemetry. AgentWait must
// NOT use this alone — see ShouldWake.
func eventWakesManager(evKind string, meta map[string]any) bool {
	switch evKind {
	case "ask", "needs_input", "permission_required", "exit":
		return true
	case "result":
		if meta != nil {
			if src, ok := meta["source"].(string); ok && src == "hook" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// ShouldWake reports whether a wait with the given Until policy returns after
// observing evKind. This is the #49 correctness fix: wake whenever next is
// "send" (or another non-wait verb the until mode cares about), including
// agent idle → needs_input, which eventWakesManager skipped.
func ShouldWake(kind Kind, until Until, evKind string, meta map[string]any) bool {
	next := DecideNext(kind, evKind, false)
	switch until {
	case UntilTerminal:
		return next == "done" || evKind == "exit"
	case UntilNeedsInput:
		// Parent must act (send/escalate) or the child is finished.
		return next == "send" || next == "escalate" || next == "done"
	default: // UntilEvent
		if next != "wait" {
			return true
		}
		// Interim milestone that still advances the cursor: keep the old
		// manager-wake result path so callers can re-arm with --from.
		return eventWakesManager(evKind, meta)
	}
}

// ArgvFor builds the re-arm / follow-up argv for a next verb.
func ArgvFor(next, handoffID string, fromSeq int64) []string {
	switch next {
	case "wait":
		return []string{"relay", "agent", "wait", handoffID, "--from", fmt.Sprintf("%d", fromSeq)}
	case "send":
		return []string{"relay", "agent", "send", handoffID}
	case "done":
		return []string{"relay", "agent", "done", handoffID}
	case "escalate":
		return []string{"relay", "ask", "QUESTION"}
	default:
		return nil
	}
}

// BuildEnvelope constructs the mandatory wake JSON body. fromSeq is the cursor
// the waiter started at; lastSeq is the event that caused the wake (or the
// latest observed seq on timeout).
func BuildEnvelope(handoffID string, status Status, fromSeq, lastSeq int64, next string, evKind string, timedOut bool, until Until) Envelope {
	env := Envelope{
		OK:        true,
		HandoffID: handoffID,
		Status:    string(status),
		FromSeq:   fromSeq,
		LastSeq:   lastSeq,
		Next:      next,
		Argv:      ArgvFor(next, handoffID, lastSeq),
		EventKind: evKind,
		TimedOut:  timedOut,
		Until:     string(until),
	}
	if env.Argv == nil {
		env.Argv = []string{}
	}
	return env
}

// MarshalEnvelope encodes a wake body. It always yields a non-empty JSON
// object — the empty-stdout interim wake from #49 must not recur.
func MarshalEnvelope(env Envelope) ([]byte, error) {
	return json.Marshal(env)
}

// ParseUntil accepts CLI values for --until.
func ParseUntil(s string) (Until, error) {
	switch s {
	case "", "event":
		return UntilEvent, nil
	case "needs-input", "needs_input", "send":
		return UntilNeedsInput, nil
	case "terminal", "done":
		return UntilTerminal, nil
	default:
		return "", fmt.Errorf("unknown --until %q (event|needs-input|terminal)", s)
	}
}
