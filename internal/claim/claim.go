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
	"crypto/sha256"
	"encoding/hex"
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
//
// The lock directory's name is reserved case-insensitively: on a
// case-insensitive filesystem (macOS, Windows) ".LOCKS" and ".locks" are the
// same directory, and a claim under that name would make the reclaim path
// delete the flock files — splitting serialisation across two inodes.
func (s *Set) projectDir(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("claim: %q is not a valid project name", name)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("claim: project name %q contains a path separator", name)
	}
	if strings.EqualFold(name, lockDirName) {
		return "", fmt.Errorf("claim: project name %q is reserved", name)
	}
	// Long names would overflow the filesystem component limit once the
	// release suffix or the lock extension is appended — an un-releasable
	// claim blocks its project until the stale window reclaims it.
	if len(name) > 200 {
		return "", fmt.Errorf("claim: project name is %d bytes; the limit is 200", len(name))
	}
	dir := filepath.Join(s.dir, name)
	if filepath.Dir(dir) != filepath.Clean(s.dir) {
		return "", fmt.Errorf("claim: project name %q escapes the claims directory", name)
	}
	return dir, nil
}

// contained reports whether path is a direct child of the claims directory —
// the last check before any recursive removal. The lock directory is never a
// removal target, whatever case it was spelled in.
func (s *Set) contained(path string) bool {
	if path == "" {
		return false
	}
	if strings.EqualFold(path, filepath.Join(s.dir, lockDirName)) {
		return false
	}
	return filepath.Dir(path) == filepath.Clean(s.dir)
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

// mkdirClaim creates the claim directory and publishes its info file
// atomically (temp + rename inside the directory). A partially written
// claim.json is indistinguishable from corruption at read time, so it must
// never exist: a failed write rolls the whole directory back rather than
// leaving a mid-write claim that blocks retries.
func (s *Set) mkdirClaim(dir, turn, tool string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	info := Info{Pid: os.Getpid(), Turn: turn, Tool: tool, Taken: time.Now()}
	if err := writeClaimInfo(filepath.Join(dir, "claim.json"), mustJSON(info)); err != nil {
		if s.contained(dir) {
			os.RemoveAll(dir)
		}
		return fmt.Errorf("claim could not be published (rolled back): %w", err)
	}
	return nil
}

// writeClaimInfo publishes claim bytes atomically. A var so tests can inject
// a write failure and assert the rollback.
var writeClaimInfo = func(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
//
// Only a claim that was never published is reclaimable by age. A claim that
// exists but cannot be read — permissions, I/O, corrupt JSON — may belong to
// a live process whose metadata rotted, and deleting it would install a
// second owner over a running one. That fails closed: it is refused until a
// human inspects it.
func (s *Set) takeHeld(dir, turn, tool string) error {
	info, rerr := s.read(dir)
	switch {
	case errors.Is(rerr, errNoClaim) && ageOf(dir) > staleAfter:
		// A crash between mkdir and publish. The atomic publish means an
		// established claim never looks like this, so age is the only
		// question left.
		if s.contained(dir) {
			os.RemoveAll(dir)
		}
		return s.mkdirClaim(dir, turn, tool)
	case errors.Is(rerr, errNoClaim):
		return fmt.Errorf("project claim is mid-write (held %.0fs) — retry shortly or remove %s",
			ageOf(dir).Seconds(), dir)
	case rerr != nil:
		return fmt.Errorf("project claim exists but is unreadable and cannot be reclaimed automatically — inspect %s (it may belong to a live process): %v", dir, rerr)
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
// that instant sees a directory with no claim.json — "mid-write" — and
// refuses, correctly by its own rules, over a release that was already
// happening. Rename is atomic: an acquire either sees the claim intact or
// sees no directory at all. The rename target is a fixed-length,
// never-colliding transient name, so it cannot land on another project's
// live claim — including a project literally named "alpha.releasing" — nor
// overflow the filesystem's component limit on long project names.
//
// Errors are propagated, not swallowed: reporting a successful release while
// the claim may still be held leaves a project blocked with nobody knowing
// why. Confirmed absence and a genuinely different turn are the only silent
// successes.
func (s *Set) Release(name, turn string) error {
	if s == nil {
		return nil
	}
	dir, err := s.projectDir(name)
	if err != nil {
		return err
	}
	return s.withProjectLock(name, func() error {
		info, rerr := s.read(dir)
		switch {
		case errors.Is(rerr, errNoClaim):
			// No metadata to compare against: nothing established to release.
			return nil
		case rerr != nil:
			return fmt.Errorf("cannot release %q — its claim is unreadable and may still be held; inspect the claim directory: %v", name, rerr)
		}
		if info.Turn != turn {
			return nil
		}
		var doomed string
		// A unique name can still lose a cosmic lottery with a project named
		// exactly like it; retry once with a fresh timestamp before failing.
		for attempt := 0; attempt < 2; attempt++ {
			doomed = releaseName(dir)
			if err := os.Rename(dir, doomed); err == nil {
				if s.contained(doomed) {
					if rmErr := os.RemoveAll(doomed); rmErr != nil {
						return fmt.Errorf("claim released but its tombstone at %s could not be removed: %w", doomed, rmErr)
					}
				}
				return nil
			} else if attempt == 1 {
				return fmt.Errorf("claim release rename %s: %w", name, err)
			}
		}
		return nil
	})
}

// releaseName is the transient name a released claim is renamed to before
// deletion. Two properties at once: unique per release (pid + nanoseconds),
// so it can never land on another project's live claim — including one
// literally named like a release path — and bounded, so a long project name
// cannot push it past the filesystem's component limit and make every
// release fail. Truncation keeps a hash of the full name, so two long
// projects sharing a prefix still get distinct names.
func releaseName(dir string) string {
	base := filepath.Base(dir)
	const budget = 200
	if len(base) > budget {
		sum := sha256.Sum256([]byte(base))
		base = base[:budget-14] + "-" + hex.EncodeToString(sum[:6])
	}
	return filepath.Join(filepath.Dir(dir), fmt.Sprintf("%s.r%d-%x", base, os.Getpid(), time.Now().UnixNano()))
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

// errNoClaim marks a claim directory whose metadata was never published —
// the crash window between mkdir and the atomic rename. Age distinguishes a
// writer that may still be running from one that is provably gone.
// errUnreadable marks a claim that exists but cannot be established:
// permissions, I/O, or corrupt bytes. These are different situations with
// opposite correct answers — the first may be reclaimed when old, the second
// must never be, because its owner may be alive and merely unreadable.
var (
	errNoClaim    = errors.New("claim not published")
	errUnreadable = errors.New("claim unreadable")
)

func (s *Set) read(dir string) (Info, error) {
	b, err := os.ReadFile(filepath.Join(dir, "claim.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Info{}, errNoClaim
		}
		return Info{}, fmt.Errorf("%w: %v", errUnreadable, err)
	}
	var info Info
	if json.Unmarshal(b, &info) != nil {
		return Info{}, fmt.Errorf("%w: corrupt claim.json", errUnreadable)
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
