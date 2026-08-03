package tmux

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dostos/relay/internal/ports"
)

type recordingTransport struct {
	commands []string
	stdout   string
	outputs  []string
	errs     []error
	err      error
}

func (t *recordingTransport) ID() string { return "test" }
func (t *recordingTransport) Run(_ context.Context, _, command string) (string, string, error) {
	t.commands = append(t.commands, command)
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
	if err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "relay marker", true); err == nil {
		t.Fatal("missing pane-level evidence was reported as delivered")
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
