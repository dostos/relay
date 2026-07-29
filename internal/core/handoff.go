package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
)

// HandoffService launches and coordinates goal-based remote work.
type HandoffService struct {
	Sessions     *SessionService
	Reg          *Registry
	Profiles     *ProfileService
	Persist      ports.Persistence
	Coord        ports.Coord // always-on relayd over SSH
	Viz          ports.Viz   // optional; may be nil or Unavailable
	NewTransport TransportFactory
}

// HandoffOpts configures a launch.
type HandoffOpts struct {
	HostID    string
	RepoRef   string
	RemoteCWD string
	Agent     string // for kind=agent
	Goal      string
	Command   string // for kind=job
	NoPane    bool
	Silence   int
	Name      string
}

// Launch creates a session, starts work, installs events, optionally presents viz, returns binding.
func (h *HandoffService) Launch(ctx context.Context, opts HandoffOpts) (*Binding, *Handoff, error) {
	if opts.HostID == "" {
		return nil, nil, fmt.Errorf("host required")
	}
	profile, err := h.Profiles.Get(ctx, opts.HostID, false)
	if err != nil {
		return nil, nil, err
	}
	silence := opts.Silence
	if silence <= 0 {
		silence = profile.Defaults.SilenceSec
	}
	if silence <= 0 {
		silence = 45
	}

	kind := KindJob
	var launchCmd string
	agentName := opts.Agent
	if opts.Command != "" && opts.Goal == "" && opts.Agent == "" {
		kind = KindJob
		launchCmd = opts.Command
	} else {
		kind = KindAgent
		if opts.Goal == "" {
			return nil, nil, fmt.Errorf("--goal required for agent handoff")
		}
		ag, err := profile.FindAgent(agentName)
		if err != nil {
			return nil, nil, err
		}
		agentName = ag.Name
		launchCmd = ag.LaunchCommand(opts.Goal)
	}

	// Holding shell first so we can install events before the real work starts.
	sess, err := h.Sessions.Create(ctx, CreateOpts{
		HostID:    opts.HostID,
		Name:      opts.Name,
		RepoRef:   opts.RepoRef,
		RemoteCWD: opts.RemoteCWD,
		Command:   "bash -l",
		Labels:    map[string]string{"role": "handoff"},
	})
	if err != nil {
		return nil, nil, err
	}

	t, err := h.NewTransport(opts.HostID)
	if err != nil {
		return nil, nil, err
	}
	if h.Coord == nil {
		return nil, nil, fmt.Errorf("coord adapter not configured")
	}
	if err := h.Coord.Ensure(ctx, t); err != nil {
		return nil, nil, err
	}
	emitFactory := func(kind string) (string, error) {
		return h.Coord.SensorCommand(sess.Persist.Name, kind)
	}
	if err := h.Persist.InstallSensors(ctx, t, sess.Persist, silence, emitFactory); err != nil {
		return nil, nil, fmt.Errorf("install sensors: %w", err)
	}

	eventsPath := h.Coord.EventsPath(sess.Persist.Name)
	hid := newID("ho")
	now := time.Now().UTC()
	ho := &Handoff{
		ID:         hid,
		SessionID:  sess.ID,
		HostID:     opts.HostID,
		Kind:       kind,
		Status:     StatusPending,
		Goal:       opts.Goal,
		Agent:      agentName,
		Command:    launchCmd,
		EventsPath: eventsPath,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := h.Reg.PutHandoff(ho); err != nil {
		return nil, nil, err
	}
	_ = AppendLedger(map[string]any{
		"v": 1, "type": "start", "ts": now.Format(time.RFC3339),
		"handoff_id": hid, "session_id": sess.ID, "host_id": opts.HostID,
		"kind": string(kind), "goal": opts.Goal, "agent": agentName, "command": launchCmd,
	})

	// Start work: send launch command into the holding shell.
	if err := h.Persist.Send(ctx, t, sess.Persist, launchCmd, true); err != nil {
		return nil, nil, err
	}
	ho.Status = StatusRunning
	_ = h.Reg.PutHandoff(ho)

	// Emit started via relayd (authoritative seq).
	if _, err := h.Coord.Emit(ctx, t, sess.Persist.Name, "started", nil); err != nil {
		return nil, nil, fmt.Errorf("emit started: %w", err)
	}

	// Inject goal for agent mode after a short readiness wait.
	if kind == KindAgent && opts.Goal != "" {
		_ = waitReady(ctx, h.Persist, t, sess.Persist, 20*time.Second)
		_ = h.Persist.Send(ctx, t, sess.Persist, opts.Goal, true)
	}

	pane := false
	if !opts.NoPane && h.Viz != nil && h.Viz.Available(ctx) {
		// Restorable argv: `relay resume --session <persist>` (cmux Vault extracts --session).
		launch := ResumeLaunchCmd(sess.Persist.Name)
		ref, err := h.Viz.Present(ctx, sess.ID, launch, ports.Layout{Mode: "remote"})
		if err == nil {
			pane = true
			sess.VizSurfaceRef = ref
			_ = h.Sessions.Reg.PutSession(sess)
			RememberResume(sess)
			RememberPane(ref, sess, true)
			labels := map[string]string{sess.ID: ProjectLabel(sess.Persist.Name)}
			if all, err := h.Sessions.List(); err == nil {
				for _, s := range all {
					labels[s.ID] = ProjectLabel(s.Persist.Name)
				}
			}
			_ = h.Viz.BrandLabels(ctx, labels)
		}
	}

	b := &Binding{
		V:         1,
		HandoffID: hid,
		SessionID: sess.ID,
		HostID:    opts.HostID,
		Kind:      string(kind),
		Goal:      opts.Goal,
		Events:    eventsPath,
		Watch:     fmt.Sprintf("relay agent wait --handoff %s", hid),
		Pane:      pane,
	}
	return b, ho, nil
}

func waitReady(ctx context.Context, p ports.Persistence, t ports.Transport, h ports.PersistHandle, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	stable := 0
	attempts := 0
	const maxAttempts = 8 // ~2.5s * 8 with ControlMaster — no SSH storm
	for time.Now().Before(deadline) && attempts < maxAttempts {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		attempts++
		text, err := p.Capture(ctx, t, h, 30)
		if err == nil {
			if text == last && text != "" {
				stable++
				if stable >= 2 {
					return nil
				}
			} else {
				stable = 0
				last = text
			}
		}
		time.Sleep(2500 * time.Millisecond)
	}
	return nil
}

// TailEvents streams events via Coord (relayd). follow mode uses one SSH stream;
// on drop it backs off (max 6 attempts / 10 min / host) — no reconnect storms.
func (h *HandoffService) TailEvents(ctx context.Context, handoffID string, fromSeq int64, follow bool, w io.Writer) error {
	ho, err := h.Reg.GetHandoff(handoffID)
	if err != nil {
		return err
	}
	sess, err := h.Sessions.Get(ho.SessionID)
	if err != nil {
		return err
	}
	if h.Coord == nil {
		return fmt.Errorf("coord adapter not configured")
	}
	t, err := h.NewTransport(ho.HostID)
	if err != nil {
		return err
	}
	cursor := fromSeq
	attempts := 0
	windowStart := time.Now()
	for {
		pr, pw := io.Pipe()
		errCh := make(chan error, 1)
		go func() {
			errCh <- h.Coord.Subscribe(ctx, t, sess.Persist.Name, cursor, follow, pw)
			_ = pw.Close()
		}()
		sc := bufio.NewScanner(pr)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			if ev.Kind == "heartbeat" {
				if _, err := fmt.Fprintln(w, line); err != nil {
					_ = pr.Close()
					return err
				}
				continue
			}
			if ev.Seq <= cursor {
				continue
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				_ = pr.Close()
				return err
			}
			cursor = ev.Seq
			ho.LastSeq = ev.Seq
			// Do not mark terminal StatusDone here — that breaks reconcile/finalize.
			switch ev.Kind {
			case "needs_input":
				ho.Status = StatusNeedsInput
			case "idle":
				if ho.Kind == KindAgent {
					ho.Status = StatusNeedsInput
				}
			case "started":
				ho.Status = StatusRunning
			case "exit":
				if ho.Status != StatusDone && ho.Status != StatusFailed && ho.Status != StatusAbandoned {
					ho.Status = StatusRunning // still needs finalize for outcome/teardown
				}
			}
			_ = h.Reg.PutHandoff(ho)
		}
		_ = pr.Close()
		subErr := <-errCh
		if !follow {
			return subErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// IT-safe reconnect: exponential backoff + hard rate limit
		if time.Since(windowStart) > 10*time.Minute {
			attempts = 0
			windowStart = time.Now()
		}
		attempts++
		if attempts > 6 {
			return fmt.Errorf("subscribe reconnect limit (6/10min) on %s — not retrying (IPS safety); last error: %v", ho.HostID, subErr)
		}
		exp := attempts - 1
		if exp > 5 {
			exp = 5
		}
		delay := time.Duration(1<<uint(exp)) * time.Second
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		delay += time.Duration(time.Now().UnixNano()%500) * time.Millisecond
		fmt.Fprintf(os.Stderr, "relay: reconnecting subscribe on %s (%d/6) in %s…\n", ho.HostID, attempts, delay.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// ReinstallSensors refreshes tmux idle/exit hooks for a session (quiet emit).
func (h *HandoffService) ReinstallSensors(ctx context.Context, sessionID string, silence int) error {
	if h.Coord == nil {
		return fmt.Errorf("coord adapter not configured")
	}
	sess, err := h.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	if silence <= 0 {
		if p, err := h.Profiles.Get(ctx, sess.HostID, true); err == nil && p.Defaults.SilenceSec > 0 {
			silence = p.Defaults.SilenceSec
		}
	}
	if silence <= 0 {
		silence = 45
	}
	t, err := h.NewTransport(sess.HostID)
	if err != nil {
		return err
	}
	if err := h.Coord.Ensure(ctx, t); err != nil {
		return err
	}
	emitFactory := func(kind string) (string, error) {
		return h.Coord.SensorCommand(sess.Persist.Name, kind)
	}
	return h.Persist.InstallSensors(ctx, t, sess.Persist, silence, emitFactory)
}

// EmitEvent emits a coordination event for a handoff's persist session.
func (h *HandoffService) EmitEvent(ctx context.Context, handoffID, kind string, meta map[string]any) (int64, error) {
	ho, err := h.Reg.GetHandoff(handoffID)
	if err != nil {
		return 0, err
	}
	sess, err := h.Sessions.Get(ho.SessionID)
	if err != nil {
		return 0, err
	}
	t, err := h.NewTransport(ho.HostID)
	if err != nil {
		return 0, err
	}
	return h.Coord.Emit(ctx, t, sess.Persist.Name, kind, meta)
}

// FinalizeOutcome closes a handoff lifecycle.
type FinalizeOutcome string

const (
	OutcomeDone      FinalizeOutcome = "done"
	OutcomeFailed    FinalizeOutcome = "failed"
	OutcomeAbandoned FinalizeOutcome = "abandoned"
)

// Finalize tears down remote work (unless keepSession) and writes end ledger.
func (h *HandoffService) Finalize(ctx context.Context, handoffID string, outcome FinalizeOutcome, keepSession bool) (*Handoff, error) {
	ho, err := h.Reg.GetHandoff(handoffID)
	if err != nil {
		return nil, err
	}
	if ho.Outcome != "" && (ho.Status == StatusDone || ho.Status == StatusFailed || ho.Status == StatusAbandoned) {
		return ho, nil
	}
	sess, err := h.Sessions.Get(ho.SessionID)
	if err != nil {
		// Already terminal from events but session registry cleared — still stamp outcome.
		if ho.Status == StatusDone || ho.Status == StatusFailed || ho.Status == StatusAbandoned {
			if outcome == "" {
				outcome = OutcomeDone
			}
			ho.Outcome = string(outcome)
			now := time.Now().UTC()
			ho.EndedAt = &now
			_ = h.Reg.PutHandoff(ho)
			return ho, nil
		}
		return nil, err
	}
	t, err := h.NewTransport(ho.HostID)
	if err != nil {
		return nil, err
	}
	dead, code, _ := h.Persist.DeadStatus(ctx, t, sess.Persist)
	if !dead && outcome == "" {
		return nil, fmt.Errorf("session still live; pass --outcome done|failed|abandoned to force finalize")
	}
	if outcome == "" {
		if code != 0 {
			outcome = OutcomeFailed
		} else {
			outcome = OutcomeDone
		}
	}
	ho.ExitCode = &code
	ho.Outcome = string(outcome)
	switch outcome {
	case OutcomeFailed:
		ho.Status = StatusFailed
	case OutcomeAbandoned:
		ho.Status = StatusAbandoned
	default:
		ho.Status = StatusDone
	}
	now := time.Now().UTC()
	ho.EndedAt = &now
	_ = h.Reg.PutHandoff(ho)
	_ = AppendLedger(map[string]any{
		"v": 1, "type": "end", "ts": now.Format(time.RFC3339),
		"handoff_id": ho.ID, "session_id": sess.ID, "host_id": ho.HostID,
		"outcome": string(outcome), "exit_code": code,
	})
	if !keepSession {
		_ = h.Sessions.Destroy(ctx, sess.ID, false)
	}
	// Viz close is intentionally NOT done here: the interactive `agent done`
	// path closes the pane itself (honoring --keep-viz), and doing it here too
	// would ignore that flag. Non-interactive callers (Reconcile) close the
	// pane explicitly. Close itself is now robust (all duplicate surfaces +
	// pane-state files) thanks to the cmux adapter fix.
	return ho, nil
}

// closePaneFor closes the presented pane(s) for a finished session, best-effort.
func (h *HandoffService) closePaneFor(ctx context.Context, sessionID string) {
	if h.Viz != nil && h.Viz.Available(ctx) {
		_ = h.Viz.Close(ctx, sessionID)
	}
}

// ReapResult reports what a stale-registry reap did.
type ReapResult struct {
	Reaped    []string `json:"reaped"`     // remote tmux confirmed gone → cleaned + panes closed
	KeptAlive []string `json:"kept_alive"` // remote tmux confirmed present
	Skipped   []string `json:"skipped"`    // host unreachable → left untouched
	DryRun    bool     `json:"dry_run"`
}

// ReapDead reconciles the resume registry against reality. It probes each
// live/disconnected entry's host (storm-safe, one tmux list per host); any entry
// whose remote tmux is confirmed gone is reaped: its local session rows are
// dropped, its presented pane + pane-state files are closed/removed, and it is
// marked cleaned so a later resume refuses cleanly instead of hanging on a dead
// attach. Unreachable hosts are skipped (never guessed). With dryRun set it only
// reports what it would do.
func (h *HandoffService) ReapDead(ctx context.Context, dryRun bool) (ReapResult, error) {
	rows, err := h.Sessions.ListResumeStatus()
	if err != nil {
		return ReapResult{}, err
	}
	var candidates []ResumeInfo
	for _, r := range rows {
		if r.Presence == PresenceLive || r.Presence == PresenceDisconnected {
			candidates = append(candidates, r)
		}
	}
	live := h.Sessions.ProbeRemoteTmux(ctx, candidates)
	res := ReapResult{DryRun: dryRun}
	for _, r := range candidates {
		if !live.HostReached[r.HostID] {
			res.Skipped = append(res.Skipped, r.PersistName)
			continue
		}
		if live.Alive[r.PersistName] {
			res.KeptAlive = append(res.KeptAlive, r.PersistName)
			continue
		}
		res.Reaped = append(res.Reaped, r.PersistName)
		if dryRun {
			continue
		}
		if all, e := h.Reg.ListSessions(); e == nil {
			for _, sx := range all {
				if sx.Persist.Name != r.PersistName {
					continue
				}
				if h.Viz != nil && h.Viz.Available(ctx) {
					_ = h.Viz.Close(ctx, sx.ID)
				}
				_ = h.Reg.DeleteSession(sx.ID)
			}
		}
		MarkResumeCleaned(r.PersistName, "reaped: remote tmux absent")
		RemovePaneBindingsForPersist(r.PersistName)
	}
	return res, nil
}

// Reconcile finalizes open handoffs whose remote persist handle is dead.
func (h *HandoffService) Reconcile(ctx context.Context) (int, error) {
	list, err := h.Reg.ListHandoffs()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ho := range list {
		// Key off Outcome — Status alone may be set by the event tail.
		if ho.Outcome != "" {
			continue
		}
		sess, err := h.Sessions.Get(ho.SessionID)
		if err != nil {
			continue
		}
		t, err := h.NewTransport(ho.HostID)
		if err != nil {
			continue
		}
		dead, _, err := h.Persist.DeadStatus(ctx, t, sess.Persist)
		if err != nil || !dead {
			continue
		}
		if _, err := h.Finalize(ctx, ho.ID, "", false); err == nil {
			// Auto-reconciled handoffs have no interactive `done` to close the
			// pane, so close it here (idempotent).
			h.closePaneFor(ctx, ho.SessionID)
			n++
		}
	}
	return n, nil
}
