package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/shellquote"
)

// `relay fleet` (#74) runs on the control host (the Mac hub) and keeps every
// relay host it knows (~/.local/state/relay/hosts/*.json) on the hub's
// build: status, upgrade (cross-compile → ssh → atomic install → service
// restart → verify → agent guidance), and a fleet-wide usage report.

// fleetFanout bounds concurrent ssh connections: the campus IPS flags more
// than 4 concurrent ssh from one source (docs/fleet-roles.md).
const fleetFanout = 3

type fleetOpts struct {
	hosts      []string
	dryRun     bool
	force      bool
	skipAgents bool
	parallel   int
	since      string
}

type fleetHost struct {
	Host           string `json:"host"`
	Hub            bool   `json:"hub,omitempty"`
	Reachable      bool   `json:"reachable"`
	OS             string `json:"os,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Platform       string `json:"platform,omitempty"`
	InstalledBuild string `json:"installed_build"`
	HubBuild       string `json:"hub_build"`
	UpToDate       bool   `json:"up_to_date"`
	OnSSHPath      string `json:"relay_on_ssh_path,omitempty"`
	// Target is the file fleet installs into: ~/.local/bin/relay, or
	// relay-real behind a host-side forwarding shim (#82).
	Target string `json:"install_target,omitempty"`
	Shim   bool   `json:"shim,omitempty"`
	// MachineKey identifies the (machine, user, HOME) an alias resolves to;
	// AliasOf names the host entry that was acted on for it (#79, #80).
	MachineKey  string          `json:"-"`
	AliasOf     string          `json:"alias_of,omitempty"`
	MissingCaps []string        `json:"missing_capabilities,omitempty"`
	Agents      json.RawMessage `json:"agents,omitempty"`
	AgentsOK    *bool           `json:"agents_ok,omitempty"`
	Action      string          `json:"action,omitempty"`
	AfterBuild  string          `json:"after_build,omitempty"`
	Restarted   string          `json:"restarted,omitempty"`
	Error       string          `json:"error,omitempty"`
	DurationMS  int64           `json:"duration_ms"`
}

func (a *App) cmdFleet(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.helpFor([]string{"fleet"}, 2)
	}
	verb := args[0]
	o := fleetOpts{parallel: fleetFanout, since: "24h"}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		switch {
		case f == "--hosts" || f == "-H":
			if i+1 >= len(rest) {
				return a.fail(fmt.Errorf("%s requires a value (comma-separated hosts)", f))
			}
			for _, h := range strings.Split(rest[i+1], ",") {
				if h = strings.TrimSpace(h); h != "" {
					o.hosts = append(o.hosts, h)
				}
			}
			i++
		case f == "--parallel":
			if i+1 >= len(rest) {
				return a.fail(fmt.Errorf("--parallel requires a value"))
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil || n < 1 || n > 4 {
				return a.fail(fmt.Errorf("--parallel must be 1-4 (ssh burst limit)"))
			}
			o.parallel = n
			i++
		case f == "--since" && verb == "report":
			if i+1 >= len(rest) {
				return a.fail(fmt.Errorf("--since requires a value"))
			}
			o.since = rest[i+1]
			i++
		case f == "--dry-run" && verb == "upgrade":
			o.dryRun = true
		case f == "--force" && verb == "upgrade":
			o.force = true
		case f == "--skip-agents" && verb == "upgrade":
			o.skipAgents = true
		default:
			return a.failStructured(rejectUnknownFlag(f), map[string]any{"reason": "usage", "argv": []string{"relay", "fleet", verb, "--help"}})
		}
	}
	hosts := o.hosts
	if len(hosts) == 0 {
		hosts = core.KnownHosts()
	}
	if len(hosts) == 0 {
		return a.failStructured(errors.New("no known relay hosts (none under ~/.local/state/relay/hosts); add one with relay host init"), map[string]any{
			"reason": "no_hosts", "argv": []string{"relay", "host", "init", "-H", "HOST", "--apply"}})
	}
	switch verb {
	case "status", "upgrade", "install":
		if verb == "install" {
			verb = "upgrade"
		}
		results := a.fleetRun(ctx, hosts, o, verb == "upgrade")
		return a.printFleet(verb, o, results)
	case "report":
		return a.fleetReport(ctx, hosts, o)
	default:
		return a.failStructured(fmt.Errorf("unknown fleet verb %q (status|upgrade|report)", verb), map[string]any{"reason": "usage", "argv": []string{"relay", "fleet", "--help"}})
	}
}

func (a *App) fleetRun(ctx context.Context, hosts []string, o fleetOpts, upgrade bool) []fleetHost {
	results := make([]fleetHost, len(hosts))
	starts := make([]time.Time, len(hosts))
	a.fleetEach(len(hosts), o.parallel, func(i int) {
		starts[i] = time.Now()
		results[i] = a.fleetProbe(ctx, hosts[i])
	})
	// Two host entries can be one machine (an ssh alias, a tunnel, the hub
	// under its own name and "local"). Acting on both races on the same
	// files (#79, #80), so act once per machine and report the rest as
	// aliases of the entry that was acted on.
	primary := fleetPrimaries(results)
	if upgrade {
		bins := &binCache{m: map[string]*binEntry{}}
		tmp, err := os.MkdirTemp("", "relay-fleet-")
		if err == nil {
			defer os.RemoveAll(tmp)
		}
		a.fleetEach(len(hosts), o.parallel, func(i int) {
			if primary[i] != i {
				return
			}
			if results[i].Reachable {
				a.fleetUpgrade(ctx, &results[i], o, bins, tmp)
			} else {
				results[i].Action = "unreachable"
			}
		})
	}
	for i := range results {
		if p := primary[i]; p != i {
			results[i] = fleetAlias(results[i], results[p])
		}
		results[i].DurationMS = time.Since(starts[i]).Milliseconds()
	}
	return results
}

func (a *App) fleetEach(n, parallel int, f func(i int)) {
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f(i)
		}(i)
	}
	wg.Wait()
}

// fleetPrimaries maps each result index to the index acted on for its
// machine: the hub entry if one resolves there, else the first listed.
func fleetPrimaries(results []fleetHost) []int {
	primary := make([]int, len(results))
	first := map[string]int{}
	for i, r := range results {
		primary[i] = i
		if !r.Reachable || r.MachineKey == "" {
			continue
		}
		p, ok := first[r.MachineKey]
		if !ok {
			first[r.MachineKey] = i
			continue
		}
		if r.Hub && !results[p].Hub {
			first[r.MachineKey] = i
		}
	}
	for i, r := range results {
		if r.Reachable && r.MachineKey != "" {
			primary[i] = first[r.MachineKey]
		}
	}
	return primary
}

// fleetAlias reports an alias entry with the state of the entry acted on.
func fleetAlias(alias, p fleetHost) fleetHost {
	out := p
	out.Host = alias.Host
	out.Hub = alias.Hub || p.Hub
	out.OnSSHPath = alias.OnSSHPath
	out.AliasOf = p.Host
	if p.Action != "" {
		out.Action = "alias_of " + p.Host
	}
	if out.Error != "" {
		out.Error = p.Host + ": " + out.Error
	}
	return out
}

// fleetResolveScript picks the relay binary fleet manages on a host. A
// text ~/.local/bin/relay next to relay-real is a host-side forwarding shim
// (e.g. one that docker-execs into a relay container and falls back to
// relay-real); fleet must manage relay-real and leave the shim alone (#82).
const fleetResolveScript = `B="$HOME/.local/bin"; R="$B/relay"; SHIM=""
if [ -f "$R" ] && [ -x "$B/relay-real" ] && [ "$(head -c 2 "$R" 2>/dev/null)" = "#!" ]; then R="$B/relay-real"; SHIM=1; fi
`

// fleetMachineScript prints the machine key that identifies which host
// entries are aliases of one machine (#79, #86).
const fleetMachineScript = `MID="$(cat /etc/machine-id 2>/dev/null || ioreg -rd1 -c IOPlatformExpertDevice 2>/dev/null | awk -F'"' '/IOPlatformUUID/{print $4}')"
printf 'machine=%s|%s|%s|%s\n' "$MID" "$(hostname 2>/dev/null || uname -n)" "$(id -un 2>/dev/null)" "$HOME"
`

const fleetProbeScript = fleetResolveScript + `printf 'os=%s\n' "$(uname -s)"
printf 'arch=%s\n' "$(uname -m)"
printf 'path=%s\n' "$(command -v relay 2>/dev/null)"
printf 'target=%s\n' "$R"
printf 'shim=%s\n' "$SHIM"
` + fleetMachineScript + `if [ -x "$R" ]; then
  printf 'build=%s\n' "$(RELAY_BRIDGE_LOCAL_INVOKE=1 "$R" build 2>/dev/null)"
  printf 'version=%s\n' "$(RELAY_BRIDGE_LOCAL_INVOKE=1 "$R" --json version 2>/dev/null | tr -d '\n')"
  printf 'agents=%s\n' "$(RELAY_BRIDGE_LOCAL_INVOKE=1 "$R" --json agents status 2>/dev/null | tr -d '\n')"
else
  echo build=absent
fi
`

func parseKV(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func (a *App) fleetProbe(ctx context.Context, host string) fleetHost {
	r := fleetHost{Host: host, HubBuild: coord.Build, InstalledBuild: "unknown"}
	r.Hub = core.IsThisHost(host)
	t, err := a.tf(host)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, stderr, err := t.Run(pctx, "", fleetProbeScript)
	if err != nil {
		r.Error = strings.TrimSpace(fmt.Sprintf("unreachable: %v %s", err, firstLine(stderr)))
		return r
	}
	r.Reachable = true
	kv := parseKV(out)
	r.OS, r.Arch, r.OnSSHPath = kv["os"], kv["arch"], kv["path"]
	r.Target, r.Shim = kv["target"], kv["shim"] == "1"
	if m := kv["machine"]; strings.Trim(m, "|") != "" {
		r.MachineKey = m
	}
	if goos, goarch, err := core.GoPlatform(r.OS, r.Arch); err == nil {
		r.Platform = goos + "/" + goarch
	}
	r.InstalledBuild = kv["build"]
	if r.InstalledBuild == "" {
		r.InstalledBuild = "unknown"
	}
	r.UpToDate = r.InstalledBuild == coord.Build
	var ver struct {
		Client coord.Identity `json:"client"`
	}
	if json.Unmarshal([]byte(kv["version"]), &ver) == nil && ver.Client.Capabilities != nil {
		r.MissingCaps = setDiff(coord.Capabilities(), ver.Client.Capabilities)
	}
	if raw := kv["agents"]; strings.HasPrefix(raw, "{") && json.Valid([]byte(raw)) {
		r.Agents = json.RawMessage(raw)
		var st struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal([]byte(raw), &st) == nil {
			ok := st.OK
			r.AgentsOK = &ok
		}
	}
	return r
}

type binEntry struct {
	once sync.Once
	path string
	err  error
}

type binCache struct {
	mu sync.Mutex
	m  map[string]*binEntry
}

func (c *binCache) get(ctx context.Context, platform, dir string) (string, error) {
	c.mu.Lock()
	e, ok := c.m[platform]
	if !ok {
		e = &binEntry{}
		c.m[platform] = e
	}
	c.mu.Unlock()
	e.once.Do(func() {
		goos, goarch, _ := strings.Cut(platform, "/")
		e.path, e.err = core.BuildRelayBinary(ctx, goos, goarch, dir)
	})
	return e.path, e.err
}

// fleetInstallScript verifies and installs a staged binary. %[1]s is the
// staged file name (unique per run, #79), %[2]s the wanted build.
const fleetInstallScript = fleetResolveScript + `set -e
N="$B/"%[1]s; WANT=%[2]s
mkdir -p "$B" "$HOME/.local/state/relay"
if [ ! -f "$N" ]; then echo "error=staged binary $N vanished before verify (another upgrade of this machine running?)"; exit 3; fi
GOT="$(RELAY_BRIDGE_LOCAL_INVOKE=1 "$N" build 2>"$N.err" || true)"
if [ "$GOT" != "$WANT" ]; then
  why="$(head -c 300 "$N.err" 2>/dev/null | tr '\n' ' ')"
  rm -f "$N" "$N.err"
  echo "error=staged build '$GOT', want $WANT: $why"; exit 3
fi
rm -f "$N.err"
if [ -e "$R" ]; then cp -p "$R" "$R.previous" 2>/dev/null || true; fi
mv -f "$N" "$R"
if [ -z "$SHIM" ]; then ln -sfn relay "$B/relayd" 2>/dev/null || true; fi
restarted=""
if [ "$(uname -s)" = Linux ] && command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  for u in relay-event.service relay.service relay-control.service; do
    if systemctl --user is-active --quiet "$u" 2>/dev/null; then systemctl --user restart "$u" && restarted="$restarted $u"; fi
  done
elif [ "$(uname -s)" = Darwin ]; then
  for l in com.dostos.relay-supervisor com.dostos.relay-system; do
    if launchctl print "gui/$(id -u)/$l" >/dev/null 2>&1; then launchctl kickstart -k "gui/$(id -u)/$l" && restarted="$restarted $l"; fi
  done
fi
if [ -z "$restarted" ] && pgrep -f '[r]elay service event run' >/dev/null 2>&1; then
  pkill -f '[r]elay service event run' 2>/dev/null || true
  sleep 0.3
  nohup "$R" service event run >/tmp/relay-event.log 2>&1 &
  restarted="nohup:relay-event"
fi
echo "restarted=$restarted"
echo "build=$(RELAY_BRIDGE_LOCAL_INVOKE=1 "$R" build)"
`

func fleetStageName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return ".relay.new.fleet." + hex.EncodeToString(b[:])
}

func (a *App) fleetUpgrade(ctx context.Context, r *fleetHost, o fleetOpts, bins *binCache, dir string) {
	t, err := a.tf(r.Host)
	if err != nil {
		r.Action, r.Error = "failed", err.Error()
		return
	}
	needBinary := !r.Hub && (!r.UpToDate || o.force)
	if r.Hub {
		r.Action = "hub"
	} else if !needBinary {
		r.Action = "current"
	}
	if needBinary && r.Platform == "" {
		r.Action, r.Error = "failed", fmt.Sprintf("unsupported platform %s/%s", r.OS, r.Arch)
		return
	}
	if o.dryRun {
		if needBinary {
			r.Action = fmt.Sprintf("would_upgrade %s → %s (%s)", r.InstalledBuild, coord.Build, r.Platform)
		}
		if !o.skipAgents {
			r.Action += "; would_run relay agents install"
		}
		return
	}
	if needBinary {
		bin, err := bins.get(ctx, r.Platform, dir)
		if err != nil {
			r.Action, r.Error = "failed", err.Error()
			return
		}
		data, err := os.ReadFile(bin)
		if err != nil {
			r.Action, r.Error = "failed", err.Error()
			return
		}
		uctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		stage := fleetStageName()
		if err := t.WriteFile(uctx, "~/.local/bin/"+stage, data, "755"); err != nil {
			r.Action, r.Error = "failed", "upload: "+err.Error()
			return
		}
		out, stderr, err := t.Run(uctx, "", fmt.Sprintf(fleetInstallScript, shellquote.Quote(stage), shellquote.Quote(coord.Build)))
		kv := parseKV(out)
		r.Restarted = strings.TrimSpace(kv["restarted"])
		r.AfterBuild = kv["build"]
		if err != nil || r.AfterBuild != coord.Build {
			msg := kv["error"]
			if msg == "" {
				msg = strings.TrimSpace(fmt.Sprintf("%v %s", err, firstLine(stderr)))
			}
			r.Action, r.Error = "failed", "install: "+msg
			return
		}
		r.Action = "upgraded"
		r.UpToDate = true
		r.InstalledBuild = r.AfterBuild
		r.MissingCaps = nil
	}
	if o.skipAgents {
		return
	}
	actx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, stderr, err := t.Run(actx, "", fleetResolveScript+`RELAY_BRIDGE_LOCAL_INVOKE=1 "$R" --json agents install | tr -d '\n'`)
	raw := strings.TrimSpace(out)
	if json.Valid([]byte(raw)) && strings.HasPrefix(raw, "{") {
		r.Agents = json.RawMessage(raw)
		var st struct {
			OK bool `json:"ok"`
		}
		_ = json.Unmarshal([]byte(raw), &st)
		ok := st.OK
		r.AgentsOK = &ok
		if !ok && r.Error == "" {
			r.Error = "agents install reported problems (see agents)"
		}
	} else if err != nil {
		f := false
		r.AgentsOK = &f
		r.Error = strings.TrimSpace("agents install: " + firstLine(stderr) + " " + firstLine(out))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (a *App) printFleet(verb string, o fleetOpts, results []fleetHost) int {
	code := 0
	summary := map[string]int{}
	for _, r := range results {
		switch {
		case !r.Reachable:
			summary["unreachable"]++
		case verb == "upgrade" && r.Action == "failed":
			summary["failed"]++
			code = 1
		case r.UpToDate:
			summary["up_to_date"]++
		default:
			summary["behind"]++
		}
		if r.AgentsOK != nil && !*r.AgentsOK {
			summary["agents_problems"]++
		}
	}
	// argv is the suggested next command (the global --json convention),
	// never this invocation's argv: only present when hosts are behind, and
	// scoped to exactly those hosts (#81).
	var next []string
	var behind []string
	for _, r := range results {
		if r.Reachable && !r.UpToDate && r.AliasOf == "" {
			behind = append(behind, r.Host)
		}
	}
	if verb == "status" && len(behind) > 0 {
		next = []string{"relay", "fleet", "upgrade", "--hosts", strings.Join(behind, ",")}
	}
	if a.JSON {
		payload := map[string]any{"ok": code == 0, "verb": verb, "dry_run": o.dryRun, "hub_build": coord.Build, "summary": summary, "hosts": results}
		if len(next) > 0 {
			payload["next"] = fmt.Sprintf("%d host(s) behind the hub build; upgrade them", len(behind))
			payload["argv"] = next
		}
		_ = a.out(payload)
		return code
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Host < results[j].Host })
	fmt.Printf("hub build %s  %v\n", coord.Build, summary)
	for _, r := range results {
		state := "ok"
		switch {
		case !r.Reachable:
			state = "unreachable"
		case !r.UpToDate:
			state = "behind"
		}
		agents := "-"
		if r.AgentsOK != nil {
			agents = strconv.FormatBool(*r.AgentsOK)
		}
		fmt.Printf("  %-12s %-11s %-14s build=%-12s agents_ok=%-5s %s %s\n", r.Host, state, r.Platform, r.InstalledBuild, agents, r.Action, r.Error)
	}
	if len(next) > 0 {
		fmt.Println("  next: " + joinArgv(next))
	}
	return code
}

func (a *App) fleetReport(ctx context.Context, hosts []string, o fleetOpts) int {
	type hostReport struct {
		Host    string          `json:"host"`
		OK      bool            `json:"ok"`
		AliasOf string          `json:"alias_of,omitempty"`
		Report  json.RawMessage `json:"report,omitempty"`
		Error   string          `json:"error,omitempty"`
		machine string
		hub     bool
	}
	out := make([]hostReport, len(hosts))
	sem := make(chan struct{}, o.parallel)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			hr := hostReport{Host: h, hub: core.IsThisHost(h)}
			t, err := a.tf(h)
			if err == nil {
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				var stdout, stderr string
				stdout, stderr, err = t.Run(rctx, "", fleetMachineScript+fmt.Sprintf(`RELAY_BRIDGE_LOCAL_INVOKE=1 "$HOME/.local/bin/relay" --json report --since %s --examples 5`, shellquote.Quote(o.since)))
				cancel()
				var raw string
				hr.machine, raw = splitMachineLine(stdout)
				if json.Valid([]byte(raw)) && strings.HasPrefix(raw, "{") {
					hr.OK, hr.Report = true, json.RawMessage(raw)
					err = nil
				} else if err == nil {
					err = fmt.Errorf("no report (relay on host too old?): %s", firstLine(stderr+stdout))
				} else {
					err = fmt.Errorf("%v %s", err, firstLine(stderr))
				}
			}
			if err != nil {
				hr.Error = err.Error()
			}
			out[i] = hr
		}(i, h)
	}
	wg.Wait()
	// Aliases of one machine read the same usage log; count it once,
	// preferring the hub entry (#86).
	keys := make([]string, len(out))
	hubs := make([]bool, len(out))
	for i, hr := range out {
		if hr.OK {
			keys[i] = hr.machine
		}
		hubs[i] = hr.hub
	}
	for i, p := range reportPrimaries(keys, hubs) {
		if p != i {
			out[i].AliasOf = out[p].Host
		}
	}
	totals := map[string]int{}
	var findings []string
	for _, hr := range out {
		if !hr.OK || hr.AliasOf != "" {
			continue
		}
		var r usageReport
		if json.Unmarshal(hr.Report, &r) != nil {
			continue
		}
		totals["events"] += r.Events
		totals["sends"] += r.Send.Total
		for k, v := range r.Send.ByState {
			totals["send_"+k] += v
		}
		totals["lock_contention"] += r.LockContention
		totals["gate_refusals"] += r.GateRefusals
		totals["composer_refusals"] += r.ComposerRefusals
		totals["direct"] += r.Send.Direct
		for _, f := range r.Findings {
			findings = append(findings, hr.Host+": "+f)
		}
	}
	if findings == nil {
		findings = []string{}
	}
	if a.JSON {
		return a.errOut(a.out(map[string]any{"ok": true, "since": o.since, "hub_build": coord.Build, "totals": totals, "findings": findings, "hosts": out}))
	}
	fmt.Printf("fleet report since %s: %v\n", o.since, totals)
	for _, hr := range out {
		if !hr.OK {
			fmt.Printf("  %-12s error: %s\n", hr.Host, hr.Error)
		} else if hr.AliasOf != "" {
			fmt.Printf("  %-12s alias_of %s (not counted twice)\n", hr.Host, hr.AliasOf)
		}
	}
	for _, f := range findings {
		fmt.Println("  finding: " + f)
	}
	return 0
}

// splitMachineLine separates the leading "machine=…" line printed by
// fleetMachineScript from the rest of a host's output.
func splitMachineLine(stdout string) (machine, rest string) {
	rest = strings.TrimSpace(stdout)
	if strings.HasPrefix(rest, "machine=") {
		line, tail, _ := strings.Cut(rest, "\n")
		if m := strings.TrimSpace(strings.TrimPrefix(line, "machine=")); strings.Trim(m, "|") != "" {
			machine = m
		}
		rest = strings.TrimSpace(tail)
	}
	return machine, rest
}

// reportPrimaries maps each entry to the entry counted for its machine: the
// hub entry if one resolves there, else the first listed. Entries without a
// machine key are their own primary.
func reportPrimaries(keys []string, hubs []bool) []int {
	primary := make([]int, len(keys))
	first := map[string]int{}
	for i, k := range keys {
		primary[i] = i
		if k == "" {
			continue
		}
		if p, ok := first[k]; !ok || (hubs[i] && !hubs[p]) {
			first[k] = i
		}
	}
	for i, k := range keys {
		if k != "" {
			primary[i] = first[k]
		}
	}
	return primary
}
