package core

import (
	"context"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

type chromePersistence struct {
	renamePersistence
	applied []ports.PersistHandle
}

func (p *chromePersistence) ApplyChrome(_ context.Context, _ ports.Transport, h ports.PersistHandle) error {
	p.applied = append(p.applied, h)
	return nil
}

func TestOpenNamedReappliesChromeToExistingSession(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	sess := &Session{
		ID: "sess-engram", HostID: "c3",
		Persist:   ports.PersistHandle{Kind: "tmux", Name: "engram"},
		CreatedAt: time.Now().UTC(),
	}
	if err := reg.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	persist := &chromePersistence{}
	service := &SessionService{
		Reg: reg, Persist: persist,
		NewTransport: func(string) (ports.Transport, error) {
			return &fakeTransport{id: "c3", outputs: map[string]string{"c3": ""}}, nil
		},
	}

	got, created, err := service.OpenNamed(context.Background(), CreateOpts{HostID: "c3", Name: "engram"})
	if err != nil || created || got.ID != sess.ID {
		t.Fatalf("got=%+v created=%v err=%v", got, created, err)
	}
	if len(persist.applied) != 1 || persist.applied[0].Name != "engram" {
		t.Fatalf("chrome applications=%+v", persist.applied)
	}
}
