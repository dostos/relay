package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/ui"
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
	ParentRouter ParentEventRouter
}

// HandoffOpts configures a launch.
type HandoffOpts struct {
	HostID            string
	RepoRef           string
	RemoteCWD         string
	Workspace         string // optional cmux workspace ref for the presented pane
	Pane              string // optional source cmux pane ref for relative placement
	ExplicitPlace     bool   // workspace/pane was explicitly selected by the caller
	Agent             string // for kind=agent
	Goal              string
	Command           string // for kind=job
	NoPane            bool
	Silence           int
	Name              string
	Container         string // optional: container name from host.yaml `containers:`
	SourceSessionID   string
	SourceHostID      string
	SourcePersistName string
	RestartedFromID   string
}

func handoffLayout(opts HandoffOpts) ports.Layout {
	return ports.Layout{
		Mode:            "remote",
		Workspace:       opts.Workspace,
		Pane:            opts.Pane,
		SourceSessionID: opts.SourceSessionID,
		ExplicitPlace:   opts.ExplicitPlace,
	}
}

// selectPreferredAgent returns a usage-aware default agent name, or "" when no
// usage hook is configured (so callers preserve the original preferred_agent
// behavior byte-for-byte). See usage.go for the ranking rules.
func (h *HandoffService) selectPreferredAgent(ctx context.Context, profile *HostProfile) string {
	if usageHookFor(profile) == "" {
		return ""
	}
	pick, _ := Suggest(ctx, profile)
	return pick
}

// Launch creates a session, starts work, installs events, optionally presents viz, returns binding.
func (h *HandoffService) Launch(ctx context.Context, opts HandoffOpts) (*Binding, *Handoff, error) {
	now := time.Now().UTC()
	hid := newID("ho")
	kind := KindAgent
	if opts.Command != "" && opts.Goal == "" && opts.Agent == "" {
		kind = KindJob
	}
	ho := &Handoff{
		ID: hid, HostID: opts.HostID, Kind: kind, Status: StatusPending,
		LaunchState: EffectPending, DeliveryState: EffectNotApplicable,
		PresentationState: EffectNotApplicable, Goal: opts.Goal, Agent: opts.Agent,
		Name: opts.Name, RepoRef: opts.RepoRef, RemoteCWD: opts.RemoteCWD,
		Container: opts.Container, NoPane: opts.NoPane, Silence: opts.Silence,
		RestartedFromID: opts.RestartedFromID, CreatedAt: now, UpdatedAt: now,
		SourceSessionID: opts.SourceSessionID, SourceHostID: opts.SourceHostID,
		SourcePersistName: opts.SourcePersistName,
	}
	if kind == KindAgent {
		ho.DeliveryState = EffectPending
	}
	if !opts.NoPane && h.Viz != nil {
		ho.PresentationState = EffectPending
	}
	if h.Reg == nil {
		return nil, ho, fmt.Errorf("persist launch attempt %s: registry unavailable", hid)
	}
	if err := h.Reg.PutHandoff(ho); err != nil {
		return nil, ho, fmt.Errorf("persist launch attempt %s: %w", hid, err)
	}
	fail := func(stage string, sess *Session, cause error) (*Binding, *Handoff, error) {
		return nil, ho, h.failLaunchStage(ctx, ho, sess, stage, cause)
	}
	if opts.HostID == "" {
		return fail("validate", nil, fmt.Errorf("host required"))
	}
	if h.Profiles == nil || h.Sessions == nil || h.Persist == nil || h.NewTransport == nil {
		return fail("adapters", nil, fmt.Errorf("launcher adapters are not fully configured"))
	}
	profile, err := h.Profiles.Get(ctx, opts.HostID, false)
	if err != nil {
		return fail("profile", nil, err)
	}
	var cref *ContainerRef
	if opts.Container != "" {
		cspec, err := profile.ResolveContainer(opts.Container)
		if err != nil {
			return fail("container", nil, err)
		}
		cwd := opts.RemoteCWD
		if cwd == "" {
			cwd = cspec.ResolveCWD(opts.RepoRef)
		}
		cref = &ContainerRef{
			Runtime: cspec.RuntimeVerb(),
			Ref:     cspec.Container,
			CWD:     cwd,
			User:    cspec.User,
		}
	}
	silence := opts.Silence
	if silence <= 0 {
		silence = profile.Defaults.SilenceSec
	}
	if silence <= 0 {
		silence = DefaultSilenceSec
	}

	var launchCmd string
	agentName := opts.Agent
	if opts.Command != "" && opts.Goal == "" && opts.Agent == "" {
		kind = KindJob
		launchCmd = opts.Command
	} else {
		kind = KindAgent
		if opts.Goal == "" {
			return fail("validate", nil, fmt.Errorf("--goal required for agent handoff"))
		}
		if agentName == "" {
			// No explicit --agent: let a configured usage hook steer the
			// default toward the agent with weekly headroom. Absent/failed
			// hook returns "" and FindAgent falls back to preferred_agent.
			if pick := h.selectPreferredAgent(ctx, profile); pick != "" {
				agentName = pick
			}
		}
		ag, err := profile.FindAgent(agentName)
		if err != nil {
			return fail("resolve_agent", nil, err)
		}
		agentName = ag.Name
		launchCmd = ag.LaunchCommand(opts.Goal)
		if cref != nil {
			launchCmd, err = ContainerExec(cref.Runtime, *cref, ag.InnerCommand(), true)
			if err != nil {
				return fail("container", nil, err)
			}
		}
	}
	ho.Kind, ho.Agent, ho.Command = kind, agentName, launchCmd
	ho.Silence = silence
	if err := h.Reg.PutHandoff(ho); err != nil {
		return fail("persist_attempt", nil, err)
	}

	// Allocate the edge id before the target session so session history can name
	// the exact handoff that created it.
	// Holding shell first so we can install events before the real work starts.
	labels := map[string]string{"role": "handoff"}
	if agentName != "" {
		labels["agent"] = agentName
	}
	sess, err := h.Sessions.Create(ctx, CreateOpts{
		HostID:             opts.HostID,
		Name:               opts.Name,
		RepoRef:            opts.RepoRef,
		RemoteCWD:          opts.RemoteCWD,
		Command:            "bash -l",
		Labels:             labels,
		SourceSessionID:    opts.SourceSessionID,
		SourceHostID:       opts.SourceHostID,
		SourcePersistName:  opts.SourcePersistName,
		CreatedByHandoffID: hid,
	})
	if err != nil {
		return fail("create_session", nil, err)
	}
	ho.SessionID, ho.Name, ho.RemoteCWD = sess.ID, sess.Persist.Name, sess.RemoteCWD
	if err := h.Reg.PutHandoff(ho); err != nil {
		return fail("persist_session", sess, err)
	}

	t, err := h.NewTransport(opts.HostID)
	if err != nil {
		return fail("transport", sess, err)
	}
	if cref != nil {
		sess.Container = cref
		if err := h.Sessions.Reg.PutSession(sess); err != nil {
			return fail("persist_container", sess, err)
		}
		agInner := "" // resolved agent inner command for the probe
		if ag, aerr := profile.FindAgent(agentName); aerr == nil {
			agInner = ag.InnerCommand()
		}
		if agInner != "" {
			if verr := h.verifyContainerAgent(ctx, t, *cref, agInner); verr != nil {
				return fail("verify_container_agent", sess, verr)
			}
		}
	}
	if h.Coord == nil {
		return fail("event_stream", sess, fmt.Errorf("coord adapter not configured"))
	}
	if err := h.Coord.Ensure(ctx, t); err != nil {
		return fail("event_stream", sess, err)
	}
	emitFactory := func(kind string) (string, error) {
		return h.Coord.SensorCommand(sess.Persist.Name, kind)
	}
	if err := h.Persist.InstallSensors(ctx, t, sess.Persist, silence, emitFactory); err != nil {
		return fail("install_sensors", sess, fmt.Errorf("install sensors: %w", err))
	}

	eventsPath := h.Coord.EventsPath(sess.Persist.Name)
	ho.EventsPath = eventsPath
	if err := h.Reg.PutHandoff(ho); err != nil {
		return fail("persist_event_stream", sess, err)
	}
	startRecord := map[string]any{
		"v": 1, "type": "start", "ts": now.Format(time.RFC3339),
		"handoff_id": hid, "session_id": sess.ID, "host_id": opts.HostID,
		"kind": string(kind), "goal": opts.Goal, "agent": agentName, "command": launchCmd,
		"source_session_id": opts.SourceSessionID, "source_host_id": opts.SourceHostID,
		"source_persist_name": opts.SourcePersistName,
	}
	if opts.RestartedFromID != "" {
		startRecord["restarted_from_id"] = opts.RestartedFromID
	}
	if err := AppendLedger(startRecord); err != nil {
		return fail("record_start", sess, err)
	}

	// Start work through the holding-shell launch protocol. Agent-composer
	// delivery is a separate contract used only after readiness is established.
	launcher, ok := h.Persist.(ports.HoldingShellLauncher)
	if !ok {
		err := fmt.Errorf("persistence %s cannot acknowledge holding-shell launch", h.Persist.Kind())
		return fail("launch", sess, err)
	}
	if err := launcher.Launch(ctx, t, sess.Persist, launchCmd); err != nil {
		return fail("launch", sess, err)
	}
	ho.LaunchState = EffectAcknowledged
	ho.Status = StatusRunning
	if err := h.Reg.PutHandoff(ho); err != nil {
		return fail("persist_launch_effect", sess, err)
	}

	// Emit started via relayd (authoritative seq).
	if _, err := h.Coord.Emit(ctx, t, sess.Persist.Name, "started", nil); err != nil {
		return fail("emit_started", sess, fmt.Errorf("emit started: %w", err))
	}

	// Inject goal for agent mode after a short readiness wait.
	if kind == KindAgent && opts.Goal != "" {
		if err := h.injectAgentGoal(ctx, t, sess, ho, opts.Goal); err != nil {
			if ho.Status != StatusNeedsInput {
				err = h.failDelivery(ctx, ho, sess, err)
			}
			return nil, ho, err
		}
	}

	pane := false
	if !opts.NoPane && h.Viz != nil && h.Viz.Available(ctx) {
		// Restorable argv: `relay resume --session <persist>` (cmux Vault extracts --session).
		launch := ResumeLaunchCmd(sess.Persist.Name)
		ref, err := PresentSession(ctx, h.Viz, sess, launch, handoffLayout(opts))
		if err != nil {
			ho.PresentationState, ho.PresentationError = EffectFailed, err.Error()
			return nil, ho, h.failPresentation(ctx, ho, sess, "visualization", err)
		}
		pane = !strings.HasPrefix(ref, "viz:queued:")
		if pane {
			ho.PresentationState = EffectAcknowledged
		} else {
			ho.PresentationState = EffectPending
		}
		ho.PresentationError = ""
		sess.VizSurfaceRef = ref
		if err := h.Sessions.Reg.PutSession(sess); err != nil {
			return nil, ho, h.failPresentation(ctx, ho, sess, "persist_visualization", err)
		}
		if err := h.Reg.PutHandoff(ho); err != nil {
			return nil, ho, h.failPresentation(ctx, ho, sess, "persist_visualization_effect", err)
		}
		RememberResume(sess)
		RememberPane(ref, sess, true)
		labels := map[string]string{sess.ID: ProjectLabel(sess.Persist.Name)}
		if all, err := h.Sessions.List(); err == nil {
			for _, s := range all {
				labels[s.ID] = ProjectLabel(s.Persist.Name)
			}
		}
		_ = h.Viz.BrandLabels(ctx, labels)
	} else if ho.PresentationState == EffectPending {
		ho.PresentationState = EffectNotApplicable
		if err := h.Reg.PutHandoff(ho); err != nil {
			return fail("persist_visualization_effect", sess, err)
		}
	}

	b := &Binding{
		V:                 1,
		HandoffID:         hid,
		SessionID:         sess.ID,
		HostID:            opts.HostID,
		Kind:              string(kind),
		Goal:              opts.Goal,
		Events:            eventsPath,
		Watch:             fmt.Sprintf("relay agent wait %s", hid),
		Pane:              pane,
		SourceSessionID:   opts.SourceSessionID,
		SourceHostID:      opts.SourceHostID,
		SourcePersistName: opts.SourcePersistName,
	}
	return b, ho, nil
}

func (h *HandoffService) failLaunch(ctx context.Context, ho *Handoff, sess *Session, cause error) error {
	return h.failLaunchStage(ctx, ho, sess, "launch", cause)
}

func (h *HandoffService) failLaunchStage(ctx context.Context, ho *Handoff, sess *Session, stage string, cause error) error {
	if ho.LaunchState == EffectPending {
		ho.LaunchState = EffectFailed
		ho.LaunchError = redactedFailureError(cause)
	}
	ho.FailureStage = stage
	ho.FailureError = redactedFailureError(cause)
	return h.failEffect(ctx, ho, sess, cause)
}

func (h *HandoffService) failDelivery(ctx context.Context, ho *Handoff, sess *Session, cause error) error {
	ho.DeliveryState = EffectFailed
	ho.DeliveryError = redactedFailureError(cause)
	ho.FailureStage = "delivery"
	ho.FailureError = redactedFailureError(cause)
	return h.failEffect(ctx, ho, sess, cause)
}

func (h *HandoffService) failPresentation(ctx context.Context, ho *Handoff, sess *Session, stage string, cause error) error {
	ho.PresentationState = EffectFailed
	ho.PresentationError = redactedFailureError(cause)
	ho.FailureStage = stage
	ho.FailureError = redactedFailureError(cause)
	return h.failEffect(ctx, ho, sess, cause)
}

func (h *HandoffService) failEffect(ctx context.Context, ho *Handoff, sess *Session, cause error) error {
	now := time.Now().UTC()
	ho.Status = StatusFailed
	ho.Outcome = string(OutcomeFailed)
	ho.EndedAt = &now
	if err := h.Reg.PutHandoff(ho); err != nil {
		return fmt.Errorf("%w; persist terminal handoff: %v", cause, err)
	}
	h.recordFailureEvent(ctx, ho, sess)
	// Terminalize before teardown so concurrent idle events cannot create a
	// parent ask. Session destruction is best effort; the durable failure keeps
	// retries idempotent even if remote cleanup needs reconciliation.
	if h.Sessions != nil && sess != nil {
		if err := h.Sessions.Destroy(ctx, sess.ID, false); err != nil {
			ho.CleanupError = redactedFailureError(err)
			ho.RetrySafe = false
			_ = h.Reg.PutHandoff(ho)
			h.notifyLaunchFailure(ctx, ho)
			return fmt.Errorf("%w; cleanup failed: %v", cause, err)
		}
	}
	ho.RetrySafe = true
	_ = h.Reg.PutHandoff(ho)
	h.notifyLaunchFailure(ctx, ho)
	return cause
}

func (h *HandoffService) recordFailureEvent(ctx context.Context, ho *Handoff, sess *Session) {
	if h.Coord == nil || sess == nil || h.NewTransport == nil {
		ho.FailureEventState = EffectNotApplicable
		_ = h.Reg.PutHandoff(ho)
		return
	}
	ho.FailureEventState = EffectPending
	_ = h.Reg.PutHandoff(ho)
	t, err := h.NewTransport(ho.HostID)
	if err == nil {
		_, err = h.Coord.Emit(ctx, t, sess.Persist.Name, "result", map[string]any{
			"source": "launcher", "correlation_id": "launch-failure:" + ho.ID,
			"attempt_id": ho.ID, "failure_stage": ho.FailureStage,
			"text": "launch failed at " + ho.FailureStage + ": " + redactedFailureError(firstFailure(ho)),
		})
	}
	if err != nil {
		ho.FailureEventState, ho.FailureEventError = EffectFailed, redactedFailureError(err)
	} else {
		ho.FailureEventState, ho.FailureEventError = EffectAcknowledged, ""
	}
	_ = h.Reg.PutHandoff(ho)
}

func redactedFailureError(err error) string {
	if err == nil {
		return ""
	}
	text := compactText(err.Error())
	lower := strings.ToLower(text)
	for _, marker := range []string{"password", "passwd", "token=", "secret", "authorization:", "private key"} {
		if strings.Contains(lower, marker) {
			return "sensitive failure details redacted; inspect the local handoff record"
		}
	}
	if len(text) > 240 {
		text = text[:240]
	}
	return text
}

func (h *HandoffService) notifyLaunchFailure(ctx context.Context, ho *Handoff) {
	if ho == nil || ho.SourceSessionID == "" {
		if ho != nil {
			ho.FailureNoticeState = EffectNotApplicable
			_ = h.Reg.PutHandoff(ho)
		}
		return
	}
	ho.FailureNoticeState = EffectPending
	_ = h.Reg.PutHandoff(ho)
	if h.ParentRouter == nil {
		ho.FailureNoticeState = EffectFailed
		ho.FailureNoticeError = "parent router unavailable; recover from durable handoff"
		_ = h.Reg.PutHandoff(ho)
		return
	}
	cleanup := "complete"
	if ho.CleanupError != "" {
		cleanup = "failed: " + redactedFailureError(fmt.Errorf("%s", ho.CleanupError))
	}
	text := fmt.Sprintf("launch attempt %s failed | host=%s agent=%s cwd=%s name=%s stage=%s launch=%s delivery=%s presentation=%s cleanup=%s retry_safe=%t session=%s handoff=%s error=%s",
		ho.ID, ho.HostID, ho.Agent, ho.RemoteCWD, ho.Name, ho.FailureStage,
		ho.LaunchState, ho.DeliveryState, ho.PresentationState, cleanup, ho.RetrySafe,
		ho.SessionID, ho.ID, redactedFailureError(firstFailure(ho)))
	ev := Event{Kind: "result", Meta: map[string]any{
		"text": compactText(text), "source": "launcher", "correlation_id": "launch-failure:" + ho.ID,
		"attempt_id": ho.ID, "failure_stage": ho.FailureStage, "retry_safe": ho.RetrySafe,
	}}
	msg, err := h.ParentRouter.RouteLaunchFailure(ctx, ho, ev)
	if err != nil {
		ho.FailureNoticeError = redactedFailureError(err)
		if msg == nil {
			ho.FailureNoticeState = EffectFailed
		}
		_ = h.Reg.PutHandoff(ho)
		return
	}
	ho.FailureNoticeState, ho.FailureNoticeError = EffectAcknowledged, ""
	_ = h.Reg.PutHandoff(ho)
}

func firstFailure(ho *Handoff) error {
	for _, text := range []string{ho.FailureError, ho.PresentationError, ho.DeliveryError, ho.LaunchError} {
		if text != "" {
			return fmt.Errorf("%s", text)
		}
	}
	return nil
}

func agentGoalPrompt(goal string) string {
	return goal + `

If blocked on manager input, run relay ask "<question>" and stop.`
}

// verifyContainerAgent runs the agent's --version inside the container and maps
// known failure signatures to an actionable error. Returns nil when the agent
// appears runnable.
func (h *HandoffService) verifyContainerAgent(ctx context.Context, t ports.Transport, ref ContainerRef, agentInner string) error {
	probe, err := ContainerExec(ref.Runtime, ref, agentInner+" --version", false)
	if err != nil {
		return err
	}
	out, errOut, _ := t.Run(ctx, "", probe)
	combined := strings.TrimSpace(out + "\n" + errOut)
	if ok, guidance := ClassifyContainerVerify(combined); !ok {
		return fmt.Errorf("container verify failed: %s\n--- probe output ---\n%s", guidance, combined)
	}
	return nil
}

func waitAgentReady(ctx context.Context, p ports.Persistence, t ports.Transport, h ports.PersistHandle, timeout time.Duration) AgentReadiness {
	deadline := time.Now().Add(timeout)
	var last string
	stable := 0
	attempts := 0
	const maxAttempts = 8 // ~2.5s * 8 with ControlMaster — no SSH storm
	for time.Now().Before(deadline) && attempts < maxAttempts {
		select {
		case <-ctx.Done():
			return AgentReadiness{State: AgentAbsent, Reason: ctx.Err().Error()}
		default:
		}
		attempts++
		text, err := p.Capture(ctx, t, h, 30)
		if err == nil {
			readiness := ClassifyAgentPane(text)
			if readiness.State == AgentBlocked {
				return readiness
			}
			if text == last && text != "" {
				stable++
				if stable >= 2 {
					return readiness
				}
			} else {
				stable = 0
				last = text
			}
		}
		time.Sleep(agentReadyPollDelay)
	}
	if last == "" {
		return AgentReadiness{State: AgentAbsent, Reason: "could not read agent pane"}
	}
	return ClassifyAgentPane(last)
}

var agentReadyPollDelay = 2500 * time.Millisecond

func (h *HandoffService) injectAgentGoal(ctx context.Context, t ports.Transport, sess *Session, ho *Handoff, goal string) error {
	readiness := waitAgentReady(ctx, h.Persist, t, sess.Persist, 20*time.Second)
	switch readiness.State {
	case AgentBlocked:
		if readiness.Gate != nil && sess.RemoteCWD != "" {
			readiness.Gate.Directory = sess.RemoteCWD
		}
		ho.Status = StatusNeedsInput
		ho.DeliveryState = EffectBlocked
		ho.DeliveryError = readiness.Reason
		ho.PendingGate = readiness.Gate
		if err := h.Reg.PutHandoff(ho); err != nil {
			return err
		}
		meta := map[string]any{"text": readiness.Reason}
		if readiness.Gate != nil {
			meta["gate"] = readiness.Gate
			meta["text"] = formatSecurityGate(readiness.Gate)
		}
		_, err := h.Coord.Emit(ctx, t, sess.Persist.Name, "permission_required", meta)
		return err
	case AgentAbsent:
		_, _ = h.Coord.Emit(ctx, t, sess.Persist.Name, "exit", map[string]any{"text": readiness.Reason})
		return fmt.Errorf("agent exited before goal delivery: %s", readiness.Reason)
	default:
		if err := h.Persist.Send(ctx, t, sess.Persist, agentGoalPrompt(goal), true); err != nil {
			return err
		}
		ho.DeliveryState = EffectAcknowledged
		ho.DeliveryError = ""
		return h.Reg.PutHandoff(ho)
	}
}

func formatSecurityGate(gate *SecurityGate) string {
	if gate == nil {
		return "security decision required"
	}
	parts := []string{gate.Reason}
	if gate.Directory != "" {
		parts = append(parts, "directory: "+gate.Directory)
	}
	for _, choice := range gate.Choices {
		parts = append(parts, fmt.Sprintf("%d. %s", choice.Index, choice.Label))
	}
	return strings.Join(parts, " | ")
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
			applyHandoffEventStatus(ho, ev.Kind)
			if ev.Kind == "exit" {
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
		st := ui.NewStatus()
		ok := st.Wait(delay, func(left time.Duration) string {
			return subscribeRetryStatus(ho.HostID, attempts, left, subErr)
		}, ctx.Done())
		st.Clear()
		if !ok {
			return ctx.Err()
		}
	}
}

// subscribeRetryStatus keeps transient SSH diagnostics inside the single-line
// reconnect UI rather than letting each retry write a separate terminal line.
func subscribeRetryStatus(host string, attempt int, left time.Duration, err error) string {
	detail := "last error unavailable"
	if err != nil {
		detail = "last error: " + ui.Truncate(err.Error(), 72)
	}
	return ui.JoinStatus(
		"waiting "+host,
		detail,
		fmt.Sprintf("retry %d/6 in %s", attempt, ui.FormatDuration(left)),
	)
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
	stream := sess.Persist.Name
	if handoffs, err := h.Reg.ListHandoffs(); err != nil {
		return err
	} else {
		var newest time.Time
		for _, ho := range handoffs {
			if ho.SessionID == sessionID && !handoffTerminal(ho) && ho.Name != "" && (newest.IsZero() || ho.CreatedAt.After(newest)) {
				stream, newest = ho.Name, ho.CreatedAt
			}
		}
	}
	emitFactory := func(kind string) (string, error) {
		return h.Coord.SensorCommand(stream, kind)
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

// Session reaping now lives in MaintenanceService.GC (sessions-only mode) so
// there is a single reap + host-probe implementation; `relay resume reap` and
// `relay gc` share it. See internal/core/maintenance.go.

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
