package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dostos/relay/internal/ports"
)

// SessionDeletion durably bridges an authoritative deletion to the optional
// display projection. Authority never waits for Viz availability; the intent
// remains until its tombstone has been accepted by the local relayd stream.
type SessionDeletion struct {
	V                  int                `json:"v"`
	Session            ports.Presentation `json:"session"`
	Created            string             `json:"created_at"`
	SuppressProjection bool               `json:"suppress_projection,omitempty"`
}

func deletionDir() string       { return AuthorityDeletionDir() }
func deletedManagerDir() string { return DeletedManagerDir() }

func DeleteSessionProjected(ctx context.Context, reg *Registry, viz ports.Viz, sess *Session, suppressProjection bool) error {
	return DeleteSessionsProjected(ctx, reg, viz, []*Session{sess}, suppressProjection, nil)
}

// DeleteSessionsProjected reserves every session against new lineage, performs
// the optional remote teardown while that reservation is transactionally
// protected, commits authoritative deletion, then projects tombstones after
// releasing the authority lock.
func DeleteSessionsProjected(ctx context.Context, reg *Registry, viz ports.Viz, sessions []*Session, suppressProjection bool, teardown func() error) error {
	if reg == nil || len(sessions) == 0 {
		return fmt.Errorf("session deletion requires registry and session")
	}
	reg.txMu.Lock()
	lock, err := openAuthorityLock()
	if err != nil {
		reg.txMu.Unlock()
		return err
	}
	locked := true
	unlock := func() {
		if locked {
			unlockAuthorityFile(lock)
			reg.txMu.Unlock()
			locked = false
		}
	}
	defer unlock()
	handoffs, err := reg.ListHandoffs()
	if err != nil {
		return err
	}
	intents := make([]SessionDeletion, 0, len(sessions))
	paths := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if sess == nil {
			return fmt.Errorf("session deletion requires non-nil sessions")
		}
		children, childErr := reg.DirectChildren(sess.ID)
		if childErr != nil {
			return childErr
		}
		if len(children) > 0 {
			return fmt.Errorf("session %s still manages %d direct child session(s)", sess.ID, len(children))
		}
		for _, handoff := range handoffs {
			if handoff.SourceSessionID == sess.ID && !handoffTerminal(handoff) {
				return fmt.Errorf("session %s still owns nonterminal handoff %s", sess.ID, handoff.ID)
			}
		}
		intent := SessionDeletion{V: 1, Created: time.Now().UTC().Format(time.RFC3339Nano), Session: projectionForSession(sess, sess.SourceSessionID, ports.ProjectionDelete).Item, SuppressProjection: suppressProjection}
		path := filepath.Join(deletionDir(), sanitizeID(sess.ID)+".json")
		if err := writeDurableJSONExclusive(path, intent); err != nil && !os.IsExist(err) {
			return err
		}
		intents, paths = append(intents, intent), append(paths, path)
	}
	if teardown != nil {
		if err := teardown(); err != nil {
			for _, path := range paths {
				_ = os.Remove(path)
			}
			_ = syncDirectory(deletionDir())
			return err
		}
	}
	for i, sess := range sessions {
		if err := reg.deleteSessionLocked(sess.ID); err != nil {
			for _, path := range paths[i:] {
				_ = os.Remove(path)
			}
			return err
		}
		marker := filepath.Join(deletedManagerDir(), sanitizeID(sess.ID)+".json")
		if err := writeDurableJSONExclusive(marker, map[string]any{"v": 1, "session_id": sess.ID, "deleted_at": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil && !os.IsExist(err) {
			return err
		}
	}
	unlock()
	// Projection is optional. A failed enqueue leaves the durable intent for a
	// later recovery pass but does not roll back or block authoritative delete.
	for i := range intents {
		_ = finishSessionDeletion(ctx, viz, paths[i], intents[i])
	}
	return nil
}

func deletionReservationPath(sessionID string) string {
	return filepath.Join(deletionDir(), sanitizeID(sessionID)+".json")
}

func managerDeletionReserved(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if _, err := os.Stat(deletionReservationPath(sessionID)); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(deletedManagerDir(), sanitizeID(sessionID)+".json"))
	return err == nil
}

// RecoverSessionDeletions retries only the projection half. A display outage
// is reported as pending, not as a control-plane outage.
func RecoverSessionDeletions(ctx context.Context, reg *Registry, viz ports.Viz) (int, error) {
	entries, err := os.ReadDir(deletionDir())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(deletionDir(), entry.Name())
		raw, readErr := os.ReadFile(path)
		var intent SessionDeletion
		if readErr != nil || json.Unmarshal(raw, &intent) != nil || intent.V != 1 || intent.Session.SessionID == "" {
			return pending, fmt.Errorf("invalid session deletion intent %s", entry.Name())
		}
		if reg != nil {
			if _, liveErr := reg.GetSession(intent.Session.SessionID); liveErr == nil {
				pending++
				continue
			} else if !errors.Is(liveErr, ErrSessionNotFound) {
				return pending, liveErr
			}
		}
		if err := finishSessionDeletion(ctx, viz, path, intent); err != nil {
			pending++
		}
	}
	return pending, nil
}

func finishSessionDeletion(ctx context.Context, viz ports.Viz, path string, intent SessionDeletion) error {
	if !intent.SuppressProjection && viz == nil {
		return fmt.Errorf("visualization projection unavailable")
	}
	if !intent.SuppressProjection {
		sess := &Session{ID: intent.Session.SessionID, HostID: intent.Session.Target, SourceSessionID: intent.Session.ParentSessionID, Persist: ports.PersistHandle{Name: intent.Session.TmuxName}}
		if _, err := ProjectSession(ctx, viz, sess, ports.ProjectionDelete); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func writeDurableJSONExclusive(path string, value any) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".intent-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
