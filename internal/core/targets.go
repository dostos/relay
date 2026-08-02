package core

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Target is one usable SSH Host alias from the user's OpenSSH config.
type Target struct {
	HostID        string `json:"host_id"`
	Hostname      string `json:"hostname,omitempty"`
	User          string `json:"user,omitempty"`
	Port          int    `json:"port,omitempty"`
	ProxyJump     bool   `json:"proxy_jump"`
	IdentityFile  bool   `json:"identity_file"`
	HasRelayCache bool   `json:"has_relay_cache"`
	ConfigFile    string `json:"config_file,omitempty"`
}

// ListTargets parses OpenSSH config (default ~/.ssh/config + Include) and
// returns concrete Host aliases suitable for relay -H.
func ListTargets() ([]Target, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".ssh", "config")
	blocks, err := parseSSHConfigFiles([]string{root}, map[string]bool{})
	if err != nil {
		return nil, err
	}
	// First Host match wins (OpenSSH semantics); skip later duplicates.
	seen := map[string]bool{}
	var out []Target
	for _, b := range blocks {
		for _, alias := range b.aliases {
			if !usableHostAlias(alias) || seen[alias] {
				continue
			}
			seen[alias] = true
			t := Target{
				HostID:        alias,
				Hostname:      b.hostname,
				User:          b.user,
				Port:          b.port,
				ProxyJump:     b.proxyJump,
				IdentityFile:  b.identityFile,
				HasRelayCache: fileExists(ProfileCachePath(alias)),
				ConfigFile:    b.source,
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// ResolveTarget expands one exact authority-side SSH alias into public
// connection coordinates. Credentials and identity paths remain client-local.
func ResolveTarget(hostID string) (*Target, error) {
	if !usableHostAlias(hostID) {
		return nil, fmt.Errorf("invalid SSH target alias %q", hostID)
	}
	targets, err := ListTargets()
	if err != nil {
		return nil, err
	}
	for _, alias := range []string{hostID} {
		configured := false
		for i := range targets {
			configured = configured || targets[i].HostID == alias
		}
		if !configured {
			continue
		}
		resolved, err := resolveEffectiveSSHTarget(alias)
		if err != nil {
			return nil, err
		}
		resolved.HostID = hostID
		return resolved, nil
	}
	return nil, fmt.Errorf("SSH target %q is absent from authority config", hostID)
}

func resolveEffectiveSSHTarget(alias string) (*Target, error) {
	out, err := exec.Command("ssh", "-G", "--", alias).Output()
	if err != nil {
		return nil, fmt.Errorf("resolve SSH target %q: %w", alias, err)
	}
	resolved := &Target{HostID: alias}
	for _, line := range strings.Split(string(out), "\n") {
		key, value := splitSSHKeyword(line)
		switch strings.ToLower(key) {
		case "hostname":
			resolved.Hostname = value
		case "user":
			resolved.User = value
		case "port":
			port, parseErr := strconv.Atoi(value)
			if parseErr != nil || port <= 0 || port > 65535 {
				return nil, fmt.Errorf("SSH target %q has invalid port", alias)
			}
			resolved.Port = port
		case "proxyjump", "proxycommand":
			if value != "" && !strings.EqualFold(value, "none") {
				return nil, fmt.Errorf("SSH target %q requires unsupported proxy routing", alias)
			}
		}
	}
	if resolved.Hostname == "" || resolved.Port == 0 {
		return nil, fmt.Errorf("SSH target %q resolved incompletely", alias)
	}
	return resolved, nil
}

type sshHostBlock struct {
	aliases      []string
	hostname     string
	user         string
	port         int
	proxyJump    bool
	identityFile bool
	source       string
}

func parseSSHConfigFiles(paths []string, seen map[string]bool) ([]sshHostBlock, error) {
	var all []sshHostBlock
	for _, path := range paths {
		if path == "" {
			continue
		}
		expanded := expandHome(path)
		abs, err := filepath.Abs(expanded)
		if err != nil {
			abs = expanded
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		blocks, includes, err := parseOneSSHConfig(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		all = append(all, blocks...)
		more, err := parseSSHConfigFiles(includes, seen)
		if err != nil {
			return nil, err
		}
		all = append(all, more...)
	}
	return all, nil
}

func parseOneSSHConfig(path string) (blocks []sshHostBlock, includes []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var cur *sshHostBlock
	flush := func() {
		if cur != nil && len(cur.aliases) > 0 {
			blocks = append(blocks, *cur)
		}
		cur = nil
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// strip inline comments
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
			if line == "" {
				continue
			}
		}
		key, val := splitSSHKeyword(line)
		if key == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			flush()
			cur = &sshHostBlock{source: path}
			for _, a := range strings.Fields(val) {
				cur.aliases = append(cur.aliases, a)
			}
		case "match":
			// Match blocks are not expanded; skip until next Host/Match.
			flush()
			cur = nil
		case "include":
			for _, pat := range strings.Fields(val) {
				includes = append(includes, expandSSHInclude(pat)...)
			}
		default:
			if cur == nil {
				continue
			}
			switch strings.ToLower(key) {
			case "hostname":
				cur.hostname = val
			case "user":
				cur.user = val
			case "port":
				if port, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && port > 0 && port <= 65535 {
					cur.port = port
				} else {
					cur.port = -1
				}
			case "proxyjump", "proxycommand":
				if strings.TrimSpace(val) != "" && !strings.EqualFold(val, "none") {
					cur.proxyJump = true
				}
			case "identityfile":
				if strings.TrimSpace(val) != "" {
					cur.identityFile = true
				}
			}
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	return blocks, includes, nil
}

func splitSSHKeyword(line string) (key, val string) {
	// Keyword Value — whitespace or = separator
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	if i := strings.IndexByte(line, '='); i >= 0 && !strings.ContainsAny(line[:i], " \t") {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", ""
	}
	if len(fields) == 1 {
		return fields[0], ""
	}
	return fields[0], strings.TrimSpace(line[len(fields[0]):])
}

func usableHostAlias(alias string) bool {
	if alias == "" || alias == "*" {
		return false
	}
	if strings.ContainsAny(alias, "*?") {
		return false
	}
	if strings.EqualFold(alias, "github.com") {
		return false
	}
	return true
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	return p
}

func expandSSHInclude(pat string) []string {
	pat = expandHome(pat)
	matches, err := filepath.Glob(pat)
	if err != nil || len(matches) == 0 {
		// Glob may fail on literal paths without meta; try as-is.
		if fileExists(pat) {
			return []string{pat}
		}
		return nil
	}
	return matches
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// FormatTargetsText is a one-line-per-alias human summary.
func FormatTargetsText(targets []Target) string {
	if len(targets) == 0 {
		return "(no Host aliases found in ssh config)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "targets  %d\n", len(targets))
	for _, t := range targets {
		flags := []string{}
		if t.ProxyJump {
			flags = append(flags, "jump")
		}
		if t.IdentityFile {
			flags = append(flags, "key")
		}
		if t.HasRelayCache {
			flags = append(flags, "cached")
		}
		extra := ""
		if len(flags) > 0 {
			extra = "  ·  " + strings.Join(flags, ", ")
		}
		host := t.Hostname
		if host == "" {
			host = "-"
		}
		user := t.User
		if user == "" {
			user = "-"
		}
		fmt.Fprintf(&b, "  %-16s %s@%s%s\n", t.HostID, user, host, extra)
	}
	return b.String()
}
