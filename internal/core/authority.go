package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dostos/relay/internal/ports"
)

// ManagerReplacement is a durable, explicit authority transition. Its file is
// an intent, not a second source of truth: recovery idempotently completes the
// writes in the authoritative registry and then removes it.
type ManagerReplacement struct {
	V                  int                  `json:"v"`
	ID                 string               `json:"id"`
	OldID              string               `json:"old_session_id"`
	NewID              string               `json:"new_session_id"`
	Children           []string             `json:"children"`
	AuthorityConverged bool                 `json:"authority_converged,omitempty"`
	Projections        []ports.Presentation `json:"projections,omitempty"`
	Created            string               `json:"created_at"`
}

type ManagerReplacementResult struct {
	OperationID       string   `json:"operation_id"`
	OldID             string   `json:"old_session_id"`
	NewID             string   `json:"new_session_id"`
	Reparented        []string `json:"reparented"`
	ProjectionPending bool     `json:"projection_pending,omitempty"`
}

func replacementIntentPath() string {
	return AuthorityReplacementPath()
}

func (r *RootService) Replace(ctx context.Context, parents *ParentService, oldID, newSessionID string) (*ManagerReplacementResult, error) {
	if r == nil || r.Reg == nil || parents == nil {
		return nil, fmt.Errorf("authority services required")
	}
	if oldID == newSessionID {
		return nil, fmt.Errorf("old and new apex must be different sessions")
	}
	lock, err := acquireAuthorityTransitionLock(r.Reg)
	if err != nil {
		return nil, err
	}
	released := false
	unlock := func() {
		if !released {
			releaseAuthorityTransitionLock(r.Reg, lock)
			released = true
		}
	}
	defer unlock()
	if _, err := os.Stat(replacementIntentPath()); err == nil {
		return r.recoverReplacementUnlocked(ctx, parents, unlock)
	}
	old, err := r.Apex()
	if err != nil || old.ID != oldID {
		return nil, fmt.Errorf("session %s is not the current apex", oldID)
	}
	if r.Sessions != nil {
		current := r.AgentReadinessFor(ctx, r.Sessions, old.ID)
		switch current.State {
		case AgentBlocked:
			return nil, fmt.Errorf("current apex is blocked (%s); answer the gate yourself before replacement", current.Reason)
		case AgentReady:
			return nil, fmt.Errorf("current apex is still active; stop it before replacement")
		case AgentUnknown:
			return nil, fmt.Errorf("current apex state could not be verified (%s); replacement refused", current.Reason)
		}
	}
	next, err := r.Reg.GetSession(newSessionID)
	if err != nil {
		return nil, err
	}
	if next.SourceSessionID != "" {
		return nil, fmt.Errorf("replacement apex %s has a manager", newSessionID)
	}
	if r.Sessions != nil {
		ready := r.AgentReadinessFor(ctx, r.Sessions, next.ID)
		if ready.State == AgentBlocked {
			return nil, fmt.Errorf("replacement apex is blocked (%s); Relay will not answer a security gate", ready.Reason)
		}
		if ready.State == AgentAbsent {
			return nil, fmt.Errorf("replacement apex is absent (%s)", ready.Reason)
		}
		if ready.State == AgentUnknown {
			return nil, fmt.Errorf("replacement apex state could not be verified (%s)", ready.Reason)
		}
	}
	sessions, err := r.Reg.ListSessions()
	if err != nil {
		return nil, err
	}
	intent := ManagerReplacement{V: 1, ID: newID("replace"), OldID: oldID, NewID: newSessionID, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, sess := range sessions {
		if sess.SourceSessionID == oldID {
			if err := validateManagerEdge(r.Reg, next, sess); err != nil {
				return nil, err
			}
			intent.Children = append(intent.Children, sess.ID)
			intent.Projections = append(intent.Projections, projectionForSession(sess, newSessionID, ports.ProjectionUpsert).Item)
		}
	}
	intent.Projections = append(intent.Projections, projectionForSession(next, "", ports.ProjectionUpsert).Item)
	if err := writeReplacementIntent(intent); err != nil {
		return nil, err
	}
	return r.applyReplacement(ctx, parents, intent, unlock)
}

func (r *RootService) RecoverReplacement(ctx context.Context, parents *ParentService) (*ManagerReplacementResult, error) {
	lock, err := acquireAuthorityTransitionLock(r.Reg)
	if err != nil {
		return nil, err
	}
	released := false
	unlock := func() {
		if !released {
			releaseAuthorityTransitionLock(r.Reg, lock)
			released = true
		}
	}
	defer unlock()
	return r.recoverReplacementUnlocked(ctx, parents, unlock)
}

func (r *RootService) recoverReplacementUnlocked(ctx context.Context, parents *ParentService, unlock func()) (*ManagerReplacementResult, error) {
	raw, err := os.ReadFile(replacementIntentPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var intent ManagerReplacement
	if json.Unmarshal(raw, &intent) != nil || intent.V != 1 || intent.OldID == "" || intent.NewID == "" {
		return nil, fmt.Errorf("invalid authority replacement intent")
	}
	return r.applyReplacement(ctx, parents, intent, unlock)
}

func acquireAuthorityTransitionLock(reg *Registry) (*os.File, error) {
	reg.txMu.Lock()
	lock, err := openAuthorityLock()
	if err != nil {
		reg.txMu.Unlock()
		return nil, err
	}
	return lock, nil
}

func releaseAuthorityTransitionLock(reg *Registry, lock *os.File) {
	unlockAuthorityFile(lock)
	reg.txMu.Unlock()
}

func (r *RootService) applyReplacement(ctx context.Context, parents *ParentService, intent ManagerReplacement, unlock func()) (*ManagerReplacementResult, error) {
	result := &ManagerReplacementResult{OperationID: intent.ID, OldID: intent.OldID, NewID: intent.NewID}
	if !intent.AuthorityConverged {
		old, err := r.Reg.GetSession(intent.OldID)
		if err != nil {
			return nil, err
		}
		next, err := r.Reg.GetSession(intent.NewID)
		if err != nil {
			return nil, err
		}
		for _, childID := range intent.Children {
			child, childErr := r.Reg.GetSession(childID)
			if childErr != nil {
				return nil, fmt.Errorf("replacement child %s is missing: %w", childID, childErr)
			}
			if child.SourceSessionID != old.ID && child.SourceSessionID != next.ID {
				return nil, fmt.Errorf("child %s changed managers during replacement", child.ID)
			}
			if child.SourceSessionID != next.ID {
				child.SourceSessionID, child.SourceHostID, child.SourcePersistName = next.ID, next.HostID, next.Persist.Name
				if err := r.Reg.putSessionLocked(child); err != nil {
					return nil, err
				}
			}
			result.Reparented = append(result.Reparented, child.ID)
		}
		if err := movePendingAuthorityMessages(parents, old.ID, next.ID); err != nil {
			return nil, err
		}
		if next.Labels == nil {
			next.Labels = map[string]string{}
		}
		next.Labels[ApexLabel] = "true"
		delete(old.Labels, ApexLabel)
		if err := r.Reg.putSessionLocked(next); err != nil {
			return nil, err
		}
		if err := r.Reg.putSessionLocked(old); err != nil {
			return nil, err
		}
		if err := appendReplacementLedgerOnce(intent); err != nil {
			return nil, err
		}
		intent.AuthorityConverged = true
		if err := rewriteReplacementIntent(intent); err != nil {
			return nil, err
		}
	} else {
		result.Reparented = append(result.Reparented, intent.Children...)
	}
	// Optional display I/O and its receipt write happen outside the authority
	// transaction. The durable intent remains until all projections enqueue.
	unlock()
	for _, projection := range intent.Projections {
		if err := parents.projectPane(projection); err != nil {
			result.ProjectionPending = true
		}
	}
	if result.ProjectionPending {
		return result, nil
	}
	finishUnlock, err := lockAuthorityWrite()
	if err != nil {
		return nil, err
	}
	defer finishUnlock()
	if err := os.Remove(replacementIntentPath()); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return result, nil
}

func rewriteReplacementIntent(intent ManagerReplacement) error {
	raw, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(StateRoot(), ".authority-replacement-*.tmp")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
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
	if err := os.Rename(path, replacementIntentPath()); err != nil {
		return err
	}
	return syncDirectory(StateRoot())
}

func movePendingAuthorityMessages(parents *ParentService, oldID, newID string) error {
	messages, err := parents.ListMessages(oldID, true)
	if err != nil {
		return err
	}
	for _, msg := range messages {
		oldPath := parentMessagePath(oldID, msg.ID)
		msg.ParentSessionID = newID
		if err := writeParentMessageLocked(msg, false); err != nil {
			return err
		}
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func appendReplacementLedgerOnce(intent ManagerReplacement) error {
	if raw, err := os.ReadFile(LedgerPath()); err == nil {
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var record map[string]any
			if json.Unmarshal(line, &record) == nil && record["operation_id"] == intent.ID {
				return nil
			}
		}
	}
	return appendLedgerLocked(map[string]any{"v": 1, "type": "manager_replace", "operation_id": intent.ID, "ts": time.Now().UTC().Format(time.RFC3339Nano), "old_session_id": intent.OldID, "new_session_id": intent.NewID, "children": intent.Children})
}

func writeReplacementIntent(intent ManagerReplacement) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	if err := EnsureStateDirs(); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(intent, "", "  ")
	tmp := replacementIntentPath() + "." + intent.ID + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, replacementIntentPath()); err != nil {
		return fmt.Errorf("another authority replacement is active: %w", err)
	}
	dir, err := os.Open(StateRoot())
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (r *Registry) DirectChildren(sessionID string) ([]*Session, error) {
	sessions, err := r.ListSessions()
	if err != nil {
		return nil, err
	}
	var children []*Session
	for _, sess := range sessions {
		if sess.SourceSessionID == sessionID {
			children = append(children, sess)
		}
	}
	return children, nil
}
