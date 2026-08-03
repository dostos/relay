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
	transport := &recordingTransport{outputs: []string{"", "", "❯ relay marker\n", "", "transcript relay marker\n❯ \n"}}
	if err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "relay marker", true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transport.commands, "\n")
	if strings.Count(joined, "send-keys -t '=agent:' Enter") != 2 || strings.Count(joined, "-l -- 'relay marker'") != 1 {
		t.Fatalf("expected one type and two enters, commands=%v", transport.commands)
	}
}

func TestSendDoesNotClaimUnknownDisappearance(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{stdout: "popup swallowed input\n"}
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
			transport := &recordingTransport{}
			// Capture the generated token from the typed launch line and return
			// it from show-option. This models a shell effect without a composer.
			transport.runHook = func(tpt *recordingTransport, command string) (string, bool) {
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
			if strings.Count(joined, launchCommand) != 1 || strings.Count(joined, " Enter") != 1 {
				t.Fatalf("launch should type once and submit once: %v", transport.commands)
			}
		})
	}
}

func TestLaunchPollsWithoutResubmitting(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{}
	err := New().Launch(context.Background(), transport, ports.PersistHandle{Name: "job"}, "make verify")
	if err == nil {
		t.Fatal("missing holding-shell acknowledgement reported as launched")
	}
	joined := strings.Join(transport.commands, "\n")
	if strings.Count(joined, "make verify") != 1 || strings.Count(joined, " Enter") != 1 || strings.Count(joined, "show-option") != sendConfirmAttempts {
		t.Fatalf("launch must submit once and retry only effect reads: %v", transport.commands)
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
	transport := &recordingTransport{}
	err := New().InstallSensors(context.Background(), transport, ports.PersistHandle{Name: "phyzfuzz-feas-alt"}, 45, func(kind string) (string, error) {
		return "echo " + kind, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.commands) != 1 {
		t.Fatalf("commands = %v", transport.commands)
	}
	got := transport.commands[0]
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

func TestApplyChromeUsesSessionColonTarget(t *testing.T) {
	transport := &recordingTransport{}
	if err := New().ApplyChrome(context.Background(), transport, ports.PersistHandle{Name: "phyzfuzz-feas-alt"}); err != nil {
		t.Fatal(err)
	}
	got := transport.commands[0]
	if !strings.Contains(got, `SESS='=phyzfuzz-feas-alt:'`) {
		t.Fatalf("chrome must target '=name:' for tmux 3.2a set-option, got %q", got)
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
func (t *recordingTransport) RunStream(context.Context, string, string, io.Writer) error {
	return nil
}
func (t *recordingTransport) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (t *recordingTransport) WriteFile(context.Context, string, []byte, string) error {
	return nil
}
func (t *recordingTransport) Interactive(context.Context, string) error { return nil }
func (t *recordingTransport) InteractiveCommand(string) string          { return "" }

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
