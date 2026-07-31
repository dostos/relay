package core

import "strings"

const DisplayNameLabel = "display_name"

// ProjectLabel returns the short project name shown in ◆ RELAY · <project>.
// Strips the common dostos-workspace- prefix used by older session names.
func ProjectLabel(persistName string) string {
	name := strings.TrimSpace(persistName)
	name = strings.TrimPrefix(name, "dostos-workspace-")
	if name == "" {
		return persistName
	}
	return name
}

// SessionDisplayName returns a stable human alias without changing the tmux
// checkpoint identity used for resume, bridge authentication, and history.
func SessionDisplayName(sess *Session) string {
	if sess == nil {
		return ""
	}
	if display := strings.TrimSpace(sess.Labels[DisplayNameLabel]); display != "" {
		return display
	}
	return ProjectLabel(sess.Persist.Name)
}

// BrandTitle is the cmux tab / status marker for a relay-managed project.
func BrandTitle(persistName string) string {
	return "◆ RELAY · " + ProjectLabel(persistName)
}

// BrandStatus joins multiple project labels for a workspace pill:
// ◆ RELAY · cdx, opaquebench-oqb
func BrandStatus(projects []string) string {
	if len(projects) == 0 {
		return ""
	}
	return "◆ RELAY · " + strings.Join(projects, ", ")
}
