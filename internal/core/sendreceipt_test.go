package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

type countingPersist struct {
	ports.Persistence
	sends   int
	receipt ports.SendReceipt
	err     error
}

func (c *countingPersist) Send(context.Context, ports.Transport, ports.PersistHandle, string, bool) (ports.SendReceipt, error) {
	c.sends++
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
	if err != nil || res.State != ports.SendSubmitted || tr.calls != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, tr.calls)
	}
	if _, err := svc.SendWithReceipt(context.Background(), "sess-ops", "hi", true, "bad key/../"); err == nil {
		t.Fatal("unsafe key accepted")
	}
}
