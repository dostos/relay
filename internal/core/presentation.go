package core

import (
	"context"

	"github.com/dostos/relay/internal/ports"
)

// PresentSession lets a visualization service own SSH and placement policy
// while the control plane supplies only authoritative session identity.
func PresentSession(ctx context.Context, viz ports.Viz, sess *Session, attachCmd string, layout ports.Layout) (string, error) {
	if presenter, ok := viz.(ports.TargetPresenter); ok {
		return presenter.PresentTarget(ctx, ports.Presentation{
			SessionID:       sess.ID,
			ParentSessionID: firstNonEmpty(sess.SourceSessionID, layout.SourceSessionID),
			Target:          sess.HostID,
			TmuxName:        sess.Persist.Name,
		})
	}
	return viz.Present(ctx, sess.ID, attachCmd, layout)
}
