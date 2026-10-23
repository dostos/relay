package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/core"
)

// `relay report` (#75) aggregates this machine's usage log for a daily
// review: receipt states, refusal reasons, errors, replays, op states, lock
// contention and recent failure examples, plus heuristic findings.

type countRow struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type sendStats struct {
	Total      int            `json:"total"`
	ByState    map[string]int `json:"by_state"`
	NotSentBy  map[string]int `json:"not_sent_by_evidence"`
	UnsureBy   map[string]int `json:"unconfirmed_by_evidence"`
	Keyed      int            `json:"keyed"`
	Replays    int            `json:"replays"`
	Direct     int            `json:"direct"`
	Bridged    int            `json:"bridged"`
	AcceptRate float64        `json:"accept_rate"`
}

type usageReport struct {
	OK               bool           `json:"ok"`
	Host             string         `json:"host"`
	Build            string         `json:"build"`
	Since            string         `json:"since"`
	From             time.Time      `json:"from"`
	To               time.Time      `json:"to"`
	Events           int            `json:"events"`
	ByCmd            map[string]int `json:"by_cmd"`
	Send             sendStats      `json:"send"`
	LockContention   int            `json:"lock_contention"`
	GateRefusals     int            `json:"gate_refusals"`
	ComposerRefusals int            `json:"composer_refusals"`
	Wait             map[string]int `json:"wait_outcomes"`
	OpWaits          map[string]int `json:"op_wait_states"`
	Ops              map[string]int `json:"ops_on_host"`
	StaleLocks       int            `json:"stale_send_locks"`
	Errors           []countRow     `json:"errors"`
	Targets          []countRow     `json:"failing_targets"`
	Builds           map[string]int `json:"builds"`
	Recent           []usageEvent   `json:"recent_failures"`
	Findings         []string       `json:"findings"`
	Malformed        int            `json:"malformed_lines,omitempty"`
}

var (
	reSess = regexp.MustCompile(`sess-[0-9a-f]+|tmux:[^\s"'` + "`" + `]+`)
	rePath = regexp.MustCompile(`(/[\w.@%+-]+){2,}`)
	reNum  = regexp.MustCompile(`\b\d+(\.\d+)?(ms|s|m)?\b`)
)

func normalizeError(s string) string {
	s = reSess.ReplaceAllString(s, "<session>")
	s = rePath.ReplaceAllString(s, "<path>")
	s = reNum.ReplaceAllString(s, "N")
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return s
}

func parseSince(v string) (time.Duration, error) {
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(v)
}

func (a *App) cmdReport(args []string) int {
	since := "24h"
	examples := 10
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--since", "--examples":
			if i+1 >= len(args) {
				return a.fail(fmt.Errorf("%s requires a value", args[i]))
			}
			if args[i] == "--since" {
				since = args[i+1]
			} else {
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n < 0 || n > 200 {
					return a.fail(fmt.Errorf("--examples must be 0-200"))
				}
				examples = n
			}
			i++
		default:
			return a.failStructured(rejectUnknownFlag(args[i]), map[string]any{"reason": "usage", "argv": []string{"relay", "report", "--help"}})
		}
	}
	d, err := parseSince(since)
	if err != nil || d <= 0 {
		return a.fail(fmt.Errorf("--since %q: use a duration like 24h, 90m or 7d", since))
	}
	r := buildReport(usageDir(), time.Now().UTC(), d, examples)
	r.Since = since
	if a.JSON {
		return a.errOut(a.out(r))
	}
	printReport(r)
	return 0
}

func buildReport(dir string, now time.Time, window time.Duration, examples int) usageReport {
	from := now.Add(-window)
	host, _ := os.Hostname()
	if id := core.LocalHostIDFromProfile(); id != "" {
		host = id
	}
	r := usageReport{OK: true, Host: host, Build: coord.Build, From: from, To: now,
		ByCmd: map[string]int{}, Wait: map[string]int{}, OpWaits: map[string]int{}, Builds: map[string]int{},
		Send: sendStats{ByState: map[string]int{}, NotSentBy: map[string]int{}, UnsureBy: map[string]int{}}}
	errs := map[string]int{}
	targets := map[string]int{}
	var failures []usageEvent
	for day := from.Truncate(24 * time.Hour); !day.After(now); day = day.Add(24 * time.Hour) {
		f, err := os.Open(filepath.Join(dir, day.Format("2006-01-02")+".jsonl"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var ev usageEvent
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				r.Malformed++
				continue
			}
			if ev.At.Before(from) || ev.At.After(now) {
				continue
			}
			r.Events++
			r.ByCmd[ev.Cmd]++
			r.Builds[ev.Build]++
			switch ev.Cmd {
			case "session.send", "session.submit":
				s := &r.Send
				s.Total++
				s.ByState[ev.State]++
				if ev.Keyed {
					s.Keyed++
				}
				if ev.Replayed {
					s.Replays++
				}
				if ev.Direct {
					s.Direct++
				}
				if ev.Bridged {
					s.Bridged++
				}
				switch ev.State {
				case "not_sent":
					key := ev.Evidence
					if key == "" {
						key = "error"
					}
					s.NotSentBy[key]++
				case "deposited", "uncertain":
					s.UnsureBy[ev.Evidence]++
				}
				switch ev.Evidence {
				case "send_lock_busy":
					r.LockContention++
				case "target_at_gate":
					r.GateRefusals++
				case "composer_not_empty":
					r.ComposerRefusals++
				}
			case "session.wait":
				r.Wait[ev.State]++
			case "session.op.wait":
				r.OpWaits[ev.State]++
			}
			if ev.Exit != 0 || ev.Error != "" {
				if ev.Error != "" {
					errs[normalizeError(ev.Error)]++
				}
				if ev.Target != "" {
					targets[ev.Target]++
				}
				failures = append(failures, ev)
			}
		}
		f.Close()
	}
	if r.Send.Total > 0 {
		ok := r.Send.ByState["submitted"] + r.Send.ByState["queued"]
		r.Send.AcceptRate = float64(int(float64(ok)/float64(r.Send.Total)*1000)) / 1000
	}
	r.Errors = topRows(errs, 10)
	r.Targets = topRows(targets, 10)
	sort.Slice(failures, func(i, j int) bool { return failures[i].At.After(failures[j].At) })
	if len(failures) > examples {
		failures = failures[:examples]
	}
	r.Recent = failures
	if r.Recent == nil {
		r.Recent = []usageEvent{}
	}
	r.Ops, r.StaleLocks = scanHostState(from)
	r.Findings = findings(r)
	return r
}

func topRows(m map[string]int, n int) []countRow {
	rows := make([]countRow, 0, len(m))
	for k, v := range m {
		rows = append(rows, countRow{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Key < rows[j].Key
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// scanHostState reads the host-side op and send-lock state that targets on
// THIS machine keep under ~/.local/state/relay.
func scanHostState(from time.Time) (map[string]int, int) {
	ops := map[string]int{}
	home, err := os.UserHomeDir()
	if err != nil {
		return ops, 0
	}
	root := filepath.Join(home, core.RemoteStateRel)
	dirs, _ := filepath.Glob(filepath.Join(root, "ops", "*", "*"))
	for _, d := range dirs {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() || st.ModTime().Before(from) {
			continue
		}
		switch {
		case fileExists(filepath.Join(d, "exit")):
			code, _ := os.ReadFile(filepath.Join(d, "exit"))
			if strings.TrimSpace(string(code)) == "0" {
				ops["exited_ok"]++
			} else {
				ops["exited_nonzero"]++
			}
		case fileExists(filepath.Join(d, "pid")):
			ops["running_or_lost"]++
		default:
			ops["starting_or_lost"]++
		}
	}
	stale := 0
	locks, _ := filepath.Glob(filepath.Join(root, "sends", "*.lock"))
	for _, l := range locks {
		if st, err := os.Stat(l); err == nil && time.Since(st.ModTime()) > 2*time.Minute {
			stale++
		}
	}
	return ops, stale
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func findings(r usageReport) []string {
	var out []string
	s := r.Send
	if s.Total >= 5 {
		unsure := s.ByState["deposited"] + s.ByState["uncertain"]
		if pct := 100 * unsure / s.Total; pct >= 10 {
			out = append(out, fmt.Sprintf("%d%% of sends unconfirmed (deposited/uncertain: %v)", pct, s.UnsureBy))
		}
	}
	if r.LockContention > 0 {
		out = append(out, fmt.Sprintf("%d sends refused for send-lock contention (send_lock_busy)", r.LockContention))
	}
	if r.GateRefusals > 0 {
		out = append(out, fmt.Sprintf("%d sends refused at approval gates (target_at_gate): a human needed to answer", r.GateRefusals))
	}
	if r.ComposerRefusals > 0 {
		out = append(out, fmt.Sprintf("%d sends refused because the target input held a draft (composer_not_empty)", r.ComposerRefusals))
	}
	if n := s.NotSentBy["error"]; n > 0 {
		out = append(out, fmt.Sprintf("%d sends failed before typing with other errors", n))
	}
	if total := sumMap(r.Wait); total >= 3 && r.Wait["timeout"]*100/total >= 30 {
		out = append(out, fmt.Sprintf("%d/%d waits timed out", r.Wait["timeout"], total))
	}
	if s.Direct > 0 {
		out = append(out, fmt.Sprintf("%d sends ran directly because the desktop bridge was unreachable", s.Direct))
	}
	if r.StaleLocks > 0 {
		out = append(out, fmt.Sprintf("%d stale send locks on this host", r.StaleLocks))
	}
	if len(r.Builds) > 1 {
		out = append(out, fmt.Sprintf("events from %d different relay builds in the window: %v", len(r.Builds), r.Builds))
	}
	if r.Malformed > 0 {
		out = append(out, fmt.Sprintf("%d malformed usage-log lines", r.Malformed))
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func sumMap(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func printReport(r usageReport) {
	fmt.Printf("relay report %s (build %s) since %s: %d events\n", r.Host, r.Build, r.Since, r.Events)
	fmt.Printf("  sends %d accept_rate %.3f states %v\n", r.Send.Total, r.Send.AcceptRate, r.Send.ByState)
	if len(r.Send.NotSentBy) > 0 {
		fmt.Printf("  not_sent %v\n", r.Send.NotSentBy)
	}
	if len(r.Send.UnsureBy) > 0 {
		fmt.Printf("  unconfirmed %v\n", r.Send.UnsureBy)
	}
	fmt.Printf("  replays %d keyed %d direct %d bridged %d\n", r.Send.Replays, r.Send.Keyed, r.Send.Direct, r.Send.Bridged)
	if len(r.Wait) > 0 {
		fmt.Printf("  waits %v\n", r.Wait)
	}
	if len(r.Ops) > 0 || len(r.OpWaits) > 0 {
		fmt.Printf("  ops %v op_waits %v\n", r.Ops, r.OpWaits)
	}
	for _, e := range r.Errors {
		fmt.Printf("  error x%d %s\n", e.Count, e.Key)
	}
	for _, f := range r.Findings {
		fmt.Println("  finding: " + f)
	}
}
