package tmux

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// #71: text accepted while the earlier turn keeps running is queued, not a
// fresh turn.
func TestSendBehindBusyTurnIsQueued(t *testing.T) {
	noSendDelay(t)
	pre := "✻ Cogitating… (40s · esc to interrupt)\n\n› \n"
	t.Run("composer released while old timer ticks", func(t *testing.T) {
		transport := paneScript("%5", pre, "next task\n✻ Cogitating… (42s · esc to interrupt)\n\n› \n")
		rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "busy"}, "next task", true)
		if err != nil || rc.State != ports.SendQueued || rc.Evidence != "accepted_behind_busy_turn" || rc.Submits != 1 {
			t.Fatalf("receipt = %+v err=%v", rc, err)
		}
	})
	t.Run("codex queue line", func(t *testing.T) {
		pre := "• Working (40s • esc to interrupt)\n\n› \n"
		transport := paneScript("%5", pre, "• Working (42s • esc to interrupt)\n  ↳ next task\n\n› \n")
		rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "busy"}, "next task", true)
		if err != nil || rc.State != ports.SendQueued {
			t.Fatalf("receipt = %+v err=%v", rc, err)
		}
	})
	t.Run("idle pane is still submitted", func(t *testing.T) {
		transport := paneScript("%5", "done\n\n› \n", "next task\n• Working (0s • esc to interrupt)\n\n› \n")
		rc, err := New().Send(context.Background(), transport, ports.PersistHandle{Name: "idle"}, "next task", true)
		if err != nil || rc.State != ports.SendSubmitted {
			t.Fatalf("receipt = %+v err=%v", rc, err)
		}
	})
}

// #70: ComposerDraft reads a real pane with styling, so a faint placeholder
// is not a draft but typed text is.
func TestComposerDraftRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	cases := []struct{ name, paint, want string }{
		{"placeholder", `printf 'done.\n\n\342\200\272 \033[2mImplement {feature}\033[0m\n\n  \033[2m? for shortcuts\033[0m'`, ""},
		{"draft", `printf 'done.\n\n\342\200\272 my unsent note\n\n  \033[2m? for shortcuts\033[0m'`, "my unsent note"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "relay-draft-" + tc.name + "-" + strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
			transport := &localExecTransport{}
			h, err := New().Create(context.Background(), transport, name, "", "bash -c "+shellquote.Quote(tc.paint+"; sleep 30"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = New().Destroy(context.Background(), transport, h) })
			time.Sleep(400 * time.Millisecond)
			draft, found, err := New().ComposerDraft(context.Background(), transport, h)
			if err != nil || !found || draft != tc.want {
				t.Fatalf("draft=%q found=%v err=%v", draft, found, err)
			}
		})
	}
}
