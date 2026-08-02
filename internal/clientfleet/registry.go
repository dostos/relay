// Package clientfleet tracks restricted relayd clients independently of the
// optional service (Viz, notifications, or otherwise) that uses their stream.
package clientfleet

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dostos/relay/internal/coord"
	coordrelayd "github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/shellquote"
)

type Client struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Kind        string `json:"kind"`
	Channel     string `json:"channel"`
	Fingerprint string `json:"fingerprint,omitempty"`
	EnrolledAt  string `json:"enrolled_at"`
}

type ClientSummary struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	Kind       string `json:"kind"`
	EnrolledAt string `json:"enrolled_at"`
}

func Summaries(clients []Client) []ClientSummary {
	out := make([]ClientSummary, 0, len(clients))
	for _, c := range clients {
		out = append(out, ClientSummary{ID: c.ID, Label: c.Label, Kind: c.Kind, EnrolledAt: c.EnrolledAt})
	}
	return out
}

type registry struct {
	V       int      `json:"v"`
	Clients []Client `json:"clients"`
}

type UpdateStatus struct {
	ClientID       string `json:"client_id"`
	Label          string `json:"label,omitempty"`
	Kind           string `json:"kind"`
	Channel        string `json:"-"`
	RequestSeq     int64  `json:"request_seq,omitempty"`
	RequestedBuild string `json:"requested_build,omitempty"`
	RequestedAt    string `json:"requested_at,omitempty"`
	AckedAt        string `json:"acked_at,omitempty"`
	InstalledBuild string `json:"installed_build,omitempty"`
	State          string `json:"state"`
}

type QueueOutcome struct {
	Seq   int64  `json:"seq,omitempty"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

func path(root string) string { return filepath.Join(root, "clients.json") }

func List(root string) ([]Client, error) {
	lock, err := lockRegistry(root)
	if err != nil {
		return nil, err
	}
	defer unlockRegistry(lock)
	return listUnlocked(root)
}

func listUnlocked(root string) ([]Client, error) {
	raw, err := os.ReadFile(path(root))
	if os.IsNotExist(err) {
		if err := migrateLegacy(root); err != nil {
			return nil, err
		}
		raw, err = os.ReadFile(path(root))
		if os.IsNotExist(err) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	var r registry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.V != 1 {
		return nil, fmt.Errorf("unsupported client registry version %d", r.V)
	}
	for _, c := range r.Clients {
		if err := validateClient(c); err != nil {
			return nil, err
		}
	}
	sort.Slice(r.Clients, func(i, j int) bool { return r.Clients[i].ID < r.Clients[j].ID })
	return r.Clients, nil
}

var legacyChannel = regexp.MustCompile(`^restrict,command="\$HOME/\.local/bin/relayd viz-broker --service (relay-viz-[A-Za-z0-9._-]+)" (.+)$`)

// migrateLegacy is a one-time import for installations enrolled before the
// generic client registry existed. authorized_keys is never consulted again
// after clients.json has been created.
func migrateLegacy(root string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	channels := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := legacyChannel.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) != 3 {
			continue
		}
		fields := strings.Fields(m[2])
		if len(fields) != 3 || fields[2] != "relay-viz-managed" || !supportedKeyType(fields[0]) {
			continue
		}
		if decoded, err := base64.StdEncoding.DecodeString(fields[1]); err != nil || !validKeyBlob(fields[0], decoded) {
			continue
		}
		channels[m[1]] = true
	}
	if len(channels) == 0 {
		return nil
	}
	clients := make([]Client, 0, len(channels))
	for channel := range channels {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		clients = append(clients, Client{ID: "client-" + hex.EncodeToString(b), Label: DefaultLabel("visualization"), Kind: "visualization", Channel: channel, EnrolledAt: time.Now().UTC().Format(time.RFC3339)})
	}
	return save(root, clients)
}

func supportedKeyType(value string) bool {
	switch value {
	case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return true
	default:
		return false
	}
}

func validKeyBlob(kind string, raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	n := int(binary.BigEndian.Uint32(raw[:4]))
	return n == len(kind) && len(raw) >= 4+n && string(raw[4:4+n]) == kind
}

// Enroll records a transport channel under an opaque client identity. Reusing
// the same channel preserves its identity across idempotent authorization.
func Enroll(root, kind, channel, label, fingerprint string) (Client, error) {
	lock, err := lockRegistry(root)
	if err != nil {
		return Client{}, err
	}
	defer unlockRegistry(lock)
	clients, err := listUnlocked(root)
	if err != nil {
		return Client{}, err
	}
	for i := range clients {
		if clients[i].Channel == channel {
			if clients[i].Fingerprint != "" && fingerprint != "" && clients[i].Fingerprint != fingerprint {
				return Client{}, fmt.Errorf("channel %s is enrolled with a different key", channel)
			}
			if label != "" {
				clients[i].Label = label
			}
			if fingerprint != "" {
				clients[i].Fingerprint = fingerprint
			}
			if err := save(root, clients); err != nil {
				return Client{}, err
			}
			return clients[i], nil
		}
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return Client{}, err
	}
	c := Client{ID: "client-" + hex.EncodeToString(b), Label: label, Kind: kind, Channel: channel, Fingerprint: fingerprint, EnrolledAt: time.Now().UTC().Format(time.RFC3339)}
	if err := validateClient(c); err != nil {
		return Client{}, err
	}
	clients = append(clients, c)
	if err := save(root, clients); err != nil {
		return Client{}, err
	}
	return c, nil
}

func lockRegistry(root string) (*os.File, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, "clients.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func unlockRegistry(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

var clientIDPattern = regexp.MustCompile(`^client-[a-f0-9]{16}$`)
var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func validateClient(c Client) error {
	if !clientIDPattern.MatchString(c.ID) {
		return fmt.Errorf("invalid client id %q", c.ID)
	}
	if !kindPattern.MatchString(c.Kind) {
		return fmt.Errorf("invalid client kind %q", c.Kind)
	}
	if err := shellquote.ValidateSessionName(c.Channel); err != nil {
		return fmt.Errorf("invalid client channel: %w", err)
	}
	if len(c.Label) > 128 || len(c.Fingerprint) > 256 {
		return fmt.Errorf("client metadata exceeds limits")
	}
	if strings.ContainsAny(c.Label+c.Fingerprint, "\r\n\x00") {
		return fmt.Errorf("client metadata contains control characters")
	}
	return nil
}

func save(root string, clients []Client) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(registry{V: 1, Clients: clients}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(root, ".clients-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, path(root)); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	return err
}

// QueueUpdate sends the lifecycle request to every enrolled client, or one
// explicitly selected opaque identity. Delivery success is reported later by
// Status, never inferred from the append succeeding.
func QueueUpdate(root, socket, selected string) (map[string]QueueOutcome, error) {
	clients, err := List(root)
	if err != nil {
		return nil, err
	}
	result := map[string]QueueOutcome{}
	found := selected == ""
	for _, c := range clients {
		if selected != "" && c.ID != selected {
			continue
		}
		found = true
		resp, err := coordrelayd.EmitLocal(socket, c.Channel, "update_relayd", map[string]any{"client_id": c.ID})
		if err != nil {
			result[c.ID] = QueueOutcome{State: "error", Error: err.Error()}
			continue
		}
		result[c.ID] = QueueOutcome{Seq: resp.Seq, State: "queued"}
	}
	if !found {
		return nil, fmt.Errorf("unknown client %q", selected)
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("no relayd clients enrolled")
	}
	return result, nil
}

func Status(root string) ([]UpdateStatus, error) {
	clients, err := List(root)
	if err != nil {
		return nil, err
	}
	statuses := make([]UpdateStatus, 0, len(clients))
	for _, c := range clients {
		s := UpdateStatus{ClientID: c.ID, Label: c.Label, Kind: c.Kind, Channel: c.Channel, State: "never_requested"}
		request, err := latestEvent(filepath.Join(root, "events", c.Channel+".jsonl"), func(e coord.Event) bool { return e.Kind == "update_relayd" })
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read requests for %s: %w", c.ID, err)
		}
		if request.Seq > 0 {
			s.RequestSeq, s.RequestedAt, s.State = request.Seq, request.TS, "pending"
			if request.Meta != nil {
				s.RequestedBuild, _ = request.Meta["requested_build"].(string)
			}
			ack, err := latestEvent(filepath.Join(root, "events", c.Channel+"-ack.jsonl"), func(e coord.Event) bool {
				return (e.Kind == "client_ack" || e.Kind == "viz_ack") && number(e.Meta["request_seq"]) == request.Seq && e.Meta["request_kind"] == "update_relayd"
			})
			if err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("read acknowledgements for %s: %w", c.ID, err)
			}
			if ack.Seq > 0 {
				s.AckedAt = ack.TS
				s.InstalledBuild, _ = ack.Meta["result"].(string)
				if ack.Kind == "client_ack" {
					s.State = "active"
				} else {
					s.State = "installed_unverified"
				}
			}
		}
		statuses = append(statuses, s)
	}
	return statuses, nil
}

func latestEvent(path string, match func(coord.Event) bool) (coord.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return coord.Event{}, err
	}
	defer f.Close()
	var latest coord.Event
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for s.Scan() {
		line++
		var e coord.Event
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			return coord.Event{}, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if match(e) {
			latest = e
		}
	}
	return latest, s.Err()
}

func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func DefaultLabel(kind string) string {
	label := strings.ReplaceAll(kind, "_", " ")
	if label == "" {
		return "Relay client"
	}
	return strings.ToUpper(label[:1]) + label[1:] + " client"
}
