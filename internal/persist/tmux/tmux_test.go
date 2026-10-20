package tmux

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

type recordingTransport struct {
	commands []string
	stdout   string
	outputs  []string
	errs     []error
	err      error
	runHook  func(*recordingTransport, string) (string, bool)
}

type localExecTransport struct{ recordingTransport }

func (t *localExecTransport) Run(ctx context.Context, cwd, command string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.CombinedOutput()
	return string(out), "", err
}

func (t *recordingTransport) ID() string { return "test" }
func (t *recordingTransport) Run(_ context.Context, _, command string) (string, string, error) {
	t.commands = append(t.commands, command)
	if t.runHook != nil {
		if out, ok := t.runHook(t, command); ok {
			return out, "", nil
		}
	}
	callErr := t.err
	if len(t.errs) > 0 {
		callErr = t.errs[0]
		t.errs = t.errs[1:]
	}
	if len(t.outputs) > 0 {
		out := t.outputs[0]
		t.outputs = t.outputs[1:]
		return out, "", callErr
	}
	return t.stdout, "", callErr
}

// paneScript models a pane: display-message returns the pane id, every
// capture-pane returns the next screen (the last one repeats), and send-keys
// succeeds. screens[0] is the pre-send snapshot.
func paneScript(pane string, screens ...string) *recordingTransport {
	transport := &recordingTransport{}
	captures := 0
	transport.runHook = func(_ *recordingTransport, command string) (string, bool) {
		switch {
		case strings.Contains(command, "display-message"):
			return pane, true
		case strings.Contains(command, "capture-pane"):
			i := captures
			if i >= len(screens) {
				i = len(screens) - 1
			}
			captures++
			return screens[i], true
		default:
			return "", true
		}
	}
	return transport
}

func noSendDelay(t *testing.T) {
	t.Helper()
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
}

func enters(transport *recordingTransport, pane string) int {
	return strings.Count(strings.Join(transport.commands, "\n"), "send-keys -t '"+pane+"' Enter")
}

func TestSendSubmitsOnceAndConfirmsFreshTurn(t *testing.T) {
	noSendDelay(t)
	idle := "previous answer\n❯ \n"
	transport := paneScript("%42", idle, "previous answer\n❯ relay marker\n", "relay marker\n• Working (0s • esc to interrupt)\n")
	rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "relay marker", true)
	if err != nil {
		t.Fatal(err)
	}
	if rc.State != ports.SendSubmitted || rc.Evidence != "fresh_working_indicator" || rc.Submits != 1 {
		t.Fatalf("receipt = %+v", rc)
	}
	joined := strings.Join(transport.commands, "\n")
	if !strings.Contains(joined, "display-message -p -t '=agent:'") {
		t.Fatalf("send must resolve an explicit pane id first: %v", transport.commands)
	}
	if enters(transport, "%42") != 1 || strings.Count(joined, "send-keys -t '%42' -l -- 'relay marker'") != 1 {
		t.Fatalf("expected one type and exactly one Enter against pane id: %v", transport.commands)
	}
	if strings.Contains(joined, "send-keys -t '=agent:'") {
		t.Fatalf("submit must not use the exact-name session target: %v", transport.commands)
	}
	// The pre-send snapshot must be taken before anything is typed.
	if first := strings.Index(joined, "capture-pane"); first < 0 || first > strings.Index(joined, "send-keys") {
		t.Fatalf("no pre-send snapshot: %v", transport.commands)
	}
}

func TestSendStaleWorkingMarkerIsNotAcknowledgement(t *testing.T) {
	noSendDelay(t)
	// #59: an old Working line in scrollback satisfied the old check even
	// though the new text never left the composer.
	stale := "• Working (9s • esc to interrupt)\n─ Worked for 9s ─\ndone.\n\n  gpt-5.6-sol default · ~/dev\n"
	deposited := "• Working (9s • esc to interrupt)\n─ Worked for 9s ─\ndone.\n  Status check on the mandate\n\n  gpt-5.6-sol default · ~/dev\n"
	transport := paneScript("%9", stale, deposited)
	rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "apex"}, "Status check on the mandate", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) {
		t.Fatalf("stale Working marker was reported as delivered: %+v", rc)
	}
	if rc.State != ports.SendDeposited || rc.Evidence != "composer_holds_text" {
		t.Fatalf("receipt should say deposited, got %+v", rc)
	}
	if !strings.Contains(err.Error(), "no turn started") {
		t.Fatalf("error should distinguish deposit-without-turn, got %v", err)
	}
	if enters(transport, "%9") != 1 {
		t.Fatalf("Enter must not be repeated after an uncertain submit: %v", transport.commands)
	}
	if rc.Polls != sendConfirmPolls {
		t.Fatalf("polls = %d", rc.Polls)
	}
}

func TestSendDelayedOutputIsConfirmedWithoutSecondEnter(t *testing.T) {
	noSendDelay(t)
	pre := "  gpt-5.6-sol default · ~/dev\n"
	held := "  Escalate only if a conclusion belongs to the human\n\n  gpt-5.6-sol default · ~/dev\n"
	working := "  Escalate only if a conclusion belongs to the human\n• Working (1s • esc to interrupt)\n"
	transport := paneScript("%7", pre, held, held, held, held, held, working)
	rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "apex"}, "Escalate only if a conclusion belongs to the human", true)
	if err != nil {
		t.Fatal(err)
	}
	if rc.State != ports.SendSubmitted || rc.Polls != 6 {
		t.Fatalf("receipt = %+v", rc)
	}
	if enters(transport, "%7") != 1 {
		t.Fatalf("delayed render must not trigger another Enter: %v", transport.commands)
	}
}

func TestSendLiveTurnNeedsTimerReset(t *testing.T) {
	noSendDelay(t)
	pre := "✻ Cogitating… (40s · esc to interrupt)\n"
	t.Run("ticking timer is the old turn", func(t *testing.T) {
		transport := paneScript("%5", pre, "✻ Cogitating… (42s · esc to interrupt)\n")
		rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "busy"}, "next task", true)
		var uncertain *ports.DeliveryUncertainError
		if err == nil || !errors.As(err, &uncertain) || rc.State != ports.SendUncertain {
			t.Fatalf("an already-running turn was credited to this send: %+v %v", rc, err)
		}
	})
	t.Run("restarted timer is a new turn", func(t *testing.T) {
		transport := paneScript("%5", pre, "✻ Cogitating… (40s · esc to interrupt)\n", "next task\n✻ Cogitating… (1s · esc to interrupt)\n")
		rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "busy"}, "next task", true)
		if err != nil || rc.Evidence != "working_timer_reset" {
			t.Fatalf("receipt = %+v err=%v", rc, err)
		}
	})
}

func TestSendComposerReleaseNeedsNewTranscriptEntry(t *testing.T) {
	noSendDelay(t)
	t.Run("new entry", func(t *testing.T) {
		transport := paneScript("%2", "❯ \n", "> relay marker\nACK\n❯ \n")
		rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "a"}, "relay marker", true)
		if err != nil || rc.Evidence != "composer_released" {
			t.Fatalf("receipt = %+v err=%v", rc, err)
		}
	})
	t.Run("same text already in scrollback", func(t *testing.T) {
		old := "relay marker\nACK\n❯ \n"
		transport := paneScript("%2", old, old)
		rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "a"}, "relay marker", true)
		var uncertain *ports.DeliveryUncertainError
		if err == nil || !errors.As(err, &uncertain) {
			t.Fatalf("an earlier identical message was credited to this send: %+v", rc)
		}
	})
}

func TestSendDoesNotClaimUnknownDisappearance(t *testing.T) {
	noSendDelay(t)
	transport := paneScript("%3", "idle\n", "popup swallowed input\n")
	rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "relay marker", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) {
		t.Fatal("missing pane-level evidence was reported as delivered")
	}
	if rc.State != ports.SendUncertain || rc.Evidence != "no_fresh_turn" || enters(transport, "%3") != 1 {
		t.Fatalf("receipt = %+v commands=%v", rc, transport.commands)
	}
}

func TestSendWithoutEnterIsDeposited(t *testing.T) {
	noSendDelay(t)
	transport := paneScript("%3", "idle\n")
	rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "draft", false)
	if err != nil || rc.State != ports.SendDeposited || enters(transport, "%3") != 0 {
		t.Fatalf("receipt = %+v err=%v commands=%v", rc, err, transport.commands)
	}
}

func TestSendSnapshotFailureTypesNothing(t *testing.T) {
	noSendDelay(t)
	transport := &recordingTransport{}
	transport.runHook = func(_ *recordingTransport, command string) (string, bool) {
		if strings.Contains(command, "display-message") {
			return "%3", true
		}
		return "", false
	}
	transport.err = errors.New("ssh: connection lost")
	rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "x", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || errors.As(err, &uncertain) || rc.State != ports.SendNotSent {
		t.Fatalf("pre-mutation failure must be not_sent: %+v %v", rc, err)
	}
	if strings.Contains(strings.Join(transport.commands, "\n"), "send-keys") {
		t.Fatalf("typed after a failed snapshot: %v", transport.commands)
	}
}

func TestSendTransportLossAfterTypingIsUncertain(t *testing.T) {
	noSendDelay(t)
	transport := paneScript("%3", "idle\n")
	hook := transport.runHook
	transport.runHook = func(tp *recordingTransport, command string) (string, bool) {
		if strings.Contains(command, " Enter") {
			return "", false
		}
		return hook(tp, command)
	}
	transport.err = errors.New("ssh: connection lost")
	rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "x", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) || rc.State != ports.SendUncertain {
		t.Fatalf("receipt = %+v err=%v", rc, err)
	}
}

func TestLaunchAcknowledgesHoldingShellWithoutComposerEvidence(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	for _, launchCommand := range []string{
		"claude --goal x",
		"codex --goal x",
		"grok --goal x",
		"make verify",
	} {
		t.Run(strings.Fields(launchCommand)[0], func(t *testing.T) {
			transport := &recordingTransport{outputs: []string{"%11"}}
			// Capture the generated token from the typed launch line and return
			// it from show-option. This models a shell effect without a composer.
			transport.runHook = func(tpt *recordingTransport, command string) (string, bool) {
				if strings.Contains(command, "display-message") {
					return "%11", true
				}
				if strings.Contains(command, "show-option") {
					for _, prior := range tpt.commands {
						if i := strings.Index(prior, "@relay_launch_ack "); i >= 0 {
							rest := prior[i+len("@relay_launch_ack "):]
							if token := regexp.MustCompile(`[a-z0-9]{8,}`).FindString(rest); token != "" {
								return token, true
							}
						}
					}
				}
				return "", false
			}
			if err := New().Launch(context.Background(), transport, ports.PersistHandle{Name: "worker"}, launchCommand); err != nil {
				t.Fatalf("%v; commands=%v", err, transport.commands)
			}
			joined := strings.Join(transport.commands, "\n")
			if strings.Count(joined, launchCommand) != 1 || strings.Count(joined, "send-keys -t '%11' Enter") != 1 {
				t.Fatalf("launch should type once and submit once against pane id: %v", transport.commands)
			}
		})
	}
}

func TestLaunchPollsWithoutResubmitting(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{outputs: []string{"%5"}}
	transport.runHook = func(_ *recordingTransport, command string) (string, bool) {
		if strings.Contains(command, "display-message") {
			return "%5", true
		}
		return "", false
	}
	err := New().Launch(context.Background(), transport, ports.PersistHandle{Name: "job"}, "make verify")
	if err == nil {
		t.Fatal("missing holding-shell acknowledgement reported as launched")
	}
	joined := strings.Join(transport.commands, "\n")
	if strings.Count(joined, "make verify") != 1 || strings.Count(joined, "send-keys -t '%5' Enter") != 1 || strings.Count(joined, "show-option") != sendConfirmAttempts {
		t.Fatalf("launch must submit once and retry only effect reads: %v", transport.commands)
	}
}

func TestSendResolvesPaneIDBecauseBareSessionTargetFails(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })

	name := "relay-send-target-" + strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
	transport := &localExecTransport{}
	handle, err := New().Create(context.Background(), transport, name, "", "bash -l")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = New().Destroy(context.Background(), transport, handle) })

	pane, err := New().resolvePaneID(context.Background(), transport, handle)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pane, "%") {
		t.Fatalf("expected %%pane id, got %q", pane)
	}
	// Exact reproduction from #46: list-panes accepts =name, send-keys does not.
	if out, _, err := transport.Run(context.Background(), "", "tmux list-panes -t "+shellquote.Quote(exactSession(name))+" -F '#{pane_id}'"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("list-panes should accept =name: out=%q err=%v", out, err)
	}
	if out, _, err := transport.Run(context.Background(), "", "tmux send-keys -t "+shellquote.Quote(exactSession(name))+" Enter"); err == nil {
		t.Fatalf("bare =name send-keys unexpectedly succeeded: %q", out)
	}
	if _, _, err := transport.Run(context.Background(), "", "tmux send-keys -t "+shellquote.Quote(pane)+" Enter"); err != nil {
		t.Fatalf("pane id send-keys must work: %v", err)
	}

	// A bare shell has no agent turn signal; Send must not report ok for a
	// composer-only deposit.
	_, err = New().Send(context.Background(), transport, handle, "relay marker for issue 46", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) {
		t.Fatal("send reported success without turn-start evidence")
	}
	if !strings.Contains(err.Error(), "no turn started") && !strings.Contains(err.Error(), "no fresh turn") {
		t.Fatalf("expected a distinguishable submit failure, got %v", err)
	}
}

func TestLaunchDisposableTmuxEffect(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	name := "relay-launch-canary-" + strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
	transport := &localExecTransport{}
	handle, err := New().Create(context.Background(), transport, name, "", "bash -l")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = New().Destroy(context.Background(), transport, handle) })
	marker := filepath.Join(t.TempDir(), "effect")
	command := "printf launched > " + shellquote.Quote(marker) + "; exec bash -l"
	if err := New().Launch(context.Background(), transport, handle, command); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, readErr := os.ReadFile(marker)
		if readErr == nil && string(data) == "launched" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("launch acknowledged but command effect missing: %v", readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, _, _ := transport.Run(context.Background(), "", "tmux show-option -t "+shellquote.Quote(exactPane(name))+" -v @relay_launch_ack 2>/dev/null"); strings.TrimSpace(out) != "" {
		t.Fatalf("launch acknowledgement leaked into later retries: %q", out)
	}
}

func TestLaunchDoesNotLeakRetryEnterIntoInteractiveRuntime(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	name := "relay-gate-canary-" + strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
	transport := &localExecTransport{}
	handle, err := New().Create(context.Background(), transport, name, "", "bash -l")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = New().Destroy(context.Background(), transport, handle) })
	leaked := filepath.Join(t.TempDir(), "unexpected-key")
	runtime := "if read -r -t 1 -n 1 key; then printf %s \"$key\" > " + shellquote.Quote(leaked) + "; fi; exec bash -l"
	command := "bash -c " + shellquote.Quote(runtime)
	if err := New().Launch(context.Background(), transport, handle, command); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if data, err := os.ReadFile(leaked); err == nil {
		t.Fatalf("launch polling injected a key into the runtime: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestDestroyUsesExactSessionTarget(t *testing.T) {
	transport := &recordingTransport{}
	if err := New().Destroy(context.Background(), transport, ports.PersistHandle{Name: "apex"}); err != nil {
		t.Fatal(err)
	}
	if got := transport.commands[0]; !strings.Contains(got, "kill-session -t '=apex'") {
		t.Fatalf("destroy command permits prefix matching: %q", got)
	}
}

func TestRenameUsesExactSessionTarget(t *testing.T) {
	transport := &recordingTransport{outputs: []string{"relay-absent", ""}}
	if err := New().Rename(context.Background(), transport, ports.PersistHandle{Name: "apex"}, ports.PersistHandle{Name: "apex-v4"}); err != nil {
		t.Fatal(err)
	}
	if got := transport.commands[1]; !strings.Contains(got, "rename-session -t '=apex'") {
		t.Fatalf("rename command permits prefix matching: %q", got)
	}
}

func TestInstallSensorsUsesSessionColonTarget(t *testing.T) {
	transport := &recordingTransport{outputs: []string{"%3", ""}}
	err := New().InstallSensors(context.Background(), transport, ports.PersistHandle{Name: "phyzfuzz-feas-alt"}, 45, func(kind string) (string, error) {
		return "echo " + kind, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.commands) != 2 {
		t.Fatalf("commands = %v", transport.commands)
	}
	if !strings.Contains(transport.commands[0], "display-message -p -t '=phyzfuzz-feas-alt:'") {
		t.Fatalf("sensors must wait for a resolvable pane before install: %q", transport.commands[0])
	}
	got := transport.commands[1]
	if !strings.Contains(got, `SESS='=phyzfuzz-feas-alt:'`) {
		t.Fatalf("sensors must target '=name:' for tmux 3.2a set-option, got %q", got)
	}
	if strings.Contains(got, `SESS='=phyzfuzz-feas-alt'`) && !strings.Contains(got, `SESS='=phyzfuzz-feas-alt:'`) {
		t.Fatalf("bare '=name' breaks set-option on tmux 3.2a: %q", got)
	}
	if strings.Count(got, "|| :") != 2 {
		t.Fatalf("sensor failures must not leak into tmux messages: %q", got)
	}
}

func TestInstallSensorsRetriesUntilPaneResolves(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{
		outputs: []string{"", "%4", ""},
		errs:    []error{errors.New("can't find pane: =fresh"), nil, nil},
	}
	err := New().InstallSensors(context.Background(), transport, ports.PersistHandle{Name: "fresh"}, 10, func(kind string) (string, error) {
		return "echo " + kind, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.commands) < 3 {
		t.Fatalf("expected resolve retry then install, got %v", transport.commands)
	}
}

func TestComposerHoldsIgnoresSubmittedScrollback(t *testing.T) {
	if composerHolds("❯ relay marker\nACCEPTED:relay marker\n", "relay marker") {
		t.Fatal("output below the old composer proves submission")
	}
}

func TestComposerHoldsOpaquePastedContent(t *testing.T) {
	screen := "⚠ MCP startup incomplete\n\n› [Pasted Content 2048 chars]\n\n  gpt-5.6-sol default"
	if !composerHolds(screen, "a long goal whose marker is hidden by the TUI") {
		t.Fatal("opaque pasted content was mistaken for a delivered message")
	}
}

func TestFreshTurnIgnoresStaleIndicators(t *testing.T) {
	stale := "• Working (9s • esc to interrupt)\n"
	if ev := freshTurn(stale, stale+"  marker\n", "marker"); ev != "" {
		t.Fatalf("stale indicator credited: %s", ev)
	}
	if ev := freshTurn("", "• Working (0s • esc to interrupt)", "marker"); ev != "fresh_working_indicator" {
		t.Fatalf("new indicator not credited: %q", ev)
	}
	if ev := freshTurn("Working...\n", "Working...\n• Working (0s • esc to interrupt)", "m"); ev != "fresh_working_indicator" {
		t.Fatalf("timed indicator after untimed stale text: %q", ev)
	}
	if ev := freshTurn("• Working (1m 05s • esc to interrupt)", "• Working (3s • esc to interrupt)", "m"); ev != "working_timer_reset" {
		t.Fatalf("timer reset: %q", ev)
	}
	deposit := "  Escalate only if a conclusion belongs to the human\n\n  gpt-5.6-sol default · ~/dev/dostos-workspace"
	if ev := freshTurn("", deposit, "Escalate only if a conclusion belongs"); ev != "" {
		t.Fatalf("glyph-less composer deposit must not count as a started turn: %s", ev)
	}
}

func (t *recordingTransport) RunStream(context.Context, string, string, io.Writer) error {
	return nil
}
func (t *recordingTransport) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (t *recordingTransport) WriteFile(context.Context, string, []byte, string) error {
	return nil
}
func (t *recordingTransport) Interactive(context.Context, string) error { return nil }

func TestExistsUsesExactSessionName(t *testing.T) {
	transport := &recordingTransport{stdout: "relay-absent"}
	exists, err := New().Exists(context.Background(), transport, ports.PersistHandle{Name: "engram"})
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("prefix-only session must not count as an exact match")
	}
	if len(transport.commands) != 1 {
		t.Fatalf("commands = %v", transport.commands)
	}
	command := transport.commands[0]
	if !strings.Contains(command, "has-session -t '=engram'") {
		t.Fatalf("expected exact name lookup, got %q", command)
	}
}

func TestExistsDoesNotTreatTransportFailureAsMissing(t *testing.T) {
	transport := &recordingTransport{err: errors.New("ssh unavailable")}
	if _, err := New().Exists(context.Background(), transport, ports.PersistHandle{Name: "engram"}); err == nil {
		t.Fatal("transport failure was reported as an absent session")
	}
}

// A surface that comes up on a fresh shell must say so. The old command was
// `tmux new-session -A`, which attaches or creates and reports neither, so a
// host that had lost the session produced a healthy-looking prompt with the
// work silently gone. Verified live on hamburg 2026-09-05.
func TestAttachCommandDistinguishesAttachFromCreate(t *testing.T) {
	got := (&Persist{}).AttachCommand(ports.PersistHandle{Name: "work"}, "")

	if strings.Contains(got, "new-session -A") {
		t.Errorf("-A attaches or creates and reports neither:\n%s", got)
	}
	// The existing-session path must be a plain attach, with nothing printed
	// into a session that is carrying real work.
	if !strings.Contains(got, "has-session -t 'work'") {
		t.Errorf("no existence check, so the two paths cannot differ:\n%s", got)
	}
	if !strings.Contains(got, "exec tmux attach -t 'work'") {
		t.Errorf("attach path missing:\n%s", got)
	}
	attachIdx := strings.Index(got, "exec tmux attach")
	noticeIdx := strings.Index(got, "relay: session")
	if noticeIdx < 0 {
		t.Fatalf("create path prints no notice:\n%s", got)
	}
	if noticeIdx < attachIdx {
		t.Errorf("the notice must belong to the create path, not the attach path:\n%s", got)
	}
	// The notice has to survive: printing before `exec tmux new-session` is
	// erased by the repaint, so it is the new session's own first line.
	if !strings.Contains(got, "new-session -s 'work' -- bash -lc") {
		t.Errorf("notice is not carried inside the new session:\n%s", got)
	}
	// The hand-over is the holding shell, not a second login shell: a login
	// shell re-reads /etc/profile's TMOUT and logs the recreated session out
	// after ten idle minutes (hamburg/oqb, 2026-10-15).
	if !strings.Contains(got, "exec "+ports.HoldingShell) {
		t.Errorf("create path must hand over the TMOUT-free holding shell:\n%s", got)
	}
	if strings.Contains(got, "exec bash -l") {
		t.Errorf("create path re-enters a login shell, which restores TMOUT:\n%s", got)
	}
}

// A session recreated after its host lost it must start where it ran, not in
// $HOME: on 2026-10-10 a madrid reboot brought three sessions back in ~.
func TestAttachCommandCreatesInRecordedCWD(t *testing.T) {
	got := (&Persist{}).AttachCommand(ports.PersistHandle{Name: "work"}, "~/dev/ws")
	if !strings.Contains(got, `new-session -s 'work' -c "$HOME"'/dev/ws' -- bash -lc`) {
		t.Errorf("create path does not start in the recorded cwd:\n%s", got)
	}
	if strings.Contains(got[:strings.Index(got, "else")], "-c ") {
		t.Errorf("attach path must not carry a cwd:\n%s", got)
	}
	if bare := (&Persist{}).AttachCommand(ports.PersistHandle{Name: "work"}, ""); strings.Contains(bare, " -c ") {
		t.Errorf("empty cwd must not emit -c:\n%s", bare)
	}
}

func TestAttachExistingCommandNeverCreates(t *testing.T) {
	got := (&Persist{}).AttachExistingCommand(ports.PersistHandle{Name: "work"})
	if strings.Contains(got, "new-session") {
		t.Errorf("--no-create attach must never create:\n%s", got)
	}
	for _, want := range []string{"has-session -t 'work'", "exec tmux attach -t 'work'", "does not exist on", ">&2", "exit 66"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if ports.ExitSessionAbsent != 66 {
		t.Errorf("documented exit code changed: %d", ports.ExitSessionAbsent)
	}
}

// TestSendRealTmuxFreshTurnVsStaleMarker drives a real tmux pane with a fake
// agent: one answers each submitted line with a fresh Working line; the other
// printed a Working line long ago and then never starts a turn.
func TestSendRealTmuxFreshTurnVsStaleMarker(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 150 * time.Millisecond
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	cases := []struct {
		name, agent string
		want        ports.SendState
	}{
		{"fresh", `while IFS= read -r l; do sleep 0.3; printf '• Working (0s • esc to interrupt)\n'; done`, ports.SendSubmitted},
		{"stale", `printf '• Working (12s • esc to interrupt)\nWorked for 12s\n'; IFS= read -r l; sleep 30`, ports.SendDeposited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "relay-send-" + tc.name + "-" + strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
			transport := &localExecTransport{}
			handle, err := New().Create(context.Background(), transport, name, "", "bash -c "+shellquote.Quote(tc.agent))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = New().Destroy(context.Background(), transport, handle) })
			time.Sleep(300 * time.Millisecond)
			rc, err := New().Send(context.Background(), transport, handle, "do the thing", true)
			if rc.State != tc.want {
				t.Fatalf("receipt = %+v err=%v", rc, err)
			}
			var uncertain *ports.DeliveryUncertainError
			if (tc.want == ports.SendSubmitted) != (err == nil) || (err != nil && !errors.As(err, &uncertain)) {
				t.Fatalf("err = %v for %s", err, rc.State)
			}
			if rc.Submits != 1 {
				t.Fatalf("submits = %d", rc.Submits)
			}
		})
	}
}
