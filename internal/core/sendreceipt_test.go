package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type countingPersist struct {
	ports.Persistence
	sends   int
	receipt ports.SendReceipt
	err     error
	screen  string
	onSend  func()
}

func (c *countingPersist) Capture(context.Context, ports.Transport, ports.PersistHandle, int) (string, error) {
	if c.screen == "" {
		return "done.\n\n› \n\n  gpt-5.6-sol default · ~/dev\n", nil
	}
	return c.screen, nil
}

func (c *countingPersist) Send(context.Context, ports.Transport, ports.PersistHandle, string, bool) (ports.SendReceipt, error) {
	c.sends++
	if c.onSend != nil {
		c.onSend()
	}
	return c.receipt, c.err
}

func sendFixture(t *testing.T, p *countingPersist) (*SessionService, *shellTransport) {
	t.Helper()
	svc, tr, _ := opsFixture(t)
	svc.Persist = p
	return svc, tr
}

func TestSendIdempotencyKeyNeverTypesTwice(t *testing.T) {
	p := &countingPersist{receipt: ports.SendReceipt{State: ports.SendSubmitted, Evidence: "fresh_working_indicator", Submits: 1}}
	svc, _ := sendFixture(t, p)
	first, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k-1")
	if err != nil || first.State != ports.SendSubmitted || first.Replayed {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	second, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k-1")
	if err != nil || second.State != ports.SendSubmitted || !second.Replayed || second.Evidence != "fresh_working_indicator" {
		t.Fatalf("retry = %+v err=%v", second, err)
	}
	if p.sends != 1 {
		t.Fatalf("typed %d times", p.sends)
	}
	if _, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k-2"); err != nil || p.sends != 2 {
		t.Fatalf("a new key is a new message: sends=%d err=%v", p.sends, err)
	}
}

func TestSendIdempotencyKeepsUncertainExplicit(t *testing.T) {
	p := &countingPersist{
		receipt: ports.SendReceipt{State: ports.SendDeposited, Evidence: "composer_holds_text", Submits: 1},
		err:     &ports.DeliveryUncertainError{Err: errors.New("no turn started")},
	}
	svc, _ := sendFixture(t, p)
	if _, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k"); err == nil {
		t.Fatal("expected uncertain")
	}
	p.receipt, p.err = ports.SendReceipt{State: ports.SendSubmitted}, nil
	again, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k")
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) || again.State != ports.SendDeposited || !again.Replayed {
		t.Fatalf("replay must stay uncertain, got %+v err=%v", again, err)
	}
	if p.sends != 1 {
		t.Fatalf("uncertain send was retyped (%d)", p.sends)
	}
}

func TestSendIdempotencyTransportLossAfterTyping(t *testing.T) {
	p := &countingPersist{receipt: ports.SendReceipt{State: ports.SendUncertain, Submits: 1}, err: &ports.DeliveryUncertainError{Err: errConnLost}}
	svc, tr := sendFixture(t, p)
	tr.dropBefore = func(cmd string) bool { return strings.Contains(cmd, "receipt.json.tmp") }
	res, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k")
	if err == nil || res.RecordWarning == "" {
		t.Fatalf("lost receipt write must be surfaced: %+v", res)
	}
	tr.dropBefore = nil
	p.receipt, p.err = ports.SendReceipt{State: ports.SendSubmitted}, nil
	again, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k")
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) || again.Evidence != "interrupted_attempt" {
		t.Fatalf("interrupted attempt must replay as uncertain: %+v err=%v", again, err)
	}
	if p.sends != 1 {
		t.Fatalf("typed %d times", p.sends)
	}
}

func TestSendIdempotencyClaimAckLost(t *testing.T) {
	p := &countingPersist{receipt: ports.SendReceipt{State: ports.SendSubmitted}}
	svc, tr := sendFixture(t, p)
	tr.dropAfter = func(cmd string) bool { return strings.Contains(cmd, "relay-send=claimed") }
	if _, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k"); err == nil {
		t.Fatal("expected error")
	}
	tr.dropAfter = nil
	if p.sends != 0 {
		t.Fatal("typed without a claim ack")
	}
	// A fresh claim held by an unknown owner is "in progress", never retyped.
	if _, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k"); !errors.Is(err, ErrSendInProgress) {
		t.Fatalf("err = %v", err)
	}
	// Once the dead claim is stale (no typing marker), a retry takes it over.
	dir := filepath.Join(tr.home, ".local/state/relay/sends/sess-ops/k")
	if _, _, err := tr.Run(context.Background(), "", "touch -d '5 minutes ago' "+dir); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hello", true, "k")
	if err != nil || res.State != ports.SendSubmitted || p.sends != 1 {
		t.Fatalf("takeover = %+v err=%v sends=%d", res, err, p.sends)
	}
}

func TestSendWithoutKeyPassesThrough(t *testing.T) {
	p := &countingPersist{receipt: ports.SendReceipt{State: ports.SendSubmitted}}
	svc, tr := sendFixture(t, p)
	res, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hi", true, "")
	// Without a key the only host state is the per-session send lock
	// (take + release); no receipt is recorded.
	if err != nil || res.State != ports.SendSubmitted || tr.calls != 2 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, tr.calls)
	}
	if _, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hi", true, "bad key/../"); err == nil {
		t.Fatal("unsafe key accepted")
	}
}

const codexApprovalPane = `• Ran tests

  Would you like to run the following command?

  $ rm -rf build

› 1. Yes, proceed (y)
  2. Yes, and don't ask again for this command in this session (a)
  3. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel
`

// #67: Enter on an approval menu selects "Yes, proceed".
func TestSendRefusesWhileTargetIsAtApprovalGate(t *testing.T) {
	p := &countingPersist{screen: codexApprovalPane, receipt: ports.SendReceipt{State: ports.SendSubmitted}}
	svc, _ := sendFixture(t, p)
	for _, key := range []string{"", "k-gate"} {
		res, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hey, any progress?", true, key)
		if !errors.Is(err, ErrTargetAtGate) || res.State != ports.SendNotSent || p.sends != 0 {
			t.Fatalf("key=%q: sent into an approval prompt: %+v err=%v sends=%d", key, res, err, p.sends)
		}
	}
	// Nothing was typed, so the same key is usable once the gate is answered.
	p.screen = ""
	res, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hey, any progress?", true, "k-gate")
	if err != nil || res.Replayed || p.sends != 1 {
		t.Fatalf("retry after gate: %+v err=%v sends=%d", res, err, p.sends)
	}
}

// #66: two senders never type into the same session at the same time.
func TestSendSerializesConcurrentSenders(t *testing.T) {
	var mu sync.Mutex
	active, maxActive := 0, 0
	p := &countingPersist{receipt: ports.SendReceipt{State: ports.SendSubmitted}}
	p.onSend = func() {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
	}
	svc, _ := sendFixture(t, p)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each sender is its own process in practice; share nothing but the host.
			_, err := svc.SendWithReceipt(context.Background(), "sess-ops", "msg", true, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxActive != 1 || p.sends != 4 {
		t.Fatalf("overlapping sends: max=%d sends=%d", maxActive, p.sends)
	}
}

func TestSendLockBusyAndStaleTakeover(t *testing.T) {
	p := &countingPersist{receipt: ports.SendReceipt{State: ports.SendSubmitted}}
	svc, tr := sendFixture(t, p)
	old := sendLockWait
	sendLockWait = 400 * time.Millisecond
	t.Cleanup(func() { sendLockWait = old })
	lock := filepath.Join(tr.home, ".local/state/relay/sends/sess-ops.lock")
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SendWithReceipt(context.Background(), "sess-ops", "msg", true, "")
	if !errors.Is(err, ErrSendBusy) || res.State != ports.SendNotSent || p.sends != 0 {
		t.Fatalf("held lock: %+v err=%v", res, err)
	}
	stale := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendWithReceipt(context.Background(), "sess-ops", "msg", true, ""); err != nil || p.sends != 1 {
		t.Fatalf("stale lock not taken over: err=%v sends=%d", err, p.sends)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock not released: %v", err)
	}
}
