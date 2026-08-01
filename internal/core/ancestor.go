package core

// maxAncestorDepth bounds the upward lineage walk. Relay does not enforce
// acyclicity anywhere, and a corrupted or hand-repaired lineage must never
// hang escalation routing.
const maxAncestorDepth = 32

// AncestorChain returns the ancestors of startSessionID, nearest first,
// excluding the start session. It stops at a root (empty SourceSessionID), a
// missing session, a cycle, or maxAncestorDepth — whichever comes first.
func AncestorChain(reg *Registry, startSessionID string) []*Session {
	if reg == nil || startSessionID == "" {
		return nil
	}
	var out []*Session
	visited := map[string]bool{startSessionID: true}
	current := startSessionID
	for depth := 0; depth < maxAncestorDepth; depth++ {
		sess, err := reg.GetSession(current)
		if err != nil || sess == nil || sess.SourceSessionID == "" {
			return out
		}
		next := sess.SourceSessionID
		if visited[next] {
			return out
		}
		visited[next] = true
		parent, err := reg.GetSession(next)
		if err != nil || parent == nil {
			return out
		}
		out = append(out, parent)
		current = next
	}
	return out
}
