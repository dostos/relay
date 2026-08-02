package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var vizServiceName = regexp.MustCompile(`^relay-viz-[A-Za-z0-9._-]+$`)

type vizAuthorizationResult struct {
	OK          bool   `json:"ok"`
	Service     string `json:"service"`
	Fingerprint string `json:"fingerprint"`
	Installed   bool   `json:"installed"`
	Path        string `json:"path"`
}

func cmdVizAuthorize(args []string) int {
	service, keyFile := "", ""
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
		default:
			fmt.Fprintf(os.Stderr, "relayd viz authorize: unknown argument %q\n", args[i])
			return 2
		}
	}
	result, err := authorizeVizKey(service, keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	return 0
}

func authorizeVizKey(service, keyFile string) (*vizAuthorizationResult, error) {
	if !vizServiceName.MatchString(service) {
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
	if err := ensurePrivateSSHDir(sshDir); err != nil {
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
	raw, err := readAuthorizedKeys(path)
	if err != nil {
		return nil, err
	}
	expected := fmt.Sprintf(`restrict,command="$HOME/.local/bin/relayd viz-broker --service %s" %s relay-viz-managed`, service, canonical)
	for _, line := range strings.Split(string(raw), "\n") {
		if authorizedLineKey(line) != canonical {
			continue
		}
		if strings.TrimSpace(line) == expected {
			return &vizAuthorizationResult{OK: true, Service: service, Fingerprint: fingerprint, Installed: false, Path: path}, nil
		}
		return nil, fmt.Errorf("public key %s already exists with different or unrestricted authorization; remove or explicitly replace that entry first", fingerprint)
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
	if err := atomicAuthorizedKeys(path, updated); err != nil {
		return nil, err
	}
	return &vizAuthorizationResult{OK: true, Service: service, Fingerprint: fingerprint, Installed: true, Path: path}, nil
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
	nonempty := make([]string, 0, 1)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			nonempty = append(nonempty, line)
		}
	}
	if len(nonempty) != 1 {
		return "", "", fmt.Errorf("public key file must contain exactly one OpenSSH public key")
	}
	lines := strings.Fields(nonempty[0])
	if len(lines) < 2 || !openSSHKeyType(lines[0]) || strings.ContainsAny(lines[1], `"',`) {
		return "", "", fmt.Errorf("public key file must contain one OpenSSH public key")
	}
	out, err := exec.Command("ssh-keygen", "-lf", path).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("validate public key: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", "", fmt.Errorf("ssh-keygen returned no fingerprint")
	}
	return lines[0] + " " + lines[1], fields[1], nil
}

func ensurePrivateSSHDir(path string) error {
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

func readAuthorizedKeys(path string) ([]byte, error) {
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

func atomicAuthorizedKeys(path string, raw []byte) error {
	tmp := path + ".relay.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	defer cleanup()
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
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
