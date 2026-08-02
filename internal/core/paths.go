package core

import (
	"os"
	"path/filepath"
)

const (
	AppName         = "relay"
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

// ConfigRoot is the local desktop control-plane configuration directory.
func ConfigRoot() string {
	if v := os.Getenv("RELAY_CONFIG_DIR"); v != "" {
		return v
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, AppName)
}

func PolicyPath() string { return filepath.Join(ConfigRoot(), "policy.yaml") }

func SessionsPath() string             { return filepath.Join(StateRoot(), "sessions.json") }
func ProjectionOnlyMarkerPath() string { return filepath.Join(StateRoot(), ".viz-projection-only") }
func AuthorityDeletionDir() string     { return filepath.Join(StateRoot(), "authority-deletions") }
func DeletedManagerDir() string        { return filepath.Join(StateRoot(), "deleted-managers") }
func AuthorityReplacementPath() string {
	return filepath.Join(StateRoot(), "authority-replacement.json")
}
func HandoffsDir() string             { return filepath.Join(StateRoot(), "handoffs") }
func LedgerPath() string              { return filepath.Join(StateRoot(), "handoffs", "ledger.jsonl") }
func DesktopBridgeSocketPath() string { return filepath.Join(StateRoot(), "desktop-bridge.sock") }
func BridgeTokensDir() string         { return filepath.Join(StateRoot(), "bridge-tokens") }
func BridgeIdentitiesDir() string     { return filepath.Join(StateRoot(), "bridge-identities") }
func ParentInboxDir() string          { return filepath.Join(StateRoot(), "parent-inbox") }
func ParentWatchDir() string          { return filepath.Join(StateRoot(), "parent-watch") }
func ParentWatchLockPath(handoffID string) string {
	return filepath.Join(ParentWatchDir(), sanitizeID(handoffID)+".lock")
}
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
	for _, d := range []string{StateRoot(), HandoffsDir(), ProfileCacheDir(), PanesDir(), BridgeTokensDir(), BridgeIdentitiesDir(), ParentInboxDir(), ParentWatchDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func writeOwnerFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// RemoteHostProfilePath is the authoritative profile on a remote host (tilde form for ssh).
func RemoteHostProfilePath() string {
	return "~/" + RemoteConfigRel
}
