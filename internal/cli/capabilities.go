package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

// Capability discovery (#62).
//
// A merged source change and the binaries actually installed are different
// things: the client on PATH and the event service on a host can each be
// older than main. `relay capabilities` reports what each running binary
// says about itself — never what the repository or docs say — so automation
// can check for a feature before relying on it.
//
// "client" is the relay that would execute this invocation's commands: like
// every other command, capabilities is forwarded to the desktop bridge when
// run inside a relay pane, so it describes the binary that actually serves
// `session submit` and friends there (any bridge/pane build drift is already
// warned about on stderr by the forwarder).

// Daemon capability states.
const (
	daemonNotQueried = "not_queried" // no -H given
	daemonReported   = "reported"    // service answered with its capability list
	daemonUnreported = "unreported"  // service answered but predates capability reporting
	daemonUnknown    = "unknown"     // transport/ping failed; nothing is known
)

// capabilityCacheTTL bounds how long a host's report is reused without a
// probe. The entry is also dropped whenever the local build changes.
var capabilityCacheTTL = 5 * time.Minute

type daemonCapabilities struct {
	Host         string   `json:"host,omitempty"`
	State        string   `json:"state"`
	Version      string   `json:"version,omitempty"`
	Build        string   `json:"build,omitempty"`
	Protocol     string   `json:"protocol,omitempty"`
	Capabilities []string `json:"capabilities"` // null unless State == reported
	CacheKey     string   `json:"cache_key,omitempty"`
	Error        string   `json:"error,omitempty"`
	Cached       bool     `json:"cached,omitempty"`
	ProbedAt     string   `json:"probed_at,omitempty"`
}

type capabilityComparison struct {
	SameBuild bool `json:"same_build"`
	// MissingOnDaemon lists client capabilities the daemon did not report.
	MissingOnDaemon []string `json:"missing_on_daemon"`
	// MissingOnClient lists daemon capabilities this client lacks.
	MissingOnClient []string `json:"missing_on_client"`
}

type capabilityReport struct {
	OK      bool                  `json:"ok"`
	Client  coord.Identity        `json:"client"`
	Daemon  daemonCapabilities    `json:"daemon"`
	Compare *capabilityComparison `json:"compare,omitempty"`
}

// daemonFromPing turns a ping reply (or its failure) into a daemon report.
// Only a reply that carries a capability list yields capabilities; anything
// else leaves them null so nothing downstream mistakes the client's list for
// the daemon's.
func daemonFromPing(host string, resp *coord.Response, err error) daemonCapabilities {
	d := daemonCapabilities{Host: host}
	if err != nil || resp == nil {
		d.State = daemonUnknown
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Error = "no response"
		}
		return d
	}
	d.Version = resp.Release
	d.Protocol = resp.Version
	d.Build = resp.Build
	if d.Build == "" {
		d.Build = "unstamped"
	}
	if resp.Capabilities == nil {
		d.State = daemonUnreported
		return d
	}
	id := coord.NewIdentity(resp.Release, d.Build, resp.Version, resp.Capabilities)
	d.State = daemonReported
	d.Capabilities = id.Capabilities
	d.CacheKey = id.CacheKey
	return d
}

func compareCapabilities(client coord.Identity, d daemonCapabilities) *capabilityComparison {
	if d.State != daemonReported && d.State != daemonUnreported {
		return nil
	}
	cmp := &capabilityComparison{SameBuild: d.Build == client.Build}
	if d.State != daemonReported {
		// Unreported: we cannot say what is missing, so say nothing rather
		// than claim the daemon lacks everything.
		cmp.MissingOnDaemon, cmp.MissingOnClient = nil, nil
		return cmp
	}
	cmp.MissingOnDaemon = setDiff(client.Capabilities, d.Capabilities)
	cmp.MissingOnClient = setDiff(d.Capabilities, client.Capabilities)
	return cmp
}

func setDiff(a, b []string) []string {
	have := make(map[string]bool, len(b))
	for _, x := range b {
		have[x] = true
	}
	out := []string{}
	for _, x := range a {
		if !have[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

type capabilityCacheEntry struct {
	ClientBuild string             `json:"client_build"`
	Daemon      daemonCapabilities `json:"daemon"`
}

func capabilityCachePath(host string) string {
	return filepath.Join(core.StateRoot(), "capabilities", host+".json")
}

func loadCapabilityCache(host string, now time.Time) (daemonCapabilities, bool) {
	b, err := os.ReadFile(capabilityCachePath(host))
	if err != nil {
		return daemonCapabilities{}, false
	}
	var e capabilityCacheEntry
	if json.Unmarshal(b, &e) != nil || e.ClientBuild != coord.Build {
		return daemonCapabilities{}, false
	}
	probed, err := time.Parse(time.RFC3339, e.Daemon.ProbedAt)
	if err != nil || now.Sub(probed) > capabilityCacheTTL || now.Before(probed) {
		return daemonCapabilities{}, false
	}
	if e.Daemon.State != daemonReported && e.Daemon.State != daemonUnreported {
		return daemonCapabilities{}, false
	}
	e.Daemon.Cached = true
	return e.Daemon, true
}

func storeCapabilityCache(host string, d daemonCapabilities) {
	// A failed probe is never cached: "unknown" must be re-asked next time.
	if d.State != daemonReported && d.State != daemonUnreported {
		return
	}
	path := capabilityCachePath(host)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	b, err := json.Marshal(capabilityCacheEntry{ClientBuild: coord.Build, Daemon: d})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

type remotePinger interface {
	RemotePing(context.Context, ports.Transport) (*coord.Response, error)
}

func (a *App) probeDaemon(ctx context.Context, host string) daemonCapabilities {
	pinger, ok := a.Coord.(remotePinger)
	if !ok || a.tf == nil {
		return daemonFromPing(host, nil, fmt.Errorf("coordination backend cannot report daemon capabilities"))
	}
	t, err := a.tf(host)
	if err != nil {
		return daemonFromPing(host, nil, err)
	}
	resp, err := pinger.RemotePing(ctx, t)
	return daemonFromPing(host, resp, err)
}

// cmdCapabilities: relay capabilities [-H HOST] [--refresh]
func (a *App) cmdCapabilities(ctx context.Context, args []string) int {
	host, rest := flagHost(args)
	refresh := false
	for _, x := range rest {
		switch x {
		case "--refresh":
			refresh = true
		case "-h", "--help":
			fmt.Println("usage: relay capabilities [-H HOST] [--refresh]")
			return 0
		default:
			return a.fail(rejectUnknownFlag(x))
		}
	}
	if host != "" && !safeCacheName(host) {
		// The transport factory validates the alias itself; this only keeps
		// the host usable as a cache file name.
		return a.fail(fmt.Errorf("invalid host %q", host))
	}
	report := capabilityReport{OK: true, Client: coord.LocalIdentity()}
	switch {
	case host == "":
		report.Daemon = daemonCapabilities{State: daemonNotQueried}
	default:
		now := time.Now().UTC()
		d, hit := daemonCapabilities{}, false
		if !refresh {
			d, hit = loadCapabilityCache(host, now)
		}
		if !hit {
			d = a.probeDaemon(ctx, host)
			d.ProbedAt = now.Format(time.RFC3339)
			storeCapabilityCache(host, d)
		}
		report.Daemon = d
		report.Compare = compareCapabilities(report.Client, d)
	}
	if !a.JSON {
		fmt.Print(formatCapabilityReport(report))
		return 0
	}
	a.CompactJSON = true // one line: cheap to cache and diff by cache_key
	return a.errOut(a.out(report))
}

func formatCapabilityReport(r capabilityReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "client  relay %s build %s protocol %s\n", r.Client.Version, r.Client.Build, r.Client.Protocol)
	fmt.Fprintf(&b, "        %s\n", strings.Join(r.Client.Capabilities, " "))
	d := r.Daemon
	switch d.State {
	case daemonNotQueried:
		b.WriteString("daemon  not queried (pass -H HOST)\n")
	case daemonUnknown:
		fmt.Fprintf(&b, "daemon  %s: unknown (%s)\n", d.Host, d.Error)
	case daemonUnreported:
		fmt.Fprintf(&b, "daemon  %s: build %s; capabilities not reported (older service)\n", d.Host, d.Build)
	default:
		fmt.Fprintf(&b, "daemon  %s: relay %s build %s protocol %s\n", d.Host, d.Version, d.Build, d.Protocol)
		fmt.Fprintf(&b, "        %s\n", strings.Join(d.Capabilities, " "))
	}
	if c := r.Compare; c != nil && len(c.MissingOnDaemon) > 0 {
		fmt.Fprintf(&b, "missing on daemon: %s\n", strings.Join(c.MissingOnDaemon, " "))
	}
	if c := r.Compare; c != nil && len(c.MissingOnClient) > 0 {
		fmt.Fprintf(&b, "missing on client: %s\n", strings.Join(c.MissingOnClient, " "))
	}
	return b.String()
}

func safeCacheName(host string) bool {
	if host == "" || host == "." || host == ".." || len(host) > 255 {
		return false
	}
	for _, r := range host {
		if !(r == '-' || r == '_' || r == '.' || r == '@' || r == ':' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
