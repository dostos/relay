package core

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
)

// streamEvents runs Coord.Subscribe for one stream and invokes fn for each
// decoded, non-heartbeat event with Seq > fromSeq. fn returns false to stop
// early (e.g. after the first actionable event). It centralizes the io.Pipe +
// scanner + JSON-decode + heartbeat/seq filter that the message bus (Read,
// WaitOne) and the agent loop (AgentWait) previously each re-implemented.
func streamEvents(ctx context.Context, c ports.Coord, t ports.Transport, stream string, fromSeq int64, follow bool, fn func(coord.Event) bool) error {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Subscribe(subCtx, t, stream, fromSeq, follow, pw)
		_ = pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev coord.Event
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Heartbeat || ev.Kind == "heartbeat" || ev.Seq <= fromSeq {
			continue
		}
		if !fn(ev) {
			cancel()
			break
		}
	}
	_ = pr.Close()
	return <-errCh // subscribe error (nil on clean end; ctx err on cancel)
}
