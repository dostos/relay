package core

import (
	"context"
	"fmt"
)

// DeleteSession removes one session's registry row. No remote teardown, no
// identity cleanup: the caller has kept the remote (or already destroyed it).
func DeleteSession(ctx context.Context, reg *Registry, sess *Session) error {
	return deleteSessions(ctx, reg, []*Session{sess}, nil, nil, false)
}

// DeleteSessions runs the remote teardown under the authority lock, then
// commits the registry deletion and marks each session cleaned: its resume
// entry becomes a tombstone and its bridge token is forgotten. A teardown
// failure leaves every record untouched.
func DeleteSessions(ctx context.Context, reg *Registry, sessions []*Session, teardown func() error) error {
	return deleteSessions(ctx, reg, sessions, teardown, nil, teardown != nil)
}

type deletionAuthorizer func(*Session) error

func deleteSessions(_ context.Context, reg *Registry, sessions []*Session, teardown func() error, authorize deletionAuthorizer, cleanupIdentity bool) error {
	if reg == nil || len(sessions) == 0 {
		return fmt.Errorf("session deletion requires registry and session")
	}
	// The registry commit happens under the authority lock; the identity
	// cleanup happens after it is released, because the resume registry takes
	// the same file lock and a second flock on it would wait on ourselves.
	if err := func() error {
		reg.txMu.Lock()
		defer reg.txMu.Unlock()
		lock, err := openAuthorityLock()
		if err != nil {
			return err
		}
		defer unlockAuthorityFile(lock)
		for i, requested := range sessions {
			if requested == nil {
				return fmt.Errorf("session deletion requires non-nil sessions")
			}
			sess, currentErr := reg.GetSession(requested.ID)
			if currentErr != nil {
				return currentErr
			}
			if authorize != nil {
				if err := authorize(sess); err != nil {
					return err
				}
			}
			sessions[i] = sess
		}
		if teardown != nil {
			if err := teardown(); err != nil {
				return err
			}
		}
		for _, sess := range sessions {
			if err := reg.deleteSessionLocked(sess.ID); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return err
	}
	if !cleanupIdentity {
		return nil
	}
	for _, sess := range sessions {
		if err := markResumeCleanedForSession(sess.Persist.Name, sess.ID, "destroyed"); err != nil {
			return err
		}
		if err := forgetBridgeTokenChecked(sess.ID); err != nil {
			return err
		}
	}
	return nil
}
