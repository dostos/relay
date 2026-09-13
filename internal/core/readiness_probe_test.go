package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

const blockedTrustPane = `Accessing workspace: /home/jingyulee/dev/oqb
   Quick safety check: Is this a project you created or one you trust?
   ❯ 1. Yes, I trust this folder
     2. No, exit
`

const readyPane = "● Working on the flaky eval…\n  reading tests/test_x.py\n"

func TestClassifyTextMirrorsTheClassifierAndNamesEmptyInput(t *testing.T) {
	rep := ClassifyText(blockedTrustPane, 0)
	if rep.State != AgentBlocked || rep.Gate == nil || len(rep.Gate.Choices) != 2 {
		t.Fatalf("blocked pane: %+v", rep)
	}
	if rep.Lines != DefaultReadinessLines || rep.SampledAt.IsZero() {
		t.Fatalf("report must carry lines and a timestamp: %+v", rep)
	}
	if rep := ClassifyText(readyPane, 10); rep.State != AgentReady || rep.Lines != 10 {
		t.Fatalf("ready pane: %+v", rep)
	}
	// No text is "unknown", not "absent": a presenter that read nothing has
	// not seen the pane; an empty pane is a different fact.
	if rep := ClassifyText("", 0); rep.State != AgentUnknown || rep.Reason != "no text" {
		t.Fatalf("empty input: %+v", rep)
	}
	b, err := json.Marshal(ClassifyText(readyPane, 10))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"state":"ready"`, `"lines":10`, `"sampled_at"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("json missing %s: %s", key, b)
		}
	}
	if strings.Contains(string(b), `"gate"`) {
		t.Errorf("no gate must omit the key: %s", b)
	}
}

// scriptedPersist answers Capture per persist name; a name with no script
// fails, which is how a dead host is simulated.
type scriptedPersist struct {
	ports.Persistence
	text  map[string]string
	mu    sync.Mutex
	calls []string
	delay time.Duration
}

func (p *scriptedPersist) Capture(ctx context.Context, _ ports.Transport, h ports.PersistHandle, _ int) (string, error) {
	p.mu.Lock()
	p.calls = append(p.calls, h.Name)
	p.mu.Unlock()
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if s, ok := p.text[h.Name]; ok {
		return s, nil
	}
	return "", errors.New("ssh: connect to host 100.99.82.119 port 7777: Operation timed out")
}

func readinessFixture(t *testing.T, persist ports.Persistence, sessions ...*Session) *SessionService {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	for _, s := range sessions {
		if err := reg.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	return &SessionService{
		Reg:          reg,
		Persist:      persist,
		NewTransport: func(string) (ports.Transport, error) { return nil, nil },
	}
}

func sess(id, host, name string) *Session {
	return &Session{ID: id, HostID: host, Persist: ports.PersistHandle{Kind: "tmux", Name: name}}
}

func TestReadinessCapturesAndClassifiesOneSession(t *testing.T) {
	p := &scriptedPersist{text: map[string]string{"eval": blockedTrustPane}}
	svc := readinessFixture(t, p, sess("sess-1", "hamburg", "eval"))
	s, rep, err := svc.Readiness(context.Background(), "sess-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "sess-1" || rep.State != AgentBlocked || rep.Gate == nil || rep.Lines != DefaultReadinessLines {
		t.Fatalf("got %+v %+v", s, rep)
	}
}

func TestReadinessOnADeadHostIsUnknownWithTheReasonAndAnError(t *testing.T) {
	p := &scriptedPersist{text: map[string]string{}}
	svc := readinessFixture(t, p, sess("sess-1", "hamburg", "eval"))
	s, rep, err := svc.Readiness(context.Background(), "sess-1", 0)
	if err == nil || s == nil || rep == nil {
		t.Fatalf("a failed capture must return the session, a report AND an error: %v %v %v", s, rep, err)
	}
	if rep.State != AgentUnknown || !strings.Contains(rep.Reason, "timed out") {
		t.Fatalf("report: %+v", rep)
	}
	if _, _, err := svc.Readiness(context.Background(), "sess-missing", 0); err == nil {
		t.Fatal("an unknown session id is an error")
	}
}

func TestReadinessIsBoundedByTheTimeout(t *testing.T) {
	// A capture that never answers must not hang the caller past the bound.
	p := &scriptedPersist{text: map[string]string{"eval": readyPane}, delay: 30 * time.Second}
	svc := readinessFixture(t, p, sess("sess-1", "hamburg", "eval"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, rep, err := svc.Readiness(ctx, "sess-1", 0)
	if time.Since(start) > 5*time.Second {
		t.Fatal("readiness ignored the context deadline")
	}
	if err == nil || rep.State != AgentUnknown {
		t.Fatalf("a timed-out capture is unknown: %+v %v", rep, err)
	}
}

func TestListWithReadinessMarksADeadHostUnknownAndClassifiesTheRest(t *testing.T) {
	p := &scriptedPersist{text: map[string]string{"eval": blockedTrustPane, "train": readyPane}}
	svc := readinessFixture(t, p,
		sess("sess-a", "hamburg", "eval"),
		sess("sess-b", "madrid", "train"),
		sess("sess-c", "cancun", "gone"),
	)
	list, err := svc.ListWithReadiness(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("every session must be reported: %d", len(list))
	}
	byID := map[string]SessionReadiness{}
	for _, r := range list {
		byID[r.ID] = r
	}
	if byID["sess-a"].Readiness.State != AgentBlocked || byID["sess-b"].Readiness.State != AgentReady {
		t.Fatalf("live hosts must classify: %+v %+v", byID["sess-a"].Readiness, byID["sess-b"].Readiness)
	}
	if got := byID["sess-c"].Readiness; got.State != AgentUnknown || !strings.Contains(got.Reason, "capture failed") {
		t.Fatalf("a dead host is unknown with its reason, never a failed list: %+v", got)
	}
	// The promoted session fields and the readiness key both appear.
	b, _ := json.Marshal(byID["sess-a"])
	for _, key := range []string{`"id":"sess-a"`, `"host_id":"hamburg"`, `"readiness":{`, `"state":"blocked"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("missing %s in %s", key, b)
		}
	}
}

func TestListWithReadinessProbesOneHostSequentially(t *testing.T) {
	// Two sessions on one host must be captured one after another, never at
	// the same time: the fleet's ssh burst limit is per source, and a host is
	// where the connections land.
	p := &concurrencyPersist{text: readyPane, delay: 30 * time.Millisecond}
	svc := readinessFixture(t, p,
		sess("sess-1", "hamburg", "a"), sess("sess-2", "hamburg", "b"), sess("sess-3", "hamburg", "c"),
		sess("sess-4", "madrid", "d"), sess("sess-5", "madrid", "e"),
		sess("sess-6", "cancun", "f"),
	)
	if _, err := svc.ListWithReadiness(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if p.maxPerHost["hamburg"] != 1 || p.maxPerHost["madrid"] != 1 {
		t.Fatalf("captures on one host overlapped: %v", p.maxPerHost)
	}
	if p.maxHosts > readinessHostFanout {
		t.Fatalf("more than %d hosts probed at once: %d", readinessHostFanout, p.maxHosts)
	}
}

// concurrencyPersist measures how many captures run at once, per host and
// overall. The host is smuggled in through the transport factory: the
// fixture's transport is nil, so the persist name prefix stands in for it.
type concurrencyPersist struct {
	ports.Persistence
	text       string
	delay      time.Duration
	mu         chan struct{}
	perHost    map[string]int
	maxPerHost map[string]int
	hosts      int
	maxHosts   int
	hostOf     map[string]string
	once       sync.Once
}

func (p *concurrencyPersist) init() {
	p.once.Do(func() {
		p.mu = make(chan struct{}, 1)
		p.perHost = map[string]int{}
		p.maxPerHost = map[string]int{}
		p.hostOf = map[string]string{"a": "hamburg", "b": "hamburg", "c": "hamburg", "d": "madrid", "e": "madrid", "f": "cancun"}
	})
}

func (p *concurrencyPersist) Capture(_ context.Context, _ ports.Transport, h ports.PersistHandle, _ int) (string, error) {
	p.init()
	host := p.hostOf[h.Name]
	p.mu <- struct{}{}
	p.perHost[host]++
	if p.perHost[host] > p.maxPerHost[host] {
		p.maxPerHost[host] = p.perHost[host]
	}
	active := 0
	for _, n := range p.perHost {
		if n > 0 {
			active++
		}
	}
	if active > p.maxHosts {
		p.maxHosts = active
	}
	<-p.mu
	time.Sleep(p.delay)
	p.mu <- struct{}{}
	p.perHost[host]--
	<-p.mu
	return p.text, nil
}
