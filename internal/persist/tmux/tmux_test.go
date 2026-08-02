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
	err      error
}

func (t *recordingTransport) ID() string { return "test" }
func (t *recordingTransport) Run(_ context.Context, _, command string) (string, string, error) {
	t.commands = append(t.commands, command)
	if len(t.outputs) > 0 {
		out := t.outputs[0]
		t.outputs = t.outputs[1:]
		return out, "", t.err
	}
	return t.stdout, "", t.err
}

func TestSendRetriesEnterUntilComposerClears(t *testing.T) {
	oldDelay := sendConfirmDelay
	sendConfirmDelay = 0
	t.Cleanup(func() { sendConfirmDelay = oldDelay })
	transport := &recordingTransport{outputs: []string{"", "", "❯ relay marker\n", "", "transcript\n❯ \n"}}
	if err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "agent"}, "relay marker", true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transport.commands, "\n")
	if strings.Count(joined, "send-keys -t '=agent:' Enter") != 2 || strings.Count(joined, "-l -- 'relay marker'") != 1 {
		t.Fatalf("expected one type and two enters, commands=%v", transport.commands)
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

func TestComposerHoldsIgnoresSubmittedScrollback(t *testing.T) {
	if composerHolds("❯ relay marker\nACCEPTED:relay marker\n", "relay marker") {
		t.Fatal("output below the old composer proves submission")
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
	transport := &recordingTransport{err: errors.New("not found")}
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
	if !strings.Contains(command, "list-sessions") || !strings.Contains(command, "grep -Fqx -- 'engram'") {
		t.Fatalf("expected exact name lookup, got %q", command)
	}
}
