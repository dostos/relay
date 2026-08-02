package core

import (
	"context"

	"github.com/dostos/relay/internal/ports"
)

// PresentSession lets a visualization service own SSH and placement policy
// while the control plane supplies only authoritative session identity.
func PresentSession(ctx context.Context, viz ports.Viz, sess *Session, attachCmd string, layout ports.Layout) (string, error) {
	if sink, ok := viz.(ports.ProjectionSink); ok {
		return sink.ApplyProjection(ctx, projectionForSession(sess, firstNonEmpty(sess.SourceSessionID, layout.SourceSessionID), ports.ProjectionUpsert))
	}
	return viz.Present(ctx, sess.ID, attachCmd, layout)
}

func ProjectSession(ctx context.Context, viz ports.Viz, sess *Session, op ports.ProjectionOp) (string, error) {
	if sink, ok := viz.(ports.ProjectionSink); ok {
		return sink.ApplyProjection(ctx, projectionForSession(sess, sess.SourceSessionID, op))
	}
	switch op {
	case ports.ProjectionFocus:
		return "", viz.Focus(ctx, sess.ID)
	case ports.ProjectionDelete:
		return "", viz.Close(ctx, sess.ID)
	default:
		return viz.Present(ctx, sess.ID, ResumeLaunchCmd(sess.Persist.Name), ports.Layout{Mode: "remote", SourceSessionID: sess.SourceSessionID})
	}
}

func projectionForSession(sess *Session, anchor string, op ports.ProjectionOp) ports.ProjectionEvent {
	return ports.ProjectionEvent{V: 1, Op: op, Item: ports.Presentation{
		SessionID: sess.ID, ParentSessionID: anchor, Target: sess.HostID, TmuxName: sess.Persist.Name,
	}}
}
