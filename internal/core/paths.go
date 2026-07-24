package core

import (
	"os"
	"path/filepath"
)

const (
	AppName        = "relay"
	RemoteConfigRel = ".config/relay/host.yaml"
	RemoteStateRel  = ".local/state/relay"
)

// StateRoot is the local XDG state directory for relay.
func StateRoot() string {
	if v := os.Getenv("RELAY_STATE_DIR"); v != "" {
		return v
	}
	xdg := os.Getenv("XDG_STATE_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(xdg, AppName)
}

func SessionsPath() string  { return filepath.Join(StateRoot(), "sessions.json") }
func HandoffsDir() string   { return filepath.Join(StateRoot(), "handoffs") }
func LedgerPath() string    { return filepath.Join(StateRoot(), "handoffs", "ledger.jsonl") }
func ProfileCacheDir() string {
	return filepath.Join(StateRoot(), "hosts")
}
func ProfileCachePath(hostID string) string {
	return filepath.Join(ProfileCacheDir(), sanitizeID(hostID)+".json")
}

func sanitizeID(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		} else {
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "host"
	}
	return string(b)
}

// EnsureStateDirs creates local state directories.
func EnsureStateDirs() error {
	for _, d := range []string{StateRoot(), HandoffsDir(), ProfileCacheDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// RemoteHostProfilePath is the authoritative profile on a remote host (tilde form for ssh).
func RemoteHostProfilePath() string {
	return "~/" + RemoteConfigRel
}
