// Package shellquote provides safe shell quoting and session-name validation.
package shellquote

import (
	"fmt"
	"regexp"
	"strings"
)

var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var eventKindRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Quote returns a single-quoted shell string safe against injection.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PathExpr returns a shell expression for a remote path.
// ~/rest becomes "$HOME"+Quote("/rest") so metacharacters in rest cannot execute.
func PathExpr(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if p == "~" {
		return `"$HOME"`, nil
	}
	if strings.HasPrefix(p, "~/") {
		rest := p[1:] // keep leading /
		if strings.ContainsAny(rest, "\n\r\x00") {
			return "", fmt.Errorf("invalid path characters")
		}
		return `"$HOME"` + Quote(rest), nil
	}
	if strings.ContainsAny(p, "\n\r\x00") {
		return "", fmt.Errorf("invalid path characters")
	}
	return Quote(p), nil
}

// ValidateSessionName rejects names unsafe for tmux hooks / filesystem paths.
func ValidateSessionName(name string) error {
	if !sessionNameRE.MatchString(name) {
		return fmt.Errorf("invalid session name %q (use [A-Za-z0-9._-]{1,64}, start alnum)", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("invalid session name %q", name)
	}
	return nil
}

// ValidateEventKind rejects event kinds unsafe for shell interpolation.
func ValidateEventKind(kind string) error {
	if !eventKindRE.MatchString(kind) {
		return fmt.Errorf("invalid event kind %q (use [A-Za-z0-9._-]{1,64}, start alnum)", kind)
	}
	if strings.Contains(kind, "..") {
		return fmt.Errorf("invalid event kind %q", kind)
	}
	return nil
}

// SanitizeSessionName maps an arbitrary string to a safe session name, or errors.
func SanitizeSessionName(name string) (string, error) {
	if err := ValidateSessionName(name); err == nil {
		return name, nil
	}
	var b strings.Builder
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			if i == 0 && (r == '.' || r == '_' || r == '-') {
				b.WriteByte('s')
			}
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		return "", fmt.Errorf("cannot sanitize session name %q", name)
	}
	// Regex already allows leading alnum (including digits). Only force a prefix
	// when the first rune is still a separator after trim.
	if out[0] == '.' || out[0] == '_' || out[0] == '-' {
		out = "s" + out
	}
	if err := ValidateSessionName(out); err != nil {
		return "", err
	}
	return out, nil
}
