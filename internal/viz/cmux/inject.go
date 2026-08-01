package cmux

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Injecting a message into an agent pane is two steps — type the text, then
// press ENTER — and cmux reports success for both as long as the keystrokes
// were accepted. Whether the agent actually received anything is a different
// question: a popup can swallow the ENTER, leaving the message sitting in the
// composer while relay stamps delivered_at and moves on.
//
// That happened to a real escalation: it sat unsent in beholder-pdf-main's
// composer for sixteen minutes while every status reported healthy, because
// nothing ever checked whether the send landed.
//
// So delivery is confirmed by reading the pane back, not by the exit status of
// a keystroke.

const (
	injectConfirmAttempts = 3
	injectConfirmDelay    = 600 * time.Millisecond
	injectConfirmLines    = 14
)

// composerPrefixes are the glyphs agent CLIs use to mark the input line.
var composerPrefixes = []string{"›", ">", "❯"}

// composerHolds reports whether marker is still sitting on the pane's input
// line, i.e. typed but never submitted.
//
// Only the bottom-most composer line counts. After a successful submit the same
// text remains on screen in the transcript — and some UIs render a submitted
// user message with the same prompt glyph — so anything less specific would
// report every delivered message as stuck and re-send forever. The live input
// line is always the last one.
//
// It is best-effort by design: if the screen cannot be parsed, or the UI has no
// recognisable composer, it reports "not held". An unfamiliar agent UI must
// degrade to the old optimistic behaviour rather than have delivery blocked.
func composerHolds(screen, marker string) bool {
	if marker == "" {
		return false
	}
	composer, found := "", false
	for _, line := range strings.Split(screen, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range composerPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				composer, found = trimmed, true
				break
			}
		}
	}
	return found && strings.Contains(composer, marker)
}

// submitInjected presses ENTER and confirms the message left the composer,
// retrying a bounded number of times.
//
// Returning an error matters as much as the retry: the caller treats a failed
// notify as an undelivered message, so it stays pending and is retried or
// reported as stalled. Swallowing the failure is what let a question go missing
// while looking delivered.
func (v *Viz) submitInjected(ctx context.Context, sessionID string, b binding, marker string) error {
	for attempt := 0; attempt < injectConfirmAttempts; attempt++ {
		if _, err := v.run(ctx, surfaceCommand("send-key", b.Surface, b.Workspace, "ENTER")...); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(injectConfirmDelay):
		}
		screen, err := v.CaptureScreen(ctx, sessionID, injectConfirmLines)
		if err != nil {
			// Cannot verify: assume the keystroke landed rather than
			// re-sending blindly into a pane we cannot read.
			return nil
		}
		if !composerHolds(screen, marker) {
			return nil
		}
	}
	return fmt.Errorf("injected message %s is still unsent in %s's composer after %d attempts",
		marker, sessionID, injectConfirmAttempts)
}
