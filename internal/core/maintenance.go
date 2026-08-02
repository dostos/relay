package core

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

// MaintenanceService is the single "clean up when done" entry point. It folds
// the previously-scattered cleanup (reap dead sessions, prune tombstones, close
// orphan panes, GC stale message channels) into ONE pass that is:
//   - token/traffic efficient: exactly one probe SSH per host (tmux sessions +
//     message channels in a single exec), reused for both session reap and
//     channel GC; local steps do zero SSH.
//   - robust: unreachable hosts are skipped (never guessed dead), everything is
//     idempotent, and --dry-run reports without mutating.
type MaintenanceService struct {
	Sessions     *SessionService
	Reg          *Registry
	Viz          ports.Viz
	NewTransport TransportFactory
}

// GCHostResult is the per-host outcome.
type GCHostResult struct {
	Host           string   `json:"host"`
	Reachable      bool     `json:"reachable"`
	ReapedSessions []string `json:"reaped_sessions,omitempty"`
	HeldSessions   []string `json:"held_sessions,omitempty"`
	GCedChannels   []string `json:"gced_channels,omitempty"`
	KeptSessions   int      `json:"kept_sessions"`
	KeptChannels   int      `json:"kept_channels"`
}

// GCReport is the whole-sweep summary (terse by design — token-efficient output).
type GCReport struct {
	DryRun             bool           `json:"dry_run"`
	Hosts              []GCHostResult `json:"hosts"`
	PaneFilesRemoved   int            `json:"pane_files_removed"`
	VizBindingsRemoved int            `json:"viz_bindings_removed"`
	TombstonesPruned   int            `json:"tombstones_pruned"`
	DanglingLineage    []string       `json:"dangling_lineage,omitempty"`
}

type channelStat struct {
	name  string
	lines int
	mtime int64
}

// GC runs the full sweep. hosts empty ⇒ every host referenced by the registry.
// channelTTL <= 0 disables age-based channel GC (empty channels are still GC'd).
// skipChannels leaves message channels untouched (session-reap only — this is
// what `relay resume reap` uses).
func (m *MaintenanceService) GC(ctx context.Context, hosts []string, channelTTL time.Duration, skipChannels, dryRun bool) (*GCReport, error) {
	report := &GCReport{DryRun: dryRun}

	if len(hosts) == 0 {
		hosts = m.registryHosts()
	}

	// Local, zero-SSH: drop pane-state files for sessions already known dead
	// (cleaned tombstones) so a later cmux restore can't resurrect them.
	if !dryRun {
		if entries, err := m.Sessions.ListResumeStatus(); err == nil {
			for _, e := range entries {
				if e.Presence == PresenceCleaned {
					report.PaneFilesRemoved += RemovePaneBindingsForPersist(e.PersistName)
					if cleaner, ok := m.Viz.(interface {
						ClosePersist(context.Context, string) int
					}); ok {
						report.VizBindingsRemoved += cleaner.ClosePersist(ctx, e.PersistName)
					}
				}
			}
		}
	}

	// Per host: one probe SSH → reap dead sessions + GC stale channels.
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // IPS-safe: cap concurrent hosts
	for _, host := range hosts {
		host := host
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := m.gcHost(ctx, host, channelTTL, skipChannels, dryRun)
			mu.Lock()
			report.Hosts = append(report.Hosts, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	report.DanglingLineage = m.danglingLineage()

	// Local: prune the tombstones (including ones this sweep just created).
	if !dryRun {
		if removed, err := PruneResume(true, 0); err == nil {
			report.TombstonesPruned = len(removed)
		}
	}
	return report, nil
}

func (m *MaintenanceService) danglingLineage() []string {
	if m.Reg == nil {
		return nil
	}
	sessions, err := m.Reg.ListSessions()
	if err != nil {
		return nil
	}
	byID := map[string]*Session{}
	for _, sess := range sessions {
		byID[sess.ID] = sess
	}
	var dangling []string
	for _, sess := range sessions {
		if sess.SourceSessionID != "" && byID[sess.SourceSessionID] == nil {
			dangling = append(dangling, sess.ID)
		}
	}
	return dangling
}

func (m *MaintenanceService) registryHosts() []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h != "" && h != LocalHostID && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if entries, err := m.Sessions.ListResumeStatus(); err == nil {
		for _, e := range entries {
			if e.Presence == PresenceLive || e.Presence == PresenceDisconnected {
				add(e.HostID)
			}
		}
	}
	if sessions, err := m.Reg.ListSessions(); err == nil {
		for _, s := range sessions {
			add(s.HostID)
		}
	}
	return out
}

func (m *MaintenanceService) gcHost(ctx context.Context, host string, channelTTL time.Duration, skipChannels, dryRun bool) GCHostResult {
	res := GCHostResult{Host: host}
	if host == LocalHostID {
		return res
	}
	t, err := m.NewTransport(host)
	if err != nil {
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tmuxSet, channels, ok := probeHostState(cctx, t)
	if !ok {
		return res // unreachable — skip, never guess dead
	}
	res.Reachable = true

	// Reap sessions whose remote tmux is gone.
	sessions, _ := m.Reg.ListSessions()
	for _, s := range sessions {
		if s.HostID != host {
			continue
		}
		if tmuxSet[s.Persist.Name] {
			res.KeptSessions++
			continue
		}
		if children, childErr := m.Reg.DirectChildren(s.ID); childErr != nil || len(children) > 0 {
			res.HeldSessions = append(res.HeldSessions, s.Persist.Name)
			continue
		}
		res.ReapedSessions = append(res.ReapedSessions, s.Persist.Name)
		if dryRun {
			continue
		}
		if err := DeleteSessionProjected(ctx, m.Reg, m.Viz, s, false); err != nil {
			res.HeldSessions = append(res.HeldSessions, s.Persist.Name)
			continue
		}
		MarkResumeCleaned(s.Persist.Name, "gc: remote tmux absent")
		RemovePaneBindingsForPersist(s.Persist.Name)
	}

	if skipChannels {
		return res
	}
	// GC message channels: empty, or untouched past the TTL.
	now := time.Now().Unix()
	var toGC []string
	for _, c := range channels {
		if strings.HasPrefix(c.name, "relay-viz-") {
			res.KeptChannels++
			continue
		}
		stale := c.lines == 0 || (channelTTL > 0 && now-c.mtime > int64(channelTTL.Seconds()))
		if stale {
			res.GCedChannels = append(res.GCedChannels, c.name)
			toGC = append(toGC, c.name)
		} else {
			res.KeptChannels++
		}
	}
	if len(toGC) > 0 && !dryRun {
		removeChannels(cctx, t, toGC)
	}
	return res
}

// probeHostState returns live tmux session names and message-channel stats from
// ONE remote exec. ok=false means the host was unreachable (transport error) —
// distinct from a reachable host with nothing to report.
func probeHostState(ctx context.Context, t ports.Transport) (tmux map[string]bool, channels []channelStat, ok bool) {
	// One shell: tmux names, then channel name<TAB>lines<TAB>mtime. `|| true`
	// keeps "no server"/"no matches" from being read as a transport failure.
	script := `printf '@TMUX\n'; tmux list-sessions -F '#{session_name}' 2>/dev/null || true; ` +
		`printf '@CHAN\n'; d="$HOME/.local/state/relay/events"; ` +
		`for f in "$d"/chan.*.jsonl; do [ -e "$f" ] || continue; ` +
		`n=$(basename "$f" .jsonl); n=${n#chan.}; ` +
		`l=$(wc -l < "$f" 2>/dev/null | tr -d ' '); ` +
		`m=$(stat -c %Y "$f" 2>/dev/null || stat -f %m "$f" 2>/dev/null || echo 0); ` +
		`printf '%s\t%s\t%s\n' "$n" "$l" "$m"; done`
	out, _, err := t.Run(ctx, "~", script)
	if err != nil {
		return nil, nil, false
	}
	tmux, channels = parseHostState(out)
	return tmux, channels, true
}

// parseHostState parses the probe output (pure; unit-testable without SSH).
func parseHostState(out string) (map[string]bool, []channelStat) {
	tmux := map[string]bool{}
	var channels []channelStat
	section := ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "@TMUX" || ln == "@CHAN" {
			section = ln
			continue
		}
		if ln == "" {
			continue
		}
		if section == "@TMUX" {
			tmux[strings.TrimSpace(ln)] = true
			continue
		}
		parts := strings.Split(ln, "\t")
		if len(parts) < 3 {
			continue
		}
		lines, _ := strconv.Atoi(parts[1])
		mt, _ := strconv.ParseInt(parts[2], 10, 64)
		channels = append(channels, channelStat{name: parts[0], lines: lines, mtime: mt})
	}
	return tmux, channels
}

func removeChannels(ctx context.Context, t ports.Transport, names []string) {
	var b strings.Builder
	b.WriteString("rm -f")
	any := false
	for _, n := range names {
		// A validated session name is [A-Za-z0-9._-]{1,64} with no "..", so it
		// is shell-safe to interpolate directly; ~ stays unquoted so it expands.
		if shellquote.ValidateSessionName(n) != nil {
			continue // never rm a path we can't validate
		}
		b.WriteString(" ~/.local/state/relay/events/chan.")
		b.WriteString(n)
		b.WriteString(".jsonl")
		any = true
	}
	if !any {
		return
	}
	_, _, _ = t.Run(ctx, "~", b.String())
}
