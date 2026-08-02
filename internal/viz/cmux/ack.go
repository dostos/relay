package cmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dostos/relay/internal/coord"
	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/core"
)

func (v *Viz) ackCursorPath() string {
	return v.cursorPath() + ".ack"
}

// SyncAcks folds visualization receipts back into the authoritative command
// server registry. Presentation is asynchronous: a queued ref is not a cmux
// surface until the Viz host has acknowledged the actual placement.
func (v *Viz) SyncAcks(ctx context.Context, reg *core.Registry) error {
	if v.ServiceID == "" || v.Control != nil || reg == nil {
		return nil
	}
	cursor := loadSequence(v.ackCursorPath())
	var out bytes.Buffer
	if err := coordrelayd.SubscribeLocal(localRelaydSocket(), v.serviceChannel()+"-ack", cursor, false, &out); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var event coord.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return err
		}
		if event.Heartbeat || event.Seq <= cursor {
			continue
		}
		if event.Kind == "viz_ack" {
			if err := applyPresentationAck(reg, event); err != nil {
				return err
			}
		}
		cursor = event.Seq
		if err := saveSequence(v.ackCursorPath(), cursor); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func applyPresentationAck(reg *core.Registry, event coord.Event) error {
	if stringMeta(event.Meta, "request_kind") != "present" {
		return nil
	}
	requestSeq, err := metaSequence(event.Meta["request_seq"])
	if err != nil {
		return err
	}
	rawResult := stringMeta(event.Meta, "result")
	var result struct {
		Surface string `json:"surface"`
	}
	if strings.HasPrefix(rawResult, "surface:") {
		result.Surface = rawResult // compatibility with the first Viz client
	} else if err := json.Unmarshal([]byte(rawResult), &result); err != nil {
		return fmt.Errorf("invalid visualization ack result for request %d", requestSeq)
	}
	if strings.TrimSpace(result.Surface) == "" {
		return fmt.Errorf("invalid visualization ack result for request %d", requestSeq)
	}
	queued := "viz:queued:" + strconv.FormatInt(requestSeq, 10)
	sessions, err := reg.ListSessions()
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		if sess.VizSurfaceRef != queued {
			continue
		}
		sess.VizSurfaceRef = result.Surface
		sess.UpdatedAt = time.Now().UTC()
		if err := reg.PutSession(sess); err != nil {
			return err
		}
		core.RememberPane(result.Surface, sess, true)
		_ = core.AppendLedger(map[string]any{
			"v": 1, "type": "viz_ack", "ts": time.Now().UTC().Format(time.RFC3339),
			"session_id": sess.ID, "request_seq": requestSeq, "surface": result.Surface,
		})
		return nil
	}
	return nil // request may belong to a session retired before Viz handled it
}

func metaSequence(value any) (int64, error) {
	switch v := value.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case json.Number:
		return v.Int64()
	default:
		return 0, fmt.Errorf("invalid visualization request sequence")
	}
}
