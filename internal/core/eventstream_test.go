package core

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type cancellationBlindCoord struct {
	release chan struct{}
}

func (*cancellationBlindCoord) Kind() string                                  { return "blind" }
func (*cancellationBlindCoord) Ensure(context.Context, ports.Transport) error { return nil }
func (*cancellationBlindCoord) Emit(context.Context, ports.Transport, string, string, map[string]any) (int64, error) {
	return 0, nil
}
func (c *cancellationBlindCoord) Subscribe(context.Context, ports.Transport, string, int64, bool, io.Writer) error {
	<-c.release
	return nil
}
func (*cancellationBlindCoord) EventsPath(string) string                     { return "" }
func (*cancellationBlindCoord) SensorCommand(string, string) (string, error) { return "", nil }

func TestStreamEventsHonorsDeadlineWhenTransportDoesNot(t *testing.T) {
	coord := &cancellationBlindCoord{release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := streamEvents(ctx, coord, nil, "stream", 0, true, func(Event) bool { return true })
	close(coord.release)
	if err == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("deadline err=%v elapsed=%s", err, time.Since(started))
	}
}
