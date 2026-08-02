package tmux

import (
	"context"
	"fmt"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// ApplyChrome stamps a distinctive teal status bar so relay-managed tmux
// sessions are visually distinct from unmanaged attaches.
func ApplyChrome(ctx context.Context, t ports.Transport, h ports.PersistHandle) error {
	if err := shellquote.ValidateSessionName(h.Name); err != nil {
		return err
	}
	script := fmt.Sprintf(`
SESS=%s
tmux has-session -t "$SESS" 2>/dev/null || exit 1
tmux set-option -t "$SESS" status on
tmux set-option -t "$SESS" status-position top
tmux set-option -t "$SESS" status-style 'bg=#0f766e,fg=#ecfdf5,bold'
tmux set-option -t "$SESS" status-left-length 40
tmux set-option -t "$SESS" status-right-length 60
tmux set-option -t "$SESS" status-left '#[bold] ◆ RELAY #[fg=#99f6e4]│ '
tmux set-option -t "$SESS" status-right ' #[fg=#99f6e4]#S · #H '
tmux set-option -t "$SESS" pane-active-border-style 'fg=#2dd4bf,bold'
tmux set-option -t "$SESS" pane-border-style 'fg=#115e59'
tmux set-option -t "$SESS" message-style 'bg=#134e4a,fg=#ccfbf1'
`, shellquote.Quote(exactTarget(h.Name)))
	_, stderr, err := t.Run(ctx, "", script)
	if err != nil {
		return fmt.Errorf("apply chrome: %w (%s)", err, stderr)
	}
	return nil
}

func (p *Persist) ApplyChrome(ctx context.Context, t ports.Transport, h ports.PersistHandle) error {
	return ApplyChrome(ctx, t, h)
}
