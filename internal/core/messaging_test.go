package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type draftPersist struct {
	countingPersist
	draft  string
	exists map[string]bool
}

func (d *draftPersist) ComposerDraft(context.Context, ports.Transport, ports.PersistHandle) (string, bool, error) {
	return d.draft, true, nil
}

func (d *draftPersist) Kind() string { return "tmux" }

func (d *draftPersist) Exists(_ context.Context, _ ports.Transport, h ports.PersistHandle) (bool, error) {
	return d.exists[h.Name], nil
}

// #70: a draft someone left in the composer is never typed onto or cleared.
func TestSendRefusesNonEmptyComposer(t *testing.T) {
	p := &draftPersist{countingPersist: countingPersist{receipt: ports.SendReceipt{State: ports.SendSubmitted}}, draft: "half-written human note"}
	svc, _ := sendFixture(t, &p.countingPersist)
	svc.Persist = p
	res, err := svc.SendWith(context.Background(), "sess-ops", "hello", SendOptions{Enter: true, Key: "k-draft"})
	if !errors.Is(err, ErrComposerNotEmpty) || res.State != ports.SendNotSent || res.Evidence != "composer_not_empty" || res.Leftover != "half-written human note" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if p.sends != 0 {
		t.Fatalf("typed onto a draft")
	}
	// The refusal released the key: once the human clears it, the same key sends.
	p.draft = ""
	if res, err := svc.SendWith(context.Background(), "sess-ops", "hello", SendOptions{Enter: true, Key: "k-draft"}); err != nil || res.Replayed || p.sends != 1 {
		t.Fatalf("retry after refusal: %+v err=%v sends=%d", res, err, p.sends)
	}
	p.draft = "another draft"
	if _, err := svc.SendWith(context.Background(), "sess-ops", "x", SendOptions{Enter: true, ForceAppend: true}); err != nil || p.sends != 2 {
		t.Fatalf("--force-append refused: %v", err)
	}
	// submit (no text) presses Enter on what is there by design.
	if _, err := svc.SendWith(context.Background(), "sess-ops", "", SendOptions{Enter: true}); err != nil || p.sends != 3 {
		t.Fatalf("submit refused: %v", err)
	}
}

// #71: queued is a success state and replays as success.
func TestQueuedReceiptReplaysAsSuccess(t *testing.T) {
	p := &countingPersist{receipt: ports.SendReceipt{State: ports.SendQueued, Evidence: "accepted_behind_busy_turn", Submits: 1}}
	svc, _ := sendFixture(t, p)
	if _, err := svc.SendWith(context.Background(), "sess-ops", "hi", SendOptions{Enter: true, Key: "q1"}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SendWith(context.Background(), "sess-ops", "hi", SendOptions{Enter: true, Key: "q1"})
	if err != nil || !res.Replayed || res.State != ports.SendQueued || p.sends != 1 {
		t.Fatalf("replay: %+v err=%v", res, err)
	}
}

// #72: HOST/NAME, unique NAME, ambiguous NAME, ad-hoc tmux targets.
func TestResolveSessionReferences(t *testing.T) {
	p := &draftPersist{exists: map[string]bool{"loose": true}}
	svc, _ := sendFixture(t, &p.countingPersist)
	svc.Persist = p
	if err := svc.Reg.PutSession(&Session{ID: "sess-a", HostID: "c1", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reg.PutSession(&Session{ID: "sess-b", HostID: "c3", Persist: ports.PersistHandle{Kind: "tmux", Name: "worker"}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if s, err := svc.Resolve(ctx, "sess-ops"); err != nil || s.ID != "sess-ops" {
		t.Fatalf("id: %v %v", s, err)
	}
	if s, err := svc.Resolve(ctx, "c3/worker"); err != nil || s.ID != "sess-b" {
		t.Fatalf("host/name: %v %v", s, err)
	}
	if s, err := svc.Resolve(ctx, "ops"); err != nil || s.ID != "sess-ops" {
		t.Fatalf("unique name: %v %v", s, err)
	}
	var amb *AmbiguousSessionError
	if _, err := svc.Resolve(ctx, "worker"); !errors.As(err, &amb) || len(amb.Candidates) != 2 || !strings.Contains(err.Error(), "c1/worker") {
		t.Fatalf("ambiguous: %v", err)
	}
	s, err := svc.Resolve(ctx, "h9/loose")
	if err != nil || !IsAdhoc(s) || s.Persist.Name != "loose" || s.HostID != "h9" {
		t.Fatalf("adhoc: %+v %v", s, err)
	}
	if got, err := svc.Get(s.ID); err != nil || got != s {
		t.Fatalf("adhoc lookup: %v", err)
	}
	var nf *SessionNotFoundError
	if _, err := svc.Resolve(ctx, "h9/missing"); !errors.As(err, &nf) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := svc.Resolve(ctx, "sess-nope"); !errors.As(err, &nf) {
		t.Fatalf("unknown id: %v", err)
	}
}

type screenSeq struct {
	countingPersist
	screens []string
	i       int
}

func (s *screenSeq) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	scr := s.screens[s.i]
	if s.i < len(s.screens)-1 {
		s.i++
	}
	return scr, nil
}

// #69: wait --idle sees busy, then requires a settled idle.
func TestWaitIdle(t *testing.T) {
	busy := func(n int) string {
		return "answer\n• Working (" + itoa(int64(n)) + "s • esc to interrupt)\n\n› \n  ? for shortcuts\n"
	}
	idle := "answer\nDone.\n\n› \n  ? for shortcuts\n"
	p := &screenSeq{screens: []string{busy(1), busy(2), busy(3), idle}}
	svc, _ := sendFixture(t, &p.countingPersist)
	svc.Persist = p
	res, err := svc.WaitIdle(context.Background(), "sess-ops", WaitOptions{Timeout: 5 * time.Second, Interval: 5 * time.Millisecond, Settle: 20 * time.Millisecond, TailLines: 5})
	if err != nil || res.Outcome != WaitIdle || !res.SawBusy || res.Samples < 5 || !strings.Contains(res.Tail, "Done.") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	p2 := &screenSeq{screens: []string{busy(1), busy(2)}}
	svc.Persist = p2
	p2.screens = []string{busy(1), busy(2), busy(3), busy(4), busy(5), busy(6), busy(7), busy(8), busy(9), busy(10), busy(11), busy(12)}
	res, _ = svc.WaitIdle(context.Background(), "sess-ops", WaitOptions{Timeout: 30 * time.Millisecond, Interval: 5 * time.Millisecond})
	if res.Outcome != WaitTimeout || res.Activity != ActivityBusy {
		t.Fatalf("timeout: %+v", res)
	}
	gate := "Would you like to run the following command?\n\n  $ rm -rf build\n\n› 1. Yes, proceed (y)\n  2. No, and tell Codex what to do differently (esc)\n"
	svc.Persist = &screenSeq{screens: []string{gate}}
	res, _ = svc.WaitIdle(context.Background(), "sess-ops", WaitOptions{Timeout: time.Second, Interval: 5 * time.Millisecond})
	if res.Outcome != WaitBlocked {
		t.Fatalf("gate: %+v", res)
	}
	// a frozen working line is stale, not a live turn
	svc.Persist = &screenSeq{screens: []string{busy(7)}}
	res, _ = svc.WaitIdle(context.Background(), "sess-ops", WaitOptions{Timeout: 2 * time.Second, Interval: 5 * time.Millisecond, Settle: 10 * time.Millisecond, StaleAfter: 30 * time.Millisecond})
	if res.Outcome != WaitIdle || !strings.Contains(res.Reason, "stale") {
		t.Fatalf("stale: %+v", res)
	}
}

