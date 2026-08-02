package main

import (
	"fmt"
	"sort"

	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
)

func visualizationAuthoritySnapshot() ([]ports.Presentation, error) {
	sessions, err := (&core.Registry{}).ListSessions()
	if err != nil {
		return nil, err
	}
	items := make([]ports.Presentation, 0, len(sessions))
	for _, session := range sessions {
		if session == nil || session.ID == "" || session.HostID == "" || session.Persist.Name == "" {
			return nil, fmt.Errorf("session registry contains incomplete visualization identity")
		}
		items = append(items, ports.Presentation{
			SessionID: session.ID, ParentSessionID: session.SourceSessionID,
			Target: session.HostID, TmuxName: session.Persist.Name,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SessionID < items[j].SessionID })
	return items, nil
}
