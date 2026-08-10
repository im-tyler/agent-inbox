// Package fsutil holds the one durable-write routine the rest of the program
// uses for state, config, notes and events.
//
// Every one of those files had its own copy of write-temp-then-rename, and the
// copies disagreed: some checked the error from Close, none called Sync, and
// the event spool skipped the temp file entirely and wrote the final name
// directly, so a reader could see a half-written event, fail to parse it, and
// delete it as though it had been handled.
package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DirMode is the permission for directories under the data dir. These hold
// assistant output and project paths, so they are the user's business alone.
const DirMode fs.FileMode = 0o700

// FileMode is the permission for files the program creates.
const FileMode fs.FileMode = 0o600

// WriteFileAtomic writes data to path so that a reader sees either the old
// contents or the new ones, never a partial write.
//
// The temp file is created in the destination directory because rename is only
// atomic within a filesystem, and /tmp is routinely a different one.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	name := tmp.Name()
	// Any failure from here on leaves a temp file behind unless we clean up.
	// The named-return dance is avoided by removing on every error path.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write temp: %w", err)
	}
	// Sync before rename: rename is atomic with respect to other processes,
	// but not with respect to a power cut. Without this the rename can land
	// while the contents are still in the page cache.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}
