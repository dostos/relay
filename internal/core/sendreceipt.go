package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// SendResult is the receipt `relay session send` prints. It embeds the
// adapter receipt (deposited / submitted / uncertain) and, when the caller
// supplied an idempotency key, whether this answer is a replay of an earlier
// attempt rather than a new delivery.
type SendResult struct {
	ports.SendReceipt
	SessionID      string    `json:"session_id"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Replayed       bool      `json:"replayed,omitempty"`
	At             time.Time `json:"at"`
	Error          string    `json:"error,omitempty"`
	// RecordWarning is set when the delivery happened but its receipt could
	// not be stored on the host (a later replay will then report uncertain).
	RecordWarning string `json:"record_warning,omitempty"`
}

// ErrSendInProgress means another send holding the same idempotency key is
// still running. Nothing was typed by this call.
var ErrSendInProgress = errors.New("another send with this idempotency key is in progress")

var opTokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidateIdempotencyKey bounds caller keys to a path- and shell-safe token.
func ValidateIdempotencyKey(key string) error {
	if !opTokenRE.MatchString(key) {
		return fmt.Errorf("invalid idempotency key %q (use 1-128 of [A-Za-z0-9._:-], starting alphanumeric)", key)
	}
	return nil
}

func validateOpToken(kind, v string) error {
	if !opTokenRE.MatchString(v) {
		return fmt.Errorf("invalid %s %q", kind, v)
	}
	return nil
}

// remoteStateRoot is where host-side receipts live. It is on the target host,
// not the control host, so it survives whichever SSH connection drops.
const remoteStateRoot = `"$HOME/.local/state/relay"`

func sendRecordDir(sessionID, key string) string {
	return remoteStateRoot + "/sends/" + shellquote.Quote(sessionID) + "/" + shellquote.Quote(key)
}

// Send delivers text without an idempotency key. See SendWithReceipt.
func (s *SessionService) Send(ctx context.Context, id, text string, enter bool) error {
	_, err := s.SendWithReceipt(ctx, id, text, enter, "")
	return err
}

// SendWithReceipt types text into the session (and submits it once when
// enter is set) and returns a structured receipt.
//
// With a non-empty key the attempt is recorded on the target host before
// anything is typed. A later call with the same key never types again: it
// replays the recorded receipt, and an attempt that was interrupted after
// typing may have begun replays as uncertain. Uncertain stays explicit; the
// caller decides, after looking, whether to send under a new key.
func (s *SessionService) SendWithReceipt(ctx context.Context, id, text string, enter bool, key string) (SendResult, error) {
	res := SendResult{SendReceipt: ports.SendReceipt{State: ports.SendNotSent}, SessionID: id, IdempotencyKey: key, At: time.Now().UTC()}
	sess, err := s.Reg.GetSession(id)
	if err != nil {
		return res, err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return res, err
	}
	if err := validateOpToken("session id", sess.ID); err != nil {
		return res, err
	}
	if key == "" {
		rc, err := s.deliver(ctx, t, sess, text, enter)
		res.SendReceipt = rc
		res.Error = errString(err)
		return res, err
	}
	if err := ValidateIdempotencyKey(key); err != nil {
		return res, err
	}
	d := sendRecordDir(sess.ID, key)
	// mkdir is the atomic claim. A claim whose owner died before typing
	// (no "typing" marker) and is older than a minute may be taken over by
	// renaming it aside, which only one contender can do.
	claim := fmt.Sprintf(`d=%s
mkdir -p "$(dirname "$d")" || exit 1
try() {
  if mkdir "$d" 2>/dev/null; then echo relay-send=claimed; exit 0; fi
  if [ -f "$d/receipt.json" ]; then echo relay-send=recorded; cat "$d/receipt.json"; exit 0; fi
  if [ -e "$d/typing" ]; then echo relay-send=interrupted; exit 0; fi
}
try
if [ -n "$(find "$d" -maxdepth 0 -mmin +1 2>/dev/null)" ] && mv "$d" "$d.stale.$$" 2>/dev/null; then try; fi
echo relay-send=in-progress
`, d)
	out, stderr, err := t.Run(ctx, "", claim)
	if err != nil {
		return res, fmt.Errorf("record send intent: %w (%s)", err, strings.TrimSpace(stderr))
	}
	head, body, _ := strings.Cut(out, "\n")
	switch strings.TrimSpace(head) {
	case "relay-send=claimed":
	case "relay-send=recorded":
		var prior SendResult
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(body)), &prior); jerr != nil {
			res.State = ports.SendUncertain
			res.Replayed = true
			res.Evidence = "unreadable_receipt"
			err := &ports.DeliveryUncertainError{Err: fmt.Errorf("a send with key %q was already attempted; its receipt is unreadable", key)}
			res.Error = err.Error()
			return res, err
		}
		prior.Replayed = true
		prior.RecordWarning = ""
		if prior.State == ports.SendSubmitted || (prior.State == ports.SendDeposited && !enter) {
			prior.Error = ""
			return prior, nil
		}
		if prior.State == ports.SendNotSent {
			// Recorded as never typed; nothing to protect. Leave it to the
			// caller to retry under a fresh key so the record stays truthful.
			return prior, fmt.Errorf("a send with key %q previously failed before typing: %s", key, prior.Error)
		}
		err := &ports.DeliveryUncertainError{Err: fmt.Errorf("a send with key %q was already attempted and ended %s; not resending", key, prior.State)}
		prior.Error = err.Error()
		return prior, err
	case "relay-send=interrupted":
		res.State = ports.SendUncertain
		res.Replayed = true
		res.Evidence = "interrupted_attempt"
		err := &ports.DeliveryUncertainError{Err: fmt.Errorf("a send with key %q was interrupted after typing may have begun; not resending", key)}
		res.Error = err.Error()
		return res, err
	case "relay-send=in-progress":
		res.Error = ErrSendInProgress.Error()
		return res, ErrSendInProgress
	default:
		return res, fmt.Errorf("record send intent: unexpected reply %q", strings.TrimSpace(out))
	}
	marked := false
	rc, sendErr := s.deliverWith(ctx, t, sess, text, enter, func() error {
		marked = true
		if _, stderr, err := t.Run(ctx, "", fmt.Sprintf(`: > %s/typing`, d)); err != nil {
			// The marker may or may not exist; a replay will conservatively
			// say uncertain. Nothing was typed by this call.
			return fmt.Errorf("mark send attempt: %v (%s)", err, strings.TrimSpace(stderr))
		}
		return nil
	})
	if rc.State == ports.SendNotSent && sendErr != nil {
		res.SendReceipt = rc
		res.Error = sendErr.Error()
		if !marked {
			// Refused before the typing marker (busy, at a gate, lock
			// failure): nothing was typed, so release the claim and let the
			// caller retry under the same key.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			_, _, _ = t.Run(rctx, "", fmt.Sprintf(`rm -rf %s`, d))
			cancel()
		}
		return res, sendErr
	}
	res.SendReceipt = rc
	res.Error = errString(sendErr)
	if b, jerr := json.Marshal(res); jerr == nil {
		write := fmt.Sprintf(`d=%s; printf '%%s\n' %s > "$d/receipt.json.tmp" && mv "$d/receipt.json.tmp" "$d/receipt.json"`, d, shellquote.Quote(string(b)))
		// Record even if ctx was cancelled mid-send: the receipt is what
		// keeps a retry from typing twice.
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_, stderr, werr := t.Run(wctx, "", write)
		cancel()
		if werr != nil {
			res.RecordWarning = fmt.Sprintf("receipt not stored: %v (%s)", werr, strings.TrimSpace(stderr))
		}
	}
	return res, sendErr
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ErrSendBusy means another send to the same session held the host-side send
// lock for the whole wait. Nothing was typed.
var ErrSendBusy = errors.New("another send to this session is still in progress")

// ErrTargetAtGate means the target pane shows an approval/login/trust prompt.
// Typing or pressing Enter there would answer the prompt, so nothing was sent.
var ErrTargetAtGate = errors.New("target is waiting at an approval prompt")

// sendLockWait bounds how long a send waits for an earlier send to the same
// session; sendLockStaleMin is when an abandoned lock may be taken over.
var (
	sendLockWait     = 60 * time.Second
	sendLockStaleMin = 2
	sendGateLines    = 40
)

func sendLockDir(sessionID string) string {
	return remoteStateRoot + "/sends/" + shellquote.Quote(sessionID+".lock")
}

func (s *SessionService) deliver(ctx context.Context, t ports.Transport, sess *Session, text string, enter bool) (ports.SendReceipt, error) {
	return s.deliverWith(ctx, t, sess, text, enter, nil)
}

// deliverWith serializes sends to one session on its host (#66), refuses to
// type into a pane that is waiting at a gate (#67), then delivers through the
// persistence adapter. before runs under the lock right before typing; an
// error from it aborts with nothing typed.
func (s *SessionService) deliverWith(ctx context.Context, t ports.Transport, sess *Session, text string, enter bool, before func() error) (ports.SendReceipt, error) {
	notSent := ports.SendReceipt{State: ports.SendNotSent}
	release, err := acquireSendLock(ctx, t, sess.ID)
	if err != nil {
		if errors.Is(err, ErrSendBusy) {
			notSent.Evidence = "send_lock_busy"
		}
		return notSent, err
	}
	defer release()
	if screen, cerr := s.Persist.Capture(ctx, t, sess.Persist, sendGateLines); cerr == nil {
		if r := ClassifyAgentPane(screen); r.State == AgentBlocked {
			return ports.SendReceipt{State: ports.SendNotSent, Evidence: "target_at_gate"},
				fmt.Errorf("%w (%s); nothing was typed. Inspect with `relay session readiness %s` and answer the prompt first", ErrTargetAtGate, r.Reason, sess.ID)
		}
	}
	if before != nil {
		if err := before(); err != nil {
			return notSent, err
		}
	}
	return s.Persist.Send(ctx, t, sess.Persist, text, enter)
}

// acquireSendLock takes an atomic mkdir lock on the target host. Sends from
// different processes or machines to the same session otherwise interleave
// their text and Enters into one turn while each reports success (#66).
func acquireSendLock(ctx context.Context, t ports.Transport, sessionID string) (func(), error) {
	d := sendLockDir(sessionID)
	token := NewOpID()
	tries := int(sendLockWait / (200 * time.Millisecond))
	if tries < 1 {
		tries = 1
	}
	script := fmt.Sprintf(`d=%s
mkdir -p "$(dirname "$d")" || exit 1
n=0
while ! mkdir "$d" 2>/dev/null; do
  if [ -n "$(find "$d" -maxdepth 0 -mmin +%d 2>/dev/null)" ] && mv "$d" "$d.stale.$$" 2>/dev/null; then rm -rf "$d.stale.$$"; continue; fi
  n=$((n+1)); if [ "$n" -ge %d ]; then echo relay-lock=busy; exit 0; fi
  sleep 0.2
done
printf '%%s\n' %s > "$d/owner"
echo relay-lock=held
`, d, sendLockStaleMin, tries, shellquote.Quote(token))
	out, stderr, err := t.Run(ctx, "", script)
	if err != nil {
		return nil, fmt.Errorf("take send lock: %w (%s)", err, strings.TrimSpace(stderr))
	}
	switch strings.TrimSpace(out) {
	case "relay-lock=held":
	case "relay-lock=busy":
		return nil, fmt.Errorf("%w (waited %s); nothing was typed", ErrSendBusy, sendLockWait)
	default:
		return nil, fmt.Errorf("take send lock: unexpected reply %q", strings.TrimSpace(out))
	}
	return func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_, _, _ = t.Run(rctx, "", fmt.Sprintf(`d=%s; [ "$(cat "$d/owner" 2>/dev/null)" = %s ] && rm -rf "$d"; true`, d, shellquote.Quote(token)))
	}, nil
}
