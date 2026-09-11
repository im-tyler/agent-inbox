package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// WithFileLock runs fn while holding an exclusive advisory lock on lockPath,
// shared across processes. The lock is a file whose contents are nothing: the
// kernel holds it on the descriptor, so a process that dies releases it
// whether it meant to or not.
//
// Everything this program persists — state, notes, config — is now written by
// more than one process at a time: the dashboard in one, a harness-driven
// `agent-inbox send` in another. Atomic rename keeps any single file valid,
// but validity is not consistency: two writers that each snapshot in one
// order and rename in the other leave the older state on disk. The lock
// serialises the read-modify-write cycles that rename alone cannot.
//
// It waits rather than failing fast, bounded by timeout, because every holder
// is doing millisecond-scale file work — an unbounded wait would only ever be
// a holder that crashed without closing a descriptor, which the kernel does
// for it. A wait that exceeds the bound reports an error rather than
// proceeding unlocked: two processes believing they hold the lock is worse
// than one believing it cannot write.
func WithFileLock(lockPath string, timeout time.Duration, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), DirMode); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, FileMode)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()

	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lock %s: held elsewhere after %s", lockPath, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}
