package core

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/ports"
)

// shellTransport runs commands with a local bash whose HOME is a temp dir,
// standing in for the target host. drop() makes calls fail like a lost SSH
// connection: dropBefore fails without running the command, dropAfter runs it
// on the "host" and then loses the reply.
type shellTransport struct {
	home       string
	dropBefore func(cmd string) bool
	dropAfter  func(cmd string) bool
	calls      int
}

var errConnLost = errors.New("ssh: Connection to host closed by remote host")

func (s *shellTransport) ID() string { return "h-test" }
func (s *shellTransport) Run(ctx context.Context, cwd, command string) (string, string, error) {
	s.calls++
	if s.dropBefore != nil && s.dropBefore(command) {
		return "", "", errConnLost
	}
	if cwd != "" {
		command = "cd " + cwd + " && " + command
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Env = append(os.Environ(), "HOME="+s.home, "SHELL=/bin/bash")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if s.dropAfter != nil && s.dropAfter(command) {
		return "", "", errConnLost
	}
	return stdout.String(), stderr.String(), err
}
func (s *shellTransport) RunStream(context.Context, string, string, io.Writer) error { return nil }
func (s *shellTransport) ReadFile(context.Context, string) ([]byte, error)           { return nil, nil }
func (s *shellTransport) WriteFile(context.Context, string, []byte, string) error    { return nil }
func (s *shellTransport) Interactive(context.Context, string) error                  { return nil }

func opsFixture(t *testing.T) (*SessionService, *shellTransport, string) {
	t.Helper()
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	reg := &Registry{}
	work := t.TempDir()
	if err := reg.PutSession(&Session{ID: "sess-ops", HostID: "h-test", RemoteCWD: work,
		Persist: ports.PersistHandle{Kind: "tmux", Name: "ops"}}); err != nil {
		t.Fatal(err)
	}
	tr := &shellTransport{home: t.TempDir()}
	svc := &SessionService{Reg: reg, NewTransport: func(string) (ports.Transport, error) { return tr, nil }}
	return svc, tr, work
}

// runs counts how many times the op body actually executed on the host.
func runs(t *testing.T, work string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(work, "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

const countedCmd = `printf x >> runs; echo hello; echo oops >&2; sleep 0.3; exit 3`

func waitTerminal(t *testing.T, svc *SessionService, op string) OpReceipt {
	t.Helper()
	rc, err := svc.WaitOp(context.Background(), "sess-ops", op, opCursorRunning, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Terminal {
		t.Fatalf("not terminal: %+v", rc)
	}
	return rc
}

func TestOpLifecycleCursorAndBoundedOutput(t *testing.T) {
	svc, _, work := opsFixture(t)
	op := NewOpID()
	rc, err := svc.StartOp(context.Background(), "sess-ops", op, countedCmd)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Existing || (rc.State != OpRunning && rc.State != OpExited) || rc.Cursor < opCursorRunning {
		t.Fatalf("start receipt = %+v", rc)
	}
	st, err := svc.OpStatus(context.Background(), "sess-ops", op, rc.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if st.State == OpRunning && (st.Changed == nil || *st.Changed) {
		t.Fatalf("unchanged status must say changed=false: %+v", st)
	}
	end := waitTerminal(t, svc, op)
	if end.State != OpExited || end.ExitCode == nil || *end.ExitCode != 3 || end.Cursor != opCursorTerminal {
		t.Fatalf("terminal receipt = %+v", end)
	}
	if end.StdoutBytes != int64(len("hello\n")) || end.StderrBytes != int64(len("oops\n")) {
		t.Fatalf("sizes = %+v", end)
	}
	// Waiting again from the terminal cursor returns at once, unchanged.
	again, err := svc.WaitOp(context.Background(), "sess-ops", op, end.Cursor, time.Minute)
	if err != nil || !again.Terminal || again.Changed == nil || *again.Changed {
		t.Fatalf("re-wait = %+v err=%v", again, err)
	}
	out, err := svc.ReadOpOutput(context.Background(), "sess-ops", op, "stdout", 0, 3)
	if err != nil || out.Data != "hel" || out.NextOffset != 3 || out.EOF {
		t.Fatalf("bounded read = %+v err=%v", out, err)
	}
	out, err = svc.ReadOpOutput(context.Background(), "sess-ops", op, "stdout", out.NextOffset, 0)
	if err != nil || out.Data != "lo\n" || !out.EOF {
		t.Fatalf("continued read = %+v err=%v", out, err)
	}
	if runs(t, work) != 1 {
		t.Fatalf("op body ran %d times", runs(t, work))
	}
}

func TestOpTransportLossBeforeStartAckDoesNotRunTwice(t *testing.T) {
	t.Run("reply lost after the host launched", func(t *testing.T) {
		svc, tr, work := opsFixture(t)
		op := NewOpID()
		tr.dropAfter = func(cmd string) bool { return strings.Contains(cmd, "launch.lock") }
		if _, err := svc.StartOp(context.Background(), "sess-ops", op, countedCmd); err == nil {
			t.Fatal("lost ack must be an error")
		}
		tr.dropAfter = nil
		rc, err := svc.StartOp(context.Background(), "sess-ops", op, countedCmd)
		if err != nil {
			t.Fatal(err)
		}
		if !rc.Existing {
			t.Fatalf("retried start must find the launched op: %+v", rc)
		}
		waitTerminal(t, svc, op)
		if n := runs(t, work); n != 1 {
			t.Fatalf("op body ran %d times", n)
		}
	})
	t.Run("connection lost before the host saw it", func(t *testing.T) {
		svc, tr, work := opsFixture(t)
		op := NewOpID()
		tr.dropBefore = func(string) bool { return true }
		if _, err := svc.StartOp(context.Background(), "sess-ops", op, countedCmd); err == nil {
			t.Fatal("expected error")
		}
		tr.dropBefore = nil
		if _, err := svc.OpStatus(context.Background(), "sess-ops", op, -1); err == nil {
			t.Fatal("op should be absent before any start reached the host")
		}
		rc, err := svc.StartOp(context.Background(), "sess-ops", op, countedCmd)
		if err != nil || rc.Existing {
			t.Fatalf("retry should launch: %+v %v", rc, err)
		}
		waitTerminal(t, svc, op)
		if n := runs(t, work); n != 1 {
			t.Fatalf("op body ran %d times", n)
		}
	})
}

func TestOpSurvivesTransportLossAfterStartAndDuringWait(t *testing.T) {
	svc, tr, work := opsFixture(t)
	op := NewOpID()
	rc, err := svc.StartOp(context.Background(), "sess-ops", op, `printf x >> runs; sleep 1; echo done`)
	if err != nil {
		t.Fatal(err)
	}
	// After remote start: the connection drops for status...
	tr.dropBefore = func(string) bool { return true }
	if _, err := svc.OpStatus(context.Background(), "sess-ops", op, rc.Cursor); err == nil {
		t.Fatal("expected transport error")
	}
	// ...and during a blocking wait (the host side ran, the reply was lost).
	tr.dropBefore = nil
	tr.dropAfter = func(cmd string) bool { return strings.Contains(cmd, "since=") }
	got, err := svc.WaitOp(context.Background(), "sess-ops", op, rc.Cursor, 5*time.Second)
	if err == nil {
		t.Fatal("expected wait to report the lost connection")
	}
	if got.Cursor != rc.Cursor || !strings.Contains(got.Error, "wait again") {
		t.Fatalf("interrupted wait must hand back the cursor: %+v", got)
	}
	// A fresh connection waits on the same op; nothing restarts.
	tr.dropAfter = nil
	end := waitTerminal(t, svc, op)
	if end.State != OpExited || *end.ExitCode != 0 {
		t.Fatalf("terminal = %+v", end)
	}
	if n := runs(t, work); n != 1 {
		t.Fatalf("op body ran %d times", n)
	}
}

func TestOpStartAfterTerminalReturnsReceiptWithoutRerun(t *testing.T) {
	svc, _, work := opsFixture(t)
	op := NewOpID()
	if _, err := svc.StartOp(context.Background(), "sess-ops", op, countedCmd); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, svc, op)
	rc, err := svc.StartOp(context.Background(), "sess-ops", op, countedCmd)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Existing || rc.State != OpExited || *rc.ExitCode != 3 {
		t.Fatalf("retry after terminal = %+v", rc)
	}
	if n := runs(t, work); n != 1 {
		t.Fatalf("op body ran %d times", n)
	}
}

func TestOpLostWhenRunnerDiesWithoutExit(t *testing.T) {
	svc, tr, _ := opsFixture(t)
	op := NewOpID()
	rc, err := svc.StartOp(context.Background(), "sess-ops", op, `sleep 30`)
	if err != nil {
		t.Fatal(err)
	}
	// Kill the runner and its child as a reboot would, before it records exit.
	dir := filepath.Join(tr.home, ".local/state/relay/ops/sess-ops", op)
	if _, _, err := tr.Run(context.Background(), "", "kill -9 -$(cat "+dir+"/pid) 2>/dev/null || kill -9 $(cat "+dir+"/pid)"); err != nil {
		t.Fatal(err)
	}
	end, err := svc.WaitOp(context.Background(), "sess-ops", op, rc.Cursor, 5*time.Second)
	if err != nil || end.State != OpLost || !end.Terminal {
		t.Fatalf("receipt = %+v err=%v", end, err)
	}
}

func TestOpLaunchClaimWithoutRunnerAgesToLost(t *testing.T) {
	sess := &Session{ID: "s", HostID: "h"}
	kv := map[string]string{"created": "1000", "now": "1005", "locked": "1"}
	if rc := opReceiptFromKV(sess, "op-1", kv); rc.State != OpStarting || rc.Terminal {
		t.Fatalf("young claim = %+v", rc)
	}
	kv["now"] = "2000"
	if rc := opReceiptFromKV(sess, "op-1", kv); rc.State != OpLost || !rc.Terminal {
		t.Fatalf("stale claim = %+v", rc)
	}
}

func TestOpRejectsUnsafeIDs(t *testing.T) {
	svc, _, _ := opsFixture(t)
	for _, id := range []string{"", "../x", "a b", "op;rm", ".hidden"} {
		if _, err := svc.StartOp(context.Background(), "sess-ops", id, "true"); err == nil {
			t.Fatalf("op id %q accepted", id)
		}
	}
}
