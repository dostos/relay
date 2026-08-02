package core

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockAuthorityWrite() (func(), error) {
	lock, err := openAuthorityLock()
	if err != nil {
		return nil, err
	}
	if err := EnsureAuthorityWritable(); err != nil {
		unlockAuthorityFile(lock)
		return nil, err
	}
	return func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }, nil
}

func unlockAuthorityFile(lock *os.File) {
	if lock == nil {
		return
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func openAuthorityLock() (*os.File, error) {
	if err := EnsureStateDirs(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(StateRoot(), "authority.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
