package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// Durable operations (#60).
//
// `relay session exec --detach` starts the command on the target host under
// a runner that is detached from the SSH connection (setsid/nohup) and keeps
// all state in files next to it:
//
//	~/.local/state/relay/ops/<session-id>/<op-id>/
//	  cmd  created  launch.lock/  pid  started  stdout  stderr  ended  exit
//
// Every status read derives the state from those files, so any later
// connection — after an SSH drop, from another relay process — can query or
// wait on the same operation without starting it again. Starting is itself
// idempotent per op id: the launch is claimed with mkdir, so a retry of a
// start whose acknowledgement was lost returns the existing operation.

// OpState is the lifecycle state of a detached operation.
type OpState string

const (
	OpStarting OpState = "starting" // claimed, runner not yet reported its pid
	OpRunning  OpState = "running"
	OpExited   OpState = "exited" // terminal; exit_code is set
	OpLost     OpState = "lost"   // terminal; runner vanished without an exit code
	OpAbsent   OpState = "absent" // no such operation on the host
)

// Cursor values are monotonic per operation: 0 starting, 1 running,
// 2 terminal. A caller passes the last cursor it saw; anything greater is a
// change.
const (
	opCursorStarting int64 = 0
	opCursorRunning  int64 = 1
	opCursorTerminal int64 = 2
)

// opLaunchGrace is how long a claimed op may lack a runner pid before it is
// reported lost (the launching shell died with its connection).
var opLaunchGrace = 30 * time.Second

// opWaitChunk bounds one remote blocking poll; wait loops over chunks so a
// single dropped connection costs at most one chunk.
var opWaitChunk = 20 * time.Second

// OpReceipt is the compact, structured status of an operation. It never
// carries output unless explicitly requested via OpOutput.
type OpReceipt struct {
	OpID        string     `json:"op_id"`
	SessionID   string     `json:"session_id"`
	HostID      string     `json:"host_id"`
	State       OpState    `json:"state"`
	Cursor      int64      `json:"cursor"`
	Terminal    bool       `json:"terminal"`
	Changed     *bool      `json:"changed,omitempty"`
	Existing    bool       `json:"existing,omitempty"` // start found the op already launched
	ExitCode    *int       `json:"exit_code,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	StdoutBytes int64      `json:"stdout_bytes"`
	StderrBytes int64      `json:"stderr_bytes"`
	Error       string     `json:"error,omitempty"`
}

// NewOpID returns a fresh operation id. Callers that may need to retry a
// start should generate (or choose) the id before the first attempt.
func NewOpID() string { return newID("op") }

func opDir(sessionID, opID string) string {
	return remoteStateRoot + "/ops/" + shellquote.Quote(sessionID) + "/" + shellquote.Quote(opID)
}

// opStatusScript prints key=value lines describing $d. Liveness is checked
// before the exit file is read so a runner that exits in between is seen as
// exited, never lost.
const opStatusScript = `
if [ ! -d "$d" ]; then echo state=absent; exit 0; fi
pid=$(cat "$d/pid" 2>/dev/null || true)
alive=0
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then alive=1; fi
echo "alive=$alive"
echo "pid=$pid"
for f in created started ended exit; do
  if [ -f "$d/$f" ]; then printf '%s=%s\n' "$f" "$(cat "$d/$f")"; fi
done
if [ -d "$d/launch.lock" ]; then echo locked=1; fi
echo "now=$(date +%s)"
echo "stdout_bytes=$(wc -c < "$d/stdout" 2>/dev/null | tr -d ' ' || echo 0)"
echo "stderr_bytes=$(wc -c < "$d/stderr" 2>/dev/null | tr -d ' ' || echo 0)"
`

// opRunner runs detached; $1 is the op dir. It records its own pid so the
// launching shell may die (SSH drop) without losing the handle.
const opRunner = `d=$1
echo $$ > "$d/pid.tmp" && mv "$d/pid.tmp" "$d/pid"
date +%s > "$d/started"
"${SHELL:-/bin/sh}" -c "$(cat "$d/cmd")" > "$d/stdout" 2> "$d/stderr" < /dev/null
rc=$?
date +%s > "$d/ended"
printf '%s\n' "$rc" > "$d/exit.tmp" && mv "$d/exit.tmp" "$d/exit"
`

func (s *SessionService) opTarget(sessionID, opID string) (*Session, ports.Transport, string, error) {
	sess, err := s.Reg.GetSession(sessionID)
	if err != nil {
		return nil, nil, "", err
	}
	if err := validateOpToken("session id", sess.ID); err != nil {
		return nil, nil, "", err
	}
	if err := validateOpToken("op id", opID); err != nil {
		return nil, nil, "", err
	}
	t, err := s.transportFor(sess)
	if err != nil {
		return nil, nil, "", err
	}
	return sess, t, opDir(sess.ID, opID), nil
}

// StartOp launches command as durable operation opID (generate one with
// NewOpID). If the op already exists on the host — a retried start whose
// acknowledgement was lost — it is not launched again; its current receipt is
// returned with Existing set.
func (s *SessionService) StartOp(ctx context.Context, sessionID, opID, command string) (OpReceipt, error) {
	rc := OpReceipt{OpID: opID, SessionID: sessionID, State: OpAbsent}
	if strings.TrimSpace(command) == "" {
		return rc, fmt.Errorf("command required")
	}
	sess, t, d, err := s.opTarget(sessionID, opID)
	if err != nil {
		return rc, err
	}
	rc.HostID = sess.HostID
	cwd, cmd, err := s.execPlan(sess, command)
	if err != nil {
		return rc, err
	}
	script := fmt.Sprintf(`d=%s
mkdir -p "$d" || exit 1
if mkdir "$d/launch.lock" 2>/dev/null; then
  printf '%%s' %s > "$d/cmd"
  date +%%s > "$d/created"
  if command -v setsid >/dev/null 2>&1; then
    setsid nohup bash -c %s relay-op "$d" > /dev/null 2>&1 < /dev/null &
  else
    nohup bash -c %s relay-op "$d" > /dev/null 2>&1 < /dev/null &
  fi
  i=0
  while [ ! -f "$d/pid" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
  echo launch=new
else
  echo launch=existing
fi
%s`, d, shellquote.Quote(cmd), shellquote.Quote(opRunner), shellquote.Quote(opRunner), opStatusScript)
	out, stderr, err := t.Run(ctx, cwd, script)
	if err != nil {
		rc.Error = fmt.Sprintf("start not acknowledged: %v (%s); the op may be running — query it, or retry the start with the same op id", err, strings.TrimSpace(stderr))
		return rc, fmt.Errorf("%s", rc.Error)
	}
	kv := parseKV(out)
	rc = opReceiptFromKV(sess, opID, kv)
	rc.Existing = kv["launch"] == "existing"
	return rc, nil
}

// OpStatus reads the current receipt. With since >= 0, Changed reports
// whether Cursor moved past since.
func (s *SessionService) OpStatus(ctx context.Context, sessionID, opID string, since int64) (OpReceipt, error) {
	sess, t, d, err := s.opTarget(sessionID, opID)
	if err != nil {
		return OpReceipt{OpID: opID, SessionID: sessionID}, err
	}
	out, stderr, err := t.Run(ctx, "", "d="+d+"\n"+opStatusScript)
	if err != nil {
		return OpReceipt{OpID: opID, SessionID: sessionID, HostID: sess.HostID, Cursor: since},
			fmt.Errorf("op status: %w (%s)", err, strings.TrimSpace(stderr))
	}
	rc := opReceiptFromKV(sess, opID, parseKV(out))
	if since >= 0 {
		changed := rc.Cursor > since
		rc.Changed = &changed
	}
	if rc.State == OpAbsent {
		return rc, fmt.Errorf("no operation %s on session %s", opID, sessionID)
	}
	return rc, nil
}

// WaitOp blocks until the op's cursor moves past since or the op is terminal,
// or timeout elapses (then Changed=false and the unchanged receipt is
// returned). Blocking happens on the host in bounded chunks; a transport error
// returns immediately with the last known cursor so the caller can wait again
// on the same op — nothing is restarted.
func (s *SessionService) WaitOp(ctx context.Context, sessionID, opID string, since int64, timeout time.Duration) (OpReceipt, error) {
	sess, t, d, err := s.opTarget(sessionID, opID)
	if err != nil {
		return OpReceipt{OpID: opID, SessionID: sessionID}, err
	}
	deadline := time.Now().Add(timeout)
	last := OpReceipt{OpID: opID, SessionID: sessionID, HostID: sess.HostID, Cursor: since}
	for {
		chunk := time.Until(deadline)
		if chunk > opWaitChunk {
			chunk = opWaitChunk
		}
		secs := int(chunk / time.Second)
		if secs < 0 {
			secs = 0
		}
		script := fmt.Sprintf(`d=%s
since=%d
end=$(( $(date +%%s) + %d ))
while [ -d "$d" ] && [ ! -f "$d/exit" ] && [ "$(date +%%s)" -lt "$end" ]; do
  pid=$(cat "$d/pid" 2>/dev/null || true)
  if [ -n "$pid" ]; then
    kill -0 "$pid" 2>/dev/null || break
    [ "$since" -lt 1 ] && break
  fi
  sleep 0.2
done
%s`, d, since, secs, opStatusScript)
		out, stderr, err := t.Run(ctx, "", script)
		if err != nil {
			last.Error = fmt.Sprintf("wait interrupted: %v (%s); wait again with --cursor %d", err, strings.TrimSpace(stderr), since)
			return last, fmt.Errorf("%s", last.Error)
		}
		rc := opReceiptFromKV(sess, opID, parseKV(out))
		if rc.State == OpAbsent {
			return rc, fmt.Errorf("no operation %s on session %s", opID, sessionID)
		}
		changed := rc.Cursor > since
		rc.Changed = &changed
		last = rc
		if changed || rc.Terminal || !time.Now().Before(deadline) {
			return rc, nil
		}
		select {
		case <-ctx.Done():
			return rc, ctx.Err()
		default:
		}
	}
}

// OpOutput is an explicit, bounded read of an operation's output stream.
type OpOutput struct {
	OpID       string `json:"op_id"`
	Stream     string `json:"stream"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	Size       int64  `json:"size"`
	EOF        bool   `json:"eof"`
	Data       string `json:"data"`
}

// DefaultOpOutputLimit bounds one output read.
const DefaultOpOutputLimit = 16 * 1024

// ReadOpOutput returns up to limit bytes of stream ("stdout"/"stderr")
// starting at offset. NextOffset is the cursor for the following read.
func (s *SessionService) ReadOpOutput(ctx context.Context, sessionID, opID, stream string, offset, limit int64) (OpOutput, error) {
	res := OpOutput{OpID: opID, Stream: stream, Offset: offset}
	if stream != "stdout" && stream != "stderr" {
		return res, fmt.Errorf("stream must be stdout or stderr")
	}
	if offset < 0 {
		return res, fmt.Errorf("offset must be non-negative")
	}
	if limit <= 0 {
		limit = DefaultOpOutputLimit
	}
	_, t, d, err := s.opTarget(sessionID, opID)
	if err != nil {
		return res, err
	}
	script := fmt.Sprintf(`d=%s
f="$d/%s"
[ -d "$d" ] || { echo relay-op-absent; exit 0; }
size=$(wc -c < "$f" 2>/dev/null | tr -d ' ' || echo 0)
[ -n "$size" ] || size=0
echo "size=$size"
tail -c +%d "$f" 2>/dev/null | head -c %d`, d, stream, offset+1, limit)
	out, stderr, err := t.Run(ctx, "", script)
	if err != nil {
		return res, fmt.Errorf("op output: %w (%s)", err, strings.TrimSpace(stderr))
	}
	if strings.HasPrefix(out, "relay-op-absent") {
		return res, fmt.Errorf("no operation %s on session %s", opID, sessionID)
	}
	head, data, _ := strings.Cut(out, "\n")
	res.Size, _ = strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(head), "size="), 10, 64)
	if offset > res.Size {
		data = ""
	}
	res.Data = data
	res.NextOffset = offset + int64(len(data))
	res.EOF = res.NextOffset >= res.Size
	return res, nil
}

func parseKV(out string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k != "" {
			kv[k] = strings.TrimSpace(v)
		}
	}
	return kv
}

func unixPtr(v string) *time.Time {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	t := time.Unix(n, 0).UTC()
	return &t
}

func opReceiptFromKV(sess *Session, opID string, kv map[string]string) OpReceipt {
	rc := OpReceipt{OpID: opID, SessionID: sess.ID, HostID: sess.HostID}
	if kv["state"] == string(OpAbsent) {
		rc.State = OpAbsent
		return rc
	}
	rc.CreatedAt = unixPtr(kv["created"])
	rc.StartedAt = unixPtr(kv["started"])
	rc.EndedAt = unixPtr(kv["ended"])
	rc.StdoutBytes, _ = strconv.ParseInt(kv["stdout_bytes"], 10, 64)
	rc.StderrBytes, _ = strconv.ParseInt(kv["stderr_bytes"], 10, 64)
	now, _ := strconv.ParseInt(kv["now"], 10, 64)
	created, _ := strconv.ParseInt(kv["created"], 10, 64)
	switch {
	case kv["exit"] != "":
		rc.State = OpExited
		if code, err := strconv.Atoi(kv["exit"]); err == nil {
			rc.ExitCode = &code
		}
	case kv["pid"] != "" && kv["alive"] == "1":
		rc.State = OpRunning
	case kv["pid"] != "":
		rc.State = OpLost
		rc.Error = "runner exited without recording an exit code (killed or host rebooted)"
	case created > 0 && now-created > int64(opLaunchGrace/time.Second):
		rc.State = OpLost
		rc.Error = "launch was claimed but the runner never started (launching connection lost)"
	default:
		rc.State = OpStarting
	}
	switch rc.State {
	case OpRunning:
		rc.Cursor = opCursorRunning
	case OpExited, OpLost:
		rc.Cursor = opCursorTerminal
		rc.Terminal = true
	default:
		rc.Cursor = opCursorStarting
	}
	return rc
}
