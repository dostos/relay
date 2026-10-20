package coord

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// ReleaseVersion is the human-facing relay version (`relay version`). Like
// Version it does not change on every rebuild, so it can never tell two
// installs apart — Build and Capabilities do that (#62).
const ReleaseVersion = "0.1.0"

// capabilities is what THIS binary implements, compiled in. It is the only
// source of truth for capability discovery: nothing is inferred from the
// repository, the docs, or ReleaseVersion, so a binary built before a feature
// landed simply does not list it. Add an entry in the same change that adds
// the feature; TestCapabilitiesMatchHelp keeps the list honest.
//
// Names are stable dotted identifiers; never rename one (automation keys on
// them), only add.
var capabilities = []string{
	"cli.capabilities",         // relay capabilities / relay version --json (#62)
	"coord.capabilities",       // event service reports these in ping/status (#62)
	"coord.emit",               // event service ops
	"coord.ping",               //
	"coord.status",             //
	"coord.subscribe",          //
	"session.exec.detach",      // durable detached operations (#60)
	"session.op.output",        //
	"session.op.status",        //
	"session.op.wait",          //
	"session.send.idempotency", // --idempotency-key replays a receipt (#59)
	"session.send.receipt",     // fresh-turn send receipts (#59)
	"session.submit",           // relay session submit (#59)
}

// Capabilities returns a sorted copy of the compiled-in capability list.
func Capabilities() []string {
	out := append([]string(nil), capabilities...)
	sort.Strings(out)
	return out
}

// Identity is a binary's build identity plus what it supports. It is
// deterministic for a given build, so CacheKey can key a cache.
type Identity struct {
	Version      string   `json:"version"`
	Build        string   `json:"build"`
	Protocol     string   `json:"protocol"`
	Capabilities []string `json:"capabilities"`
	CacheKey     string   `json:"cache_key"`
}

// LocalIdentity describes the running binary.
func LocalIdentity() Identity {
	return NewIdentity(ReleaseVersion, Build, Version, Capabilities())
}

// NewIdentity builds an Identity with a sorted capability list and its
// cache key. caps may be nil only for callers that will not use it as a
// capability report.
func NewIdentity(version, build, protocol string, caps []string) Identity {
	sorted := append([]string(nil), caps...)
	sort.Strings(sorted)
	return Identity{
		Version:      version,
		Build:        build,
		Protocol:     protocol,
		Capabilities: sorted,
		CacheKey:     CacheKey(version, build, protocol, sorted),
	}
}

// CacheKey is a short, stable digest of a build identity and its sorted
// capability list. Two binaries with the same key answer identically.
func CacheKey(version, build, protocol string, sortedCaps []string) string {
	h := sha256.New()
	h.Write([]byte(version + "\x00" + build + "\x00" + protocol + "\x00" + strings.Join(sortedCaps, ",")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}
