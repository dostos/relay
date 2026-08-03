package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// A relay message channel is a relayd event stream named chan.<channel>. This
// keeps agent-to-agent chatter in its own namespace, distinct from the
// per-session handoff streams, while reusing relayd's authoritative seq log +
// subscribe (no new daemon, no poll loop).
func channelStream(channel string) string { return "chan." + channel }

// MsgEnvelope is one message on a channel — a relayd event with from/text
// lifted out of meta into the agent-facing shape for `relay msg`.
type MsgEnvelope struct {
	Channel string         `json:"channel,omitempty"`
	Seq     int64          `json:"seq"`
	TS      string         `json:"-"` // internal board ordering; event log owns it
	Kind    string         `json:"kind"`
	From    string         `json:"from,omitempty"`
	Text    string         `json:"text,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// MsgService is a thin agent-to-agent message bus over relayd channels.
type MsgService struct {
	Coord        ports.Coord
	NewTransport TransportFactory
}

func envelopeFromEvent(channel string, ev coord.Event) MsgEnvelope {
	m := MsgEnvelope{Channel: channel, Seq: ev.Seq, TS: ev.TS, Kind: ev.Kind}
	if ev.Meta != nil {
		rest := map[string]any{}
		for k, v := range ev.Meta {
			switch k {
			case "from":
				if s, ok := v.(string); ok {
					m.From = s
				}
			case "text":
				if s, ok := v.(string); ok {
					m.Text = s
				}
			default:
				rest[k] = v
			}
		}
		if len(rest) > 0 {
			m.Meta = rest
		}
	}
	return m
}

// Send publishes a message to a channel on host and returns its seq.
func (s *MsgService) Send(ctx context.Context, host, channel, kind, from, text string, meta map[string]any) (int64, error) {
	if host == "" || channel == "" {
		return 0, fmt.Errorf("host and channel required")
	}
	if kind == "" {
		kind = "msg"
	}
	t, err := s.NewTransport(host)
	if err != nil {
		return 0, err
	}
	if err := s.Coord.Ensure(ctx, t); err != nil {
		return 0, err
	}
	full := map[string]any{}
	for k, v := range meta {
		full[k] = v
	}
	if from != "" {
		full["from"] = from
	}
	if text != "" {
		full["text"] = text
	}
	if len(full) == 0 {
		full = nil
	}
	return s.Coord.Emit(ctx, t, channelStream(channel), kind, full)
}

// RemoveChannels deletes channel event logs on host (explicit per-task cleanup,
// e.g. drop a coordination channel when the task is done). Returns the names it
// acted on (validated). Reuses the same rm path as `relay gc`.
func (s *MsgService) RemoveChannels(ctx context.Context, host string, channels []string) ([]string, error) {
	if host == "" || len(channels) == 0 {
		return nil, fmt.Errorf("host and at least one channel required")
	}
	t, err := s.NewTransport(host)
	if err != nil {
		return nil, err
	}
	var ok []string
	for _, c := range channels {
		if shellquote.ValidateSessionName(c) == nil {
			ok = append(ok, c)
		}
	}
	if len(ok) > 0 {
		removeChannels(ctx, t, ok)
	}
	return ok, nil
}

// Read drains messages on a channel from fromSeq. With follow it streams until
// timeout/ctx; otherwise it returns the current backlog. Returns the messages
// and the highest seq seen (the next cursor).
func (s *MsgService) Read(ctx context.Context, host, channel string, fromSeq int64, follow bool, timeout time.Duration) ([]MsgEnvelope, int64, error) {
	t, err := s.NewTransport(host)
	if err != nil {
		return nil, fromSeq, err
	}
	if err := s.Coord.Ensure(ctx, t); err != nil {
		return nil, fromSeq, err
	}
	rctx := ctx
	if follow && timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var out []MsgEnvelope
	last := fromSeq
	_ = streamEvents(rctx, s.Coord, t, channelStream(channel), fromSeq, follow, func(ev coord.Event) bool {
		message := envelopeFromEvent("", ev) // the read response owns channel
		out = append(out, message)
		if ev.Seq > last {
			last = ev.Seq
		}
		return true // drain all
	})
	return out, last, nil
}

// WaitOne blocks until the first new message on ANY of channels (each with its
// own fromSeq cursor), or timeout. This is the fan-in primitive: one call waits
// on N agents at once, first message wins. timedOut is true if none arrived.
func (s *MsgService) WaitOne(ctx context.Context, host string, channels []string, fromSeq map[string]int64, timeout time.Duration) (*MsgEnvelope, bool, error) {
	if len(channels) == 0 {
		return nil, false, fmt.Errorf("at least one --channel required")
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	t, err := s.NewTransport(host)
	if err != nil {
		return nil, false, err
	}
	if err := s.Coord.Ensure(ctx, t); err != nil {
		return nil, false, err
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	found := make(chan MsgEnvelope, len(channels))
	var wg sync.WaitGroup
	for _, ch := range channels {
		ch := ch
		from := fromSeq[ch]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = streamEvents(wctx, s.Coord, t, channelStream(ch), from, true, func(ev coord.Event) bool {
				select {
				case found <- envelopeFromEvent(ch, ev):
					cancel() // first message wins; stop the others
				default:
				}
				return false // stop after the first event on this channel
			})
		}()
	}
	go func() { wg.Wait(); close(found) }()

	select {
	case m, ok := <-found:
		cancel()
		if !ok {
			return nil, true, nil
		}
		return &m, false, nil
	case <-wctx.Done():
		return nil, true, nil
	}
}
