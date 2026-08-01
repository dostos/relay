package cmux

import "testing"

// The live failure this guards against: an escalation was typed into the
// manager's composer, the ENTER was swallowed by a popup, and cmux reported
// success for both calls. relay stamped delivered_at and moved on, so the
// question sat unsent for 16 minutes while every status said healthy.
func TestComposerHoldsDetectsAnUnsentMessage(t *testing.T) {
	screen := `
  No further production fixes are allowed during this gate.


› [relay ask pm-ca9bdc1781367036 folio-system-improvement-c1@c1 ho-6e99c44]
  remote child idle on c1; inspect handoff, manager decide blocked/completed

  Create a plan?  shift + tab use Plan mode   esc dismiss
`
	if !composerHolds(screen, "pm-ca9bdc1781367036") {
		t.Fatal("a message still on the composer line must be reported as unsent")
	}
}

// After submission the text moves into the transcript, where it legitimately
// still appears. Treating that as "unsent" would make every delivery look
// failed and re-send forever.
func TestComposerHoldsIgnoresTheTranscript(t *testing.T) {
	screen := `
› [relay ask pm-ca9bdc1781367036 folio-system-improvement-c1@c1 ho-6e99c44]
  remote child idle on c1; manager decide blocked/completed

• Working (3s • esc to interrupt)

› Improve documentation in @filename
`
	if composerHolds(screen, "pm-ca9bdc1781367036") {
		t.Fatal("once the composer shows its placeholder the message was sent")
	}
}

// An unfamiliar agent UI has no recognisable composer. Reporting "held" there
// would block delivery to every such pane, so absence of evidence must read as
// sent rather than stuck.
func TestComposerHoldsAssumesSentWhenNoComposerIsRecognisable(t *testing.T) {
	if composerHolds("just some output\nwith no prompt glyph\n", "pm-x") {
		t.Fatal("an unrecognisable UI must not be treated as stuck")
	}
	if composerHolds("", "pm-x") {
		t.Fatal("an empty capture must not be treated as stuck")
	}
}

func TestComposerHoldsRequiresTheMarker(t *testing.T) {
	if composerHolds("› [relay ask pm-other other@c1 ho-zzz]\n", "pm-ca9bdc17") {
		t.Fatal("a different message in the composer is not ours")
	}
}
