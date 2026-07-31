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
	err      error
}

func (t *recordingTransport) ID() string { return "test" }
func (t *recordingTransport) Run(_ context.Context, _, command string) (string, string, error) {
	t.commands = append(t.commands, command)
	return t.stdout, "", t.err
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
