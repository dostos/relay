package cli

import (
	"errors"
	"testing"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func TestParseSendArgs(t *testing.T) {
	o, err := parseSendArgs("send", []string{"--idempotency-key", "k1", "--", "hello", "world"})
	if err != nil || o.key != "k1" || o.text != "hello world" {
		t.Fatalf("o=%+v err=%v", o, err)
	}
	// Legacy form without `--` keeps every word as text, even flag-looking ones.
	o, err = parseSendArgs("send", []string{"--not-a-flag", "text"})
	if err != nil || o.text != "--not-a-flag text" || o.key != "" {
		t.Fatalf("legacy o=%+v err=%v", o, err)
	}
	if _, err := parseSendArgs("send", []string{"--bogus", "--", "x"}); err == nil {
		t.Fatal("unknown flag before -- accepted")
	}
	if _, err := parseSendArgs("send", []string{"--"}); err == nil {
		t.Fatal("empty text accepted")
	}
	o, err = parseSendArgs("submit", []string{"--key", "k2"})
	if err != nil || o.text != "" || o.key != "k2" {
		t.Fatalf("submit o=%+v err=%v", o, err)
	}
	if _, err := parseSendArgs("submit", []string{"--", "text"}); err == nil {
		t.Fatal("submit must not take text")
	}
}

func TestParseExecArgs(t *testing.T) {
	o, err := parseExecArgs([]string{"--detach", "--op-id", "op-1", "--", "make", "test"})
	if err != nil || !o.detach || o.opID != "op-1" || o.cmd != "make test" {
		t.Fatalf("o=%+v err=%v", o, err)
	}
	o, err = parseExecArgs([]string{"--", "ls", "--detach"})
	if err != nil || o.detach || o.cmd != "ls --detach" {
		t.Fatalf("flags after -- belong to CMD: o=%+v err=%v", o, err)
	}
	o, err = parseExecArgs([]string{"echo", "hi"})
	if err != nil || o.cmd != "echo hi" {
		t.Fatalf("legacy o=%+v err=%v", o, err)
	}
	if _, err := parseExecArgs([]string{"--op-id", "x", "--", "true"}); err == nil {
		t.Fatal("--op-id without --detach accepted")
	}
}

func TestSendOutcomeExitCodes(t *testing.T) {
	a := New()
	if code := a.sendOutcome(core.SendResult{}, nil); code != 0 {
		t.Fatalf("ok = %d", code)
	}
	unc := &ports.DeliveryUncertainError{Err: errors.New("no turn")}
	if code := a.sendOutcome(core.SendResult{SendReceipt: ports.SendReceipt{State: ports.SendDeposited}}, unc); code != ports.ExitDeliveryUncertain {
		t.Fatalf("uncertain = %d", code)
	}
	if code := a.sendOutcome(core.SendResult{}, errors.New("session not found")); code != 1 {
		t.Fatalf("plain = %d", code)
	}
}
