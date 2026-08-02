package core

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RetireLocalAuthorityState atomically fences local authority writers and
// quarantines their durable state before a host becomes visualization-only.
func RetireLocalAuthorityState() error {
	lock, err := openAuthorityLock()
	if err != nil {
		return err
	}
	defer unlockAuthorityFile(lock)

	root := StateRoot()
	marker := ProjectionOnlyMarkerPath()
	markerExists := false
	paths := []string{
		SessionsPath(), HandoffsDir(), ParentInboxDir(), ParentWatchDir(),
		BridgeTokensDir(), BridgeIdentitiesDir(), ResumeRegistryPath(),
		AuthorityDeletionDir(), DeletedManagerDir(), AuthorityReplacementPath(),
	}
	if info, err := os.Lstat(marker); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("unsafe projection-only marker %s", marker)
		}
		markerExists = true
		meaningful, scanErr := authorityPathsMeaningful(paths)
		if scanErr != nil {
			return scanErr
		}
		if !meaningful {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Publish and persist the read fence before moving any authority path.
	// A partial quarantine is safe: readers and post-lock writers fail closed,
	// and a later follower start resumes quarantining what remains.
	if !markerExists {
		if err := writeProjectionOnlyMarker(marker, root); err != nil {
			return err
		}
	}

	archive := filepath.Join(root, "retired-local-authority", time.Now().UTC().Format("20060102T150405.000000000Z"))
	createdArchive := false
	for _, source := range paths {
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if !createdArchive {
			if err := os.MkdirAll(archive, 0o700); err != nil {
				return err
			}
			createdArchive = true
		}
		target := filepath.Join(archive, filepath.Base(source))
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("archive local authority %s: %w", source, err)
		}
	}
	return nil
}

func writeProjectionOnlyMarker(marker, root string) error {
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString("home control plane is authoritative\n"); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	return err
}

func authorityPathsMeaningful(paths []string) (bool, error) {
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		// Symlinks and non-directories are authority state. Never follow a
		// link and classify its target as a harmless empty directory.
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return true, nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return false, err
		}
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}
