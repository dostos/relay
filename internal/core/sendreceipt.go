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
	if key == "" {
		rc, err := s.Persist.Send(ctx, t, sess.Persist, text, enter)
		res.SendReceipt = rc
		res.Error = errString(err)
		return res, err
	}
	if err := ValidateIdempotencyKey(key); err != nil {
		return res, err
	}
	if err := validateOpToken("session id", sess.ID); err != nil {
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
	if _, stderr, err := t.Run(ctx, "", fmt.Sprintf(`: > %s/typing`, d)); err != nil {
		// The marker may or may not exist; a replay will conservatively say
		// uncertain. Nothing was typed by this call.
		res.Error = fmt.Sprintf("mark send attempt: %v (%s)", err, strings.TrimSpace(stderr))
		return res, errors.New(res.Error)
	}
	rc, sendErr := s.Persist.Send(ctx, t, sess.Persist, text, enter)
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
