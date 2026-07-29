package core

import (
	"context"
	"fmt"
	"io"
)

// fakeTransport implements ports.Transport for tests. Run returns per-host
// canned output; a host absent from outputs is treated as unreachable (error).
type fakeTransport struct {
	id      string
	outputs map[string]string
}

func (f *fakeTransport) ID() string { return f.id }
func (f *fakeTransport) Run(ctx context.Context, cwd, command string) (string, string, error) {
	out, ok := f.outputs[f.id]
	if !ok {
		return "", "", fmt.Errorf("unreachable host %s", f.id)
	}
	return out, "", nil
}
func (f *fakeTransport) RunStream(ctx context.Context, cwd, command string, w io.Writer) error {
	return nil
}
func (f *fakeTransport) ReadFile(ctx context.Context, path string) ([]byte, error) { return nil, nil }
func (f *fakeTransport) WriteFile(ctx context.Context, path string, data []byte, mode string) error {
	return nil
}
func (f *fakeTransport) Interactive(ctx context.Context, command string) error { return nil }
func (f *fakeTransport) InteractiveCommand(remoteCmd string) string            { return remoteCmd }
