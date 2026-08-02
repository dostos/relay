package main

import (
	"fmt"
	"sort"

	"github.com/dostos/relay/internal/coord/relayd"
	"github.com/dostos/relay/internal/core"
	"github.com/dostos/relay/internal/ports"
	"github.com/dostos/relay/internal/shellquote"
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

func visualizationAuthoritySnapshotV2(service string) (*ports.AuthoritySnapshot, error) {
	_, events, err := relayd.DefaultPaths()
	if err != nil {
		return nil, err
	}
	revision := func() (int64, error) {
		// A fresh Store is intentional: Store caches the sequence in memory,
		// while the long-running event daemon may append between samples.
		store, err := relayd.NewStore(events)
		if err != nil {
			return 0, err
		}
		return store.LastSeq(service)
	}
	return consistentAuthoritySnapshot(revision, visualizationAuthoritySnapshot)
}

func consistentAuthoritySnapshot(revision func() (int64, error), items func() ([]ports.Presentation, error)) (*ports.AuthoritySnapshot, error) {
	for attempt := 0; attempt < 8; attempt++ {
		before, err := revision()
		if err != nil {
			return nil, err
		}
		current, err := items()
		if err != nil {
			return nil, err
		}
		after, err := revision()
		if err != nil {
			return nil, err
		}
		if before == after {
			return &ports.AuthoritySnapshot{V: 1, Revision: after, Items: current}, nil
		}
	}
	return nil, fmt.Errorf("visualization authority changed continuously while taking snapshot")
}

func visualizationAuthorityResume(persistName string) (*ports.ResumeResolution, error) {
	if err := shellquote.ValidateSessionName(persistName); err != nil {
		return nil, err
	}
	reg := &core.Registry{}
	sessions, err := reg.ListSessions()
	if err != nil {
		return nil, err
	}
	var matched *core.Session
	for _, session := range sessions {
		if session.Persist.Name != persistName {
			continue
		}
		if matched != nil {
			return nil, fmt.Errorf("multiple authoritative sessions use persist name %q", persistName)
		}
		matched = session
	}
	if matched == nil {
		return nil, fmt.Errorf("session %q is absent from the authoritative registry", persistName)
	}
	if matched.ID == "" || matched.HostID == "" {
		return nil, fmt.Errorf("authoritative resume identity incomplete for %q", persistName)
	}
	return &ports.ResumeResolution{SessionID: matched.ID, Target: matched.HostID, TmuxName: matched.Persist.Name}, nil
}
