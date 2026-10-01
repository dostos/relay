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

func TestSendRetriesEnterUntilComposerClears(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{outputs: []string{
		"%42", // resolvePaneID
		"",    // type
		"",    // Enter 1
		"❯ relay marker\n", // still holding
		"",    // Enter 2
		"transcript relay marker\n❯ \n", // released
	}}
	if err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "relay marker", true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transport.commands, "\n")
	if !strings.Contains(joined, "display-message -p -t '=agent:'") {
		t.Fatalf("send must resolve an explicit pane id first: %v", transport.commands)
	}
	if strings.Count(joined, "send-keys -t '%42' Enter") != 2 || strings.Count(joined, "send-keys -t '%42' -l -- 'relay marker'") != 1 {
		t.Fatalf("expected one type and two enters against pane id, commands=%v", transport.commands)
	}
	if strings.Contains(joined, "send-keys -t '=agent:'") {
		t.Fatalf("submit must not use the exact-name session target: %v", transport.commands)
	}
}

func TestSendConfirmsTurnStartNotComposerDeposit(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{outputs: []string{
		"%7",
		"",
		"",
		"  Escalate only if a conclusion belongs to the human\n\n  gpt-5.6-sol default · ~/dev/dostos-workspace\n",
		"",
		"  Escalate only if a conclusion belongs to the human\n• Working (9s • esc to interrupt)\n",
	}}
	if err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "apex"}, "Escalate only if a conclusion belongs to the human", true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transport.commands, "\n")
	if strings.Count(joined, "send-keys -t '%7' Enter") != 2 {
		t.Fatalf("expected submit retries until Working appeared: %v", transport.commands)
	}
}

func TestSendFailsWhenTextDepositedWithoutTurn(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	idle := "  Escalate only if a conclusion belongs to the human\n  Status check on the mandate above\n\n  gpt-5.6-sol default · ~/dev/dostos-workspace\n"
	transport := &recordingTransport{outputs: []string{"%9", "", "", idle, "", idle, "", idle}}
	err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "apex"}, "Escalate only if a conclusion belongs to the human", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) {
		t.Fatal("composer-only deposit was reported as delivered")
	}
	if !strings.Contains(err.Error(), "no turn started") {
		t.Fatalf("error should distinguish deposit-without-turn, got %v", err)
	}
}

func TestSendDoesNotClaimUnknownDisappearance(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{outputs: []string{"%3", "", "", "popup swallowed input\n", "", "popup swallowed input\n", "", "popup swallowed input\n"}}
	err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "relay marker", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) {
		t.Fatal("missing pane-level evidence was reported as delivered")
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
	err = New().Send(context.Background(), transport, handle, "relay marker for issue 46", true)
	var uncertain *ports.DeliveryUncertainError
	if err == nil || !errors.As(err, &uncertain) {
		t.Fatal("send reported success without turn-start evidence")
	}
	if !strings.Contains(err.Error(), "no turn started") && !strings.Contains(err.Error(), "still unsent") {
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

func TestMessageSubmittedRequiresTurnOrReleasedComposer(t *testing.T) {
	deposit := "  Escalate only if a conclusion belongs to the human\n\n  gpt-5.6-sol default · ~/dev/dostos-workspace"
	if messageSubmitted(deposit, "Escalate only if a conclusion belongs") {
		t.Fatal("glyph-less composer deposit must not count as a started turn")
	}
	working := deposit + "\n• Working (9s • esc to interrupt)"
	if !messageSubmitted(working, "Escalate only if a conclusion belongs") {
		t.Fatal("Working indicator must count as turn start")
	}
	released := "Escalate only if a conclusion belongs\n❯ \n"
	if !messageSubmitted(released, "Escalate only if a conclusion belongs") {
		t.Fatal("empty composer after marker must count as submitted")
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
	if !strings.Contains(got, "exec bash -l") {
		t.Errorf("create path must still hand over a login shell:\n%s", got)
	}
}
