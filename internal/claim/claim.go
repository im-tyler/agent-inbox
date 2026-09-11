// Package claim extends the "already working" guard across processes.
//
// One program, one guard: the Inbox refuses a second send to a project whose
// status is Working, and that was enough when there was one Inbox. There are
// now several — the dashboard, a harness-driven `agent-inbox send`, an MCP
// tool call — and each holds its own memory, so each would happily report a
// project idle that another is mid-turn on. Two writers on one agent session
// is precisely the failure the in-process guard exists to prevent: a
// transcript interleaved by two subprocesses resumes as garbage.
//
// So a send claims the project in the filesystem, where every process can
// see it. The claim is a directory — mkdir is atomic — holding a small file
// naming the pid that took it and the turn it was taken for.
//
// Directory existence alone cannot decide the two hard cases safely, so every
// mutation of a project's claim (acquire, reclaim, release) runs under an
// advisory exclusive flock on a per-project lock file. The flock is held by
// the kernel for an open file description, so it dies with its holder — it
// cannot go stale the way a directory can. Contenders therefore re-read the
// claim under the lock: whoever reclaims a dead holder's claim cannot have it
// torn out from under them by the next contender, and a release cannot race
// an acquire.
package claim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// staleAfter is how old a claim with no readable pid must be before it is
// treated as abandoned. A crash between mkdir and writing the file inside
// leaves a directory no holder can be established for; refusing forever on
// that would make one killed process cost a project until somebody noticed.
// Younger than this, the honest answer is that somebody may be mid-write, so
// wait.
const staleAfter = 2 * time.Minute

// lockDirName holds the per-project flock files. It lives inside the claims
// directory, so it must be a name no project can bear.
const lockDirName = ".locks"

// lockWait bounds how long a claim mutation waits for the per-project flock.
// The lock is only ever held for a handful of filesystem operations, so
// waiting longer than this means something is wrong, not busy.
const lockWait = 10 * time.Second

// Info is what a claim says about itself.
type Info struct {
	Pid   int       `json:"pid"`
	Turn  string    `json:"turn"` // identifies the turn, for supersede-safe release
	Tool  string    `json:"tool,omitempty"`
	Taken time.Time `json:"taken"`
}

// Set is the claim set for one data directory. Methods are safe for
// concurrent use; the atomicity comes from the filesystem and a per-project
// flock, not a mutex, because the processes competing for a claim are
// separate programs.
type Set struct{ dir string }

// New returns the claim set rooted at dir (typically <dataDir>/claims).
func New(dir string) *Set { return &Set{dir: dir} }

// projectDir returns the claim directory for name, refusing any name that
// could escape the claims directory. This is the boundary guard for the
// filesystem: ".", ".." and separator-bearing names must never become paths,
// because Join(dir, "..") is the parent of the claims directory and a stale
// reclaim of it would recursively delete the data directory.
func (s *Set) projectDir(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("claim: %q is not a valid project name", name)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("claim: project name %q contains a path separator", name)
	}
	if name == lockDirName {
		return "", fmt.Errorf("claim: project name %q is reserved", name)
	}
	dir := filepath.Join(s.dir, name)
	if filepath.Dir(dir) != filepath.Clean(s.dir) {
		return "", fmt.Errorf("claim: project name %q escapes the claims directory", name)
	}
	return dir, nil
}

// contained reports whether path is a direct child of the claims directory —
// the last check before any recursive removal.
func (s *Set) contained(path string) bool {
	return path != "" && filepath.Dir(path) == filepath.Clean(s.dir)
}

// withProjectLock runs fn while holding an exclusive advisory lock on name's
// lock file. The lock is per open file description, so it serializes
// contenders across processes and goroutines alike, and the kernel drops it
// if the holder dies.
func (s *Set) withProjectLock(name string, fn func() error) error {
	lockDir := filepath.Join(s.dir, lockDirName)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("claim lock dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(lockDir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("claim lock file: %w", err)
	}
	defer f.Close()
	deadline := time.Now().Add(lockWait)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("claim lock for %q stayed busy for %s", name, lockWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

// Acquire claims name for one turn, reporting an error if somebody else
// already holds it. The error names the holder, because "cannot send" and
// "another agent-inbox is mid-turn in pid 412" call for different responses —
// the second one is not a malfunction in this program.
//
// A nil set acquires nothing and refuses nothing: it means this inbox has no
// state directory, so no other process exists to contend with.
func (s *Set) Acquire(name, turn, tool string) error {
	if s == nil {
		return nil
	}
	dir, err := s.projectDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("claims dir: %w", err)
	}
	return s.withProjectLock(name, func() error {
		return s.acquireLocked(dir, turn, tool)
	})
}

// mkdirClaim creates the claim directory and writes its info file.
func (s *Set) mkdirClaim(dir, turn, tool string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "claim.json"),
		mustJSON(Info{Pid: os.Getpid(), Turn: turn, Tool: tool, Taken: time.Now()}), 0o600)
}

// acquireLocked is the lock-held acquire: mkdir fast path, stale-reclaim slow
// path.
func (s *Set) acquireLocked(dir, turn, tool string) error {
	err := s.mkdirClaim(dir, turn, tool)
	if err == nil {
		return nil
	}
	if !os.IsExist(err) {
		return err
	}
	return s.takeHeld(dir, turn, tool)
}

// HeldError reports a claim held by a live process. It is a distinct type so
// a caller can tell "another process is mid-turn" — fail fast — from anything
// else, and so a process can recognise its own claims, whose release it may
// know to be imminent.
type HeldError struct {
	Name string
	Info Info
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("project %s is already being driven by agent-inbox pid %d (since %s)",
		e.Name, e.Info.Pid, e.Info.Taken.Format(time.Kitchen))
}

// takeHeld decides what to do about an existing claim: obey it, or reclaim it
// if its holder is provably gone. The caller holds the per-project lock, so
// the read-remove-recreate sequence cannot interleave with another contender
// doing the same: whoever reclaims first installs a live claim, and the next
// contender reads that one and obeys it.
func (s *Set) takeHeld(dir, turn, tool string) error {
	info, rerr := s.read(dir)
	switch {
	case rerr != nil && ageOf(dir) > staleAfter:
		// Unreadable and old: a crash between mkdir and write. Reclaim.
		if s.contained(dir) {
			os.RemoveAll(dir)
		}
		return s.mkdirClaim(dir, turn, tool)
	case rerr != nil:
		return fmt.Errorf("project claim is unreadable (held %.0fs) — retry shortly or remove %s",
			ageOf(dir).Seconds(), dir)
	case !pidAlive(info.Pid):
		if s.contained(dir) {
			os.RemoveAll(dir)
		}
		return s.mkdirClaim(dir, turn, tool)
	default:
		return &HeldError{Name: filepath.Base(dir), Info: info}
	}
}

// Release drops the claim if it still names this turn. A claim taken by a
// later turn is left alone: the release belongs to that turn, and a
// superseded sender removing its successor's claim would unlock the project
// while the successor is mid-turn.
//
// The removal renames the directory out of the way before deleting it.
// RemoveAll alone removes the file inside first, so an acquire arriving in
// that instant sees a directory with no claim.json — "unreadable, held 0s" —
// and refuses, correctly by its own rules, over a release that was already
// happening. Rename is atomic: an acquire either sees the claim intact or
// sees no directory at all. The rename target carries the releasing pid and
// a timestamp, so it can never collide with another project's live claim —
// including a project literally named "alpha.releasing" — and a failed rename
// aborts the release instead of deleting whatever sits at the target.
func (s *Set) Release(name, turn string) error {
	if s == nil {
		return nil
	}
	dir, err := s.projectDir(name)
	if err != nil {
		return err
	}
	return s.withProjectLock(name, func() error {
		info, err := s.read(dir)
		if err != nil || info.Turn != turn {
			return nil
		}
		doomed := fmt.Sprintf("%s.releasing.%d.%d", dir, os.Getpid(), time.Now().UnixNano())
		if err := os.Rename(dir, doomed); err != nil {
			return fmt.Errorf("claim release rename %s: %w", name, err)
		}
		if s.contained(doomed) {
			os.RemoveAll(doomed)
		}
		return nil
	})
}

// Peek reports the current claim on a project without taking it: who holds
// it, if anybody. This is the liveness oracle for cross-process status — a
// state entry that says Working is only as true as the claim behind it.
func (s *Set) Peek(name string) (Info, bool) {
	if s == nil {
		return Info{}, false
	}
	dir, err := s.projectDir(name)
	if err != nil {
		return Info{}, false
	}
	info, err := s.read(dir)
	if err != nil {
		return Info{}, false
	}
	return info, pidAlive(info.Pid)
}

// errUnreadable marks a claim whose contents cannot be established; the
// directory's mtime stands in for its age.
var errUnreadable = errors.New("claim unreadable")

func (s *Set) read(dir string) (Info, error) {
	b, err := os.ReadFile(filepath.Join(dir, "claim.json"))
	if err != nil {
		return Info{}, errUnreadable
	}
	var info Info
	if json.Unmarshal(b, &info) != nil {
		return Info{}, errUnreadable
	}
	return info, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func ageOf(dir string) time.Duration {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0
	}
	return time.Since(fi.ModTime())
}

// pidAlive reports whether a process exists. Signal 0 performs the existence
// check without delivering anything; EPERM means it exists but belongs to
// somebody else, which is alive for every decision made here.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
