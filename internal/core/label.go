package core

import "strings"

// ProjectLabel returns the short project name shown in ◆ RELAY · <project>.
// Strips the common dostos-workspace- prefix used by sst-era session names.
func ProjectLabel(persistName string) string {
	name := strings.TrimSpace(persistName)
	name = strings.TrimPrefix(name, "dostos-workspace-")
	if name == "" {
		return persistName
	}
	return name
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
