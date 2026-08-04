package vizbroker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/dostos/relay/internal/clientfleet"
	"github.com/dostos/relay/internal/core"
)

var serviceName = regexp.MustCompile(`^relay-viz-[A-Za-z0-9._-]+$`)

type AuthorizationResult struct {
	OK          bool   `json:"ok"`
	Service     string `json:"service"`
	Fingerprint string `json:"fingerprint"`
	Installed   bool   `json:"installed"`
	Path        string `json:"path"`
	ClientID    string `json:"client_id"`
}

// AuthorizeCommand performs the explicit human-controlled enrollment of a
// projection-only SSH key. It is intentionally not reachable through the
// authenticated agent command policy.
func AuthorizeCommand(args []string) int {
	service, keyFile, label := "", "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--service":
			i++
			if i < len(args) {
				service = args[i]
			}
		case "--public-key-file":
			i++
			if i < len(args) {
				keyFile = args[i]
			}
		case "--label":
			i++
			if i < len(args) {
				label = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "relay viz authorize: unknown argument %q\n", args[i])
			return 2
		}
	}
	result, err := authorizeClient(service, keyFile, label)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	return 0
}

func authorizeClient(service, keyFile, label string) (*AuthorizationResult, error) {
	if !serviceName.MatchString(service) {
		return nil, fmt.Errorf("valid --service relay-viz-NAME required")
	}
	canonical, fingerprint, err := validatedPublicKey(keyFile)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := ensurePrivateDir(sshDir); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(sshDir, ".relay-authorized-keys.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	path := filepath.Join(sshDir, "authorized_keys")
	raw, err := readPrivateFile(path)
	if err != nil {
		return nil, err
	}
	expected := fmt.Sprintf(`restrict,command="$HOME/.local/bin/relay viz-broker --service %s" %s relay-viz-managed`, service, canonical)
	for _, line := range strings.Split(string(raw), "\n") {
		if authorizedLineKey(line) != canonical {
			continue
		}
		if strings.TrimSpace(line) != expected {
			return nil, fmt.Errorf("public key %s already exists with different or unrestricted authorization; remove or explicitly replace that entry first", fingerprint)
		}
		client, err := clientfleet.Enroll(core.StateRoot(), "visualization", service, label, fingerprint)
		if err != nil {
			return nil, err
		}
		return &AuthorizationResult{OK: true, Service: service, Fingerprint: fingerprint, Path: path, ClientID: client.ID}, nil
	}
	if len(raw) > 4<<20 {
		return nil, fmt.Errorf("authorized_keys exceeds 4 MiB")
	}
	updated := append([]byte{}, raw...)
	if len(updated) > 0 && updated[len(updated)-1] != '\n' {
		updated = append(updated, '\n')
	}
	updated = append(updated, expected...)
	updated = append(updated, '\n')
	if err := atomicPrivateFile(path, updated); err != nil {
		return nil, err
	}
	client, err := clientfleet.Enroll(core.StateRoot(), "visualization", service, label, fingerprint)
	if err != nil {
		if rollbackErr := atomicPrivateFile(path, raw); rollbackErr != nil {
			return nil, fmt.Errorf("register client: %v; rollback authorized key: %w", err, rollbackErr)
		}
		return nil, fmt.Errorf("register client: %w (authorized key rolled back)", err)
	}
	return &AuthorizationResult{OK: true, Service: service, Fingerprint: fingerprint, Installed: true, Path: path, ClientID: client.ID}, nil
}

func validatedPublicKey(path string) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", fmt.Errorf("--public-key-file required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return "", "", fmt.Errorf("public key file must be a regular file no larger than 16 KiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var nonempty []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			nonempty = append(nonempty, line)
		}
	}
	if len(nonempty) != 1 {
		return "", "", fmt.Errorf("public key file must contain exactly one OpenSSH public key")
	}
	fields := strings.Fields(nonempty[0])
	if len(fields) < 2 || !openSSHKeyType(fields[0]) || strings.ContainsAny(fields[1], `"',`) {
		return "", "", fmt.Errorf("public key file must contain one OpenSSH public key")
	}
	out, err := exec.Command("ssh-keygen", "-lf", path).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("validate public key: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	fingerprintFields := strings.Fields(string(out))
	if len(fingerprintFields) < 2 {
		return "", "", fmt.Errorf("ssh-keygen returned no fingerprint")
	}
	return fields[0] + " " + fields[1], fingerprintFields[1], nil
}

func ensurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return os.Mkdir(path, 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a private 0700 directory", path)
	}
	return nil
}

func readPrivateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a private regular file", path)
	}
	return os.ReadFile(path)
}

func authorizedLineKey(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	for i := 0; i+1 < len(fields); i++ {
		if openSSHKeyType(fields[i]) {
			return fields[i] + " " + fields[i+1]
		}
	}
	return ""
}

func openSSHKeyType(value string) bool {
	return strings.HasPrefix(value, "ssh-") || strings.HasPrefix(value, "ecdsa-") || strings.HasPrefix(value, "sk-")
}

func atomicPrivateFile(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".authorized-keys-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	return err
}
