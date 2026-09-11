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
// see it. The claim is a directory — mkdir is atomic, existence is the lock —
// holding a small file naming the pid that took it and the turn it was taken
// for. A pid that dies releases nothing, but a dead pid can be detected, and
// a claim whose pid is gone is reclaimed rather than obeyed.
package claim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// Info is what a claim says about itself.
type Info struct {
	Pid   int       `json:"pid"`
	Turn  string    `json:"turn"` // identifies the turn, for supersede-safe release
	Tool  string    `json:"tool,omitempty"`
	Taken time.Time `json:"taken"`
}

// Set is the claim set for one data directory. Methods are safe for
// concurrent use; the atomicity comes from the filesystem, not a mutex,
// because the processes competing for a claim are separate programs.
type Set struct{ dir string }

// New returns the claim set rooted at dir (typically <dataDir>/claims).
func New(dir string) *Set { return &Set{dir: dir} }

func (s *Set) projectDir(name string) string {
	// Project names are validated to letters, digits, dot, underscore and
	// hyphen (ident.ValidateName), so a name cannot escape the claims dir.
	return filepath.Join(s.dir, name)
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
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("claims dir: %w", err)
	}
	dir := s.projectDir(name)
	err := os.Mkdir(dir, 0o700)
	if err == nil {
		return os.WriteFile(filepath.Join(dir, "claim.json"),
			mustJSON(Info{Pid: os.Getpid(), Turn: turn, Tool: tool, Taken: time.Now()}), 0o600)
	}
	if !os.IsExist(err) {
		return err
	}
	return s.takeHeld(name, dir, turn, tool)
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
// if its holder is provably gone.
func (s *Set) takeHeld(name, dir, turn, tool string) error {
	info, rerr := s.read(dir)
	switch {
	case rerr != nil && ageOf(dir) > staleAfter:
		// Unreadable and old: a crash between mkdir and write. Reclaim.
		os.RemoveAll(dir)
		return s.Acquire(name, turn, tool)
	case rerr != nil:
		return fmt.Errorf("project %s has an unreadable claim (held %.0fs) — retry shortly or remove %s",
			name, ageOf(dir).Seconds(), dir)
	case !pidAlive(info.Pid):
		os.RemoveAll(dir)
		return s.Acquire(name, turn, tool)
	default:
		return &HeldError{Name: name, Info: info}
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
// sees no directory at all.
func (s *Set) Release(name, turn string) {
	if s == nil {
		return
	}
	dir := s.projectDir(name)
	info, err := s.read(dir)
	if err != nil || info.Turn != turn {
		return
	}
	// A name no project can have (ValidateName rejects ':' and '/'), so a
	// rename can never collide with a live claim directory.
	doomed := dir + ".releasing"
	os.Rename(dir, doomed)
	os.RemoveAll(doomed)
}

// Peek reports the current claim on a project without taking it: who holds
// it, if anybody. This is the liveness oracle for cross-process status — a
// state entry that says Working is only as true as the claim behind it.
func (s *Set) Peek(name string) (Info, bool) {
	if s == nil {
		return Info{}, false
	}
	info, err := s.read(s.projectDir(name))
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
