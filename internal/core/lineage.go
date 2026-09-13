package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dostos/relay/internal/bridge"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
)

const HomeClientSessionID = "human-local"

// BridgeRemoteSocket is the owner-only stream-local endpoint exposed inside a
// relay tmux session. The SSH attach maps it to the desktop bridge socket.
func BridgeRemoteSocket(sessionID string) string {
	return "/tmp/relay-bridge-" + sanitizeID(sessionID) + ".sock"
}

func relaySessionCommand(command, sessionID, hostID, persistName, bridgeToken string) string {
	exports := []string{
		"RELAY_SESSION_ID=" + shellquote.Quote(sessionID),
		"RELAY_SESSION_HOST=" + shellquote.Quote(hostID),
		"RELAY_SESSION_NAME=" + shellquote.Quote(persistName),
		bridge.SocketEnv + "=" + shellquote.Quote(BridgeRemoteSocket(sessionID)),
		bridge.SourceTokenEnv + "=" + shellquote.Quote(bridgeToken),
	}
	return "export " + strings.Join(exports, " ") + "; exec " + command
}

func bridgeTokenPath(sessionID string) string {
	return filepath.Join(BridgeTokensDir(), sanitizeID(sessionID)+".token")
}

// BridgeIdentity is the owner-only fallback used by already-running adopted
// tmux sessions. New Relay sessions also receive it so nested handoffs keep
// working after an agent replaces itself without inheriting the launch env.
type BridgeIdentity struct {
	V           int    `json:"v"`
	SessionID   string `json:"session_id"`
	HostID      string `json:"host_id"`
	PersistName string `json:"persist_name"`
	Socket      string `json:"socket"`
	Token       string `json:"token"`
}

func remoteBridgeIdentityPath(sessionID string) string {
	return "~/" + RemoteStateRel + "/bridge-identities/" + sanitizeID(sessionID) + ".json"
}

func bridgeIdentityPath(sessionID string) string {
	return filepath.Join(BridgeIdentitiesDir(), sanitizeID(sessionID)+".json")
}

func provisionBridgeIdentity(ctx context.Context, t ports.Transport, sess *Session, token string) error {
	if sess == nil || t == nil || token == "" {
		return fmt.Errorf("bridge identity requires session, transport, and token")
	}
	identity := BridgeIdentity{
		V: 1, SessionID: sess.ID, HostID: sess.HostID, PersistName: sess.Persist.Name,
		Socket: BridgeRemoteSocket(sess.ID), Token: token,
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	return t.WriteFile(ctx, remoteBridgeIdentityPath(sess.ID), raw, "600")
}

func clearBridgeIdentity(ctx context.Context, t ports.Transport, sessionID string) {
	// Overwrite instead of shell-removing: this revokes the secret while keeping
	// cleanup inside Transport's bounded file API.
	_ = t.WriteFile(ctx, remoteBridgeIdentityPath(sessionID), []byte("{}\n"), "600")
}

// LoadBridgeIdentityForCurrentPane discovers the tmux session containing the
// caller, then loads only that session's owner-readable identity. It is used
// when an adopted/rerolled agent process lacks Relay's original launch env.
func LoadBridgeIdentityForCurrentPane() (*BridgeIdentity, error) {
	pane := strings.TrimSpace(os.Getenv("TMUX_PANE"))
	if pane == "" {
		return nil, fmt.Errorf("not running in tmux")
	}
	out, err := exec.Command("tmux", "display-message", "-p", "-t", pane, "#{session_name}").Output()
	if err != nil {
		return nil, err
	}
	return loadBridgeIdentityForPersist(strings.TrimSpace(string(out)))
}

func loadBridgeIdentityForPersist(persistName string) (*BridgeIdentity, error) {
	entries, err := os.ReadDir(BridgeIdentitiesDir())
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(BridgeIdentitiesDir(), entry.Name()))
		if readErr != nil {
			continue
		}
		var identity BridgeIdentity
		if json.Unmarshal(raw, &identity) == nil && identity.PersistName == persistName && identity.SessionID != "" && identity.Socket != "" && identity.Token != "" {
			return &identity, nil
		}
	}
	return nil, fmt.Errorf("no bridge identity for tmux session %q", persistName)
}

func rememberBridgeToken(sessionID, token string) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	if sessionID == "" || token == "" {
		return fmt.Errorf("bridge session and token required")
	}
	if err := EnsureStateDirs(); err != nil {
		return err
	}
	return os.WriteFile(bridgeTokenPath(sessionID), []byte(token), 0o600)
}

func forgetBridgeToken(sessionID string) {
	_ = forgetBridgeTokenChecked(sessionID)
}

func forgetBridgeTokenChecked(sessionID string) error {
	if EnsureAuthorityWritable() != nil {
		return EnsureAuthorityWritable()
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	err = os.Remove(bridgeTokenPath(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// AuthorizeBridgeSource binds a forwarded request to the unguessable token
// injected into its originating tmux session.
func AuthorizeBridgeSource(source bridge.Source) error {
	if source.SessionID == HomeClientSessionID {
		raw, err := os.ReadFile(HomeClientTokenPath())
		if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(raw))), []byte(source.Token)) != 1 {
			return fmt.Errorf("relay home client identity rejected")
		}
		return nil
	}
	if source.SessionID == "" || source.Token == "" {
		return fmt.Errorf("relay bridge source identity missing")
	}
	raw, err := os.ReadFile(bridgeTokenPath(source.SessionID))
	if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(raw))), []byte(source.Token)) != 1 {
		return fmt.Errorf("relay bridge source identity rejected")
	}
	if _, err := (&Registry{}).GetSession(source.SessionID); err != nil {
		return fmt.Errorf("relay bridge source session is not active locally")
	}
	return nil
}

// EnsureHomeClientIdentity creates the owner-only credential used by local
// stateless CLI requests. Unix socket permissions plus this token bind the
// request to the same OS account without inventing a hierarchy session.
func EnsureHomeClientIdentity() (bridge.Source, error) {
	if err := EnsureStateDirs(); err != nil {
		return bridge.Source{}, err
	}
	if identity, err := LoadHomeClientIdentity(); err == nil {
		return identity, nil
	} else if _, statErr := os.Lstat(HomeClientTokenPath()); statErr == nil {
		return bridge.Source{}, err
	} else if !os.IsNotExist(statErr) {
		return bridge.Source{}, statErr
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return bridge.Source{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	file, err := os.OpenFile(HomeClientTokenPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return LoadHomeClientIdentity()
	}
	if err != nil {
		return bridge.Source{}, err
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return bridge.Source{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return bridge.Source{}, err
	}
	if err := file.Close(); err != nil {
		return bridge.Source{}, err
	}
	return bridge.Source{SessionID: HomeClientSessionID, HostID: LocalHostID, Token: token}, nil
}

func LoadHomeClientIdentity() (bridge.Source, error) {
	path := HomeClientTokenPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return bridge.Source{}, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return bridge.Source{}, fmt.Errorf("invalid home client credential at %s", path)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 {
		return bridge.Source{}, fmt.Errorf("invalid home client credential at %s", path)
	}
	return bridge.Source{SessionID: HomeClientSessionID, HostID: LocalHostID, Token: token}, nil
}
