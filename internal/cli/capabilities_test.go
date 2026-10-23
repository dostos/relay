package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dostos/relay/internal/coord"
	"github.com/dostos/relay/internal/coord/sshcoord"
	"github.com/dostos/relay/internal/ports"
)

// pingTransport answers the remote event-service ping with a canned reply.
type pingTransport struct {
	reply string
	err   error
	calls int
}

func (p *pingTransport) ID() string { return "h1" }
func (p *pingTransport) Run(_ context.Context, _, command string) (string, string, error) {
	p.calls++
	if !strings.Contains(command, "service event ping") {
		return "", "", errors.New("unexpected command " + command)
	}
	return p.reply, "", p.err
}
func (p *pingTransport) RunStream(context.Context, string, string, io.Writer) error { return nil }
func (p *pingTransport) ReadFile(context.Context, string) ([]byte, error)           { return nil, nil }
func (p *pingTransport) WriteFile(context.Context, string, []byte, string) error    { return nil }
func (p *pingTransport) Interactive(context.Context, string) error                  { return nil }

func runCapabilities(t *testing.T, tr *pingTransport, args ...string) capabilityReport {
	t.Helper()
	app := &App{Coord: sshcoord.New(), JSON: true, CompactJSON: true,
		tf: func(string) (ports.Transport, error) { return tr, nil }}
	var code int
	out := captureStdout(t, func() { code = app.Run(append([]string{"--json", "capabilities"}, args...)) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var r capabilityReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	return r
}

func withBuild(t *testing.T, build string) {
	t.Helper()
	old := coord.Build
	coord.Build = build
	t.Cleanup(func() { coord.Build = old })
}

func TestCapabilitiesClientOnlyListsCompiledFeatures(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	withBuild(t, "client-new")
	r := runCapabilities(t, &pingTransport{})
	if r.Client.Build != "client-new" || r.Client.Version != coord.ReleaseVersion || r.Client.CacheKey == "" {
		t.Fatalf("client identity: %+v", r.Client)
	}
	if !contains(r.Client.Capabilities, "session.submit") || !contains(r.Client.Capabilities, "session.op.wait") {
		t.Fatalf("client capabilities missing lifecycle features: %v", r.Client.Capabilities)
	}
	if r.Daemon.State != daemonNotQueried || r.Daemon.Capabilities != nil || r.Compare != nil {
		t.Fatalf("daemon should be not_queried without -H: %+v", r.Daemon)
	}
}

// A feature that is on main but absent from the installed service binary
// must show up as missing — the gap #62 was filed for.
func TestCapabilitiesMismatchedClientAndDaemon(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	withBuild(t, "client-new")
	tr := &pingTransport{reply: `{"ok":true,"version":"0.1.0","build":"daemon-old","release":"0.1.0","capabilities":["coord.ping","coord.emit","session.send.receipt","coord.capabilities","coord.subscribe","coord.status","daemon.only"]}`}
	r := runCapabilities(t, tr, "-H", "h1")
	d := r.Daemon
	if d.State != daemonReported || d.Build != "daemon-old" || d.Host != "h1" || d.CacheKey == "" || d.CacheKey == r.Client.CacheKey {
		t.Fatalf("daemon report: %+v", d)
	}
	if contains(d.Capabilities, "session.submit") {
		t.Fatalf("daemon capabilities were filled from the client list: %v", d.Capabilities)
	}
	if r.Compare == nil || r.Compare.SameBuild {
		t.Fatalf("compare: %+v", r.Compare)
	}
	if !contains(r.Compare.MissingOnDaemon, "session.submit") || !contains(r.Compare.MissingOnDaemon, "session.op.wait") {
		t.Fatalf("missing_on_daemon: %v", r.Compare.MissingOnDaemon)
	}
	if len(r.Compare.MissingOnClient) != 1 || r.Compare.MissingOnClient[0] != "daemon.only" {
		t.Fatalf("missing_on_client: %v", r.Compare.MissingOnClient)
	}
}

// A service from before capability reporting answers ping without a list:
// report it as unreported, not as supporting (or lacking) everything.
func TestCapabilitiesOlderDaemonIsUnreported(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	tr := &pingTransport{reply: `{"ok":true,"version":"0.1.0","build":"2025-old"}`}
	r := runCapabilities(t, tr, "-H", "h1")
	if r.Daemon.State != daemonUnreported || r.Daemon.Capabilities != nil || r.Daemon.Build != "2025-old" {
		t.Fatalf("daemon report: %+v", r.Daemon)
	}
	if r.Compare == nil || r.Compare.MissingOnDaemon != nil {
		t.Fatalf("unreported daemon must not produce a missing list: %+v", r.Compare)
	}
}

func TestCapabilitiesTransportFailureIsUnknownAndNotCached(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	tr := &pingTransport{err: errors.New("ssh: connect timed out")}
	r := runCapabilities(t, tr, "-H", "h1")
	if r.Daemon.State != daemonUnknown || r.Daemon.Capabilities != nil || r.Daemon.Error == "" || r.Compare != nil {
		t.Fatalf("daemon report: %+v", r.Daemon)
	}
	runCapabilities(t, tr, "-H", "h1")
	if tr.calls != 2 {
		t.Fatalf("failed probe was cached: %d calls", tr.calls)
	}
}

func TestCapabilitiesCachedPerClientBuild(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", t.TempDir())
	withBuild(t, "b1")
	tr := &pingTransport{reply: `{"ok":true,"version":"0.1.0","build":"d1","release":"0.1.0","capabilities":["coord.ping"]}`}
	first := runCapabilities(t, tr, "-H", "h1")
	second := runCapabilities(t, tr, "-H", "h1")
	if tr.calls != 1 || first.Daemon.Cached || !second.Daemon.Cached {
		t.Fatalf("cache not used: calls=%d first=%v second=%v", tr.calls, first.Daemon.Cached, second.Daemon.Cached)
	}
	if second.Daemon.CacheKey != first.Daemon.CacheKey {
		t.Fatal("cached report differs from probed one")
	}
	runCapabilities(t, tr, "-H", "h1", "--refresh")
	if tr.calls != 2 {
		t.Fatalf("--refresh did not probe: %d", tr.calls)
	}
	coord.Build = "b2" // client upgraded: old entry no longer applies
	runCapabilities(t, tr, "-H", "h1")
	if tr.calls != 3 {
		t.Fatalf("cache survived a client build change: %d", tr.calls)
	}
	old := capabilityCacheTTL
	capabilityCacheTTL = time.Nanosecond
	t.Cleanup(func() { capabilityCacheTTL = old })
	time.Sleep(time.Millisecond)
	runCapabilities(t, tr, "-H", "h1")
	if tr.calls != 4 {
		t.Fatalf("expired cache entry was used: %d", tr.calls)
	}
}

func TestVersionJSONReportsBuildIdentity(t *testing.T) {
	withBuild(t, "abc123")
	app := &App{JSON: true}
	out := captureStdout(t, func() { app.Run([]string{"--json", "version"}) })
	var got struct {
		Client coord.Identity `json:"client"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	if got.Client.Build != "abc123" || got.Client.CacheKey != coord.LocalIdentity().CacheKey {
		t.Fatalf("version --json: %+v", got.Client)
	}
}

// Every advertised CLI capability must be a real command in this binary's
// help, so the list cannot claim something the binary does not do.
func TestCapabilitiesMatchHelp(t *testing.T) {
	help := fullHelpText
	want := map[string]string{
		"session.submit":              "relay session submit",
		"session.send.idempotency":    "--idempotency-key",
		"session.exec.detach":         "--detach",
		"session.op.status":           "relay session op status",
		"session.op.wait":             "relay session op wait",
		"session.op.output":           "relay session op output",
		"cli.capabilities":            "relay capabilities",
		"session.send.paste":          "Paste TEXT (bracketed)",
		"session.send.serialized":     "serialized on its host",
		"session.send.gate_guard":     "approval prompt is refused",
		"cli.agents":                  "relay agents install",
		"cli.fleet":                   "relay fleet upgrade",
		"cli.report":                  "relay report",
		"session.wait.idle":           "relay session wait ID --idle",
		"session.send.queued":         "queued",
		"session.send.composer_guard": "--force-append",
		"session.address":             "HOST/NAME",
		"session.direct":              "bridge is down",
	}
	for _, c := range coord.Capabilities() {
		if needle, ok := want[c]; ok && !strings.Contains(help, needle) {
			t.Errorf("capability %s advertised but help lacks %q", c, needle)
		}
	}
	for c := range want {
		if !contains(coord.Capabilities(), c) {
			t.Errorf("help documents %s but it is not in the capability list", c)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
