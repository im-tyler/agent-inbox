package claim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireRelease(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Acquire("neutron", "t1", "claude"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	s.Release("neutron", "t1")
	if err := s.Acquire("neutron", "t2", "claude"); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
}

func TestAcquireRefusesWhileHeld(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Acquire("teploy", "t1", "opencode"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	err := s.Acquire("teploy", "t2", "opencode")
	if err == nil {
		t.Fatal("second acquire succeeded; the guard is the point of the package")
	}
	// The refusal must name the holder — "cannot" is not actionable without it.
	if !strings.Contains(err.Error(), "pid") {
		t.Errorf("error %q does not name the holder", err)
	}
}

func TestReleaseWrongTurnLeavesClaim(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Acquire("tebian", "t1", "claude"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// A superseded turn must not unlock the project for the turn that
	// replaced it.
	s.Release("tebian", "t0")
	if err := s.Acquire("tebian", "t2", "claude"); err == nil {
		t.Fatal("release for a different turn removed the claim")
	}
}

func TestDeadPidReclaimed(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.projectDir("maccel")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A pid that cannot exist: the claim is provably holderless.
	b := []byte(`{"pid": 99999999, "turn": "t1", "taken": "` + time.Now().Format(time.RFC3339) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "claim.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("maccel", "t2", "codex"); err != nil {
		t.Fatalf("dead-pid claim not reclaimed: %v", err)
	}
}

func TestUnreadableClaimRefusedWhenFresh(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.projectDir("fylun")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No claim.json at all, directory just created: mid-write, so refuse.
	if err := s.Acquire("fylun", "t1", "claude"); err == nil {
		t.Fatal("fresh unreadable claim was reclaimed; a live writer could be unlocked")
	}
}

// F12: "." and ".." pass name validation's character rules but Join(dir, "..")
// is the parent of the claims directory — a stale reclaim must never get a
// path to recursively delete.
func TestDotNamesRejectedAtClaimBoundary(t *testing.T) {
	root := t.TempDir()
	claims := filepath.Join(root, "claims")
	s := New(claims)
	if err := os.MkdirAll(claims, 0o700); err != nil {
		t.Fatal(err)
	}
	// Sentinel state in the directory a stale ".." claim would target.
	sentinel := filepath.Join(root, "state.json")
	if err := os.WriteFile(sentinel, []byte(`{"projects":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Age the claims dir past staleAfter so the old code's reclaim path
	// would have fired.
	old := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(claims, old, old); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", "..", "", "a/b", "a\\b", ".locks"} {
		if err := s.Acquire(name, "t1", "claude"); err == nil {
			t.Errorf("Acquire(%q) succeeded; the boundary must refuse it", name)
		}
		if err := s.Release(name, "t1"); err == nil {
			t.Errorf("Release(%q) succeeded; the boundary must refuse it", name)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel state.json vanished: %v", err)
	}
	if _, err := os.Stat(claims); err != nil {
		t.Fatalf("claims dir vanished: %v", err)
	}
}

// F02: two contenders for a dead holder's claim. Both may read the dead pid,
// but the per-project flock serializes them: the first installs a live claim,
// the second re-reads under the lock and obeys it.
func TestReclaimRaceSerialized(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.projectDir("haven")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := []byte(`{"pid": 99999999, "turn": "t1", "taken": "` + time.Now().Format(time.RFC3339) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "claim.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	const contenders = 8
	errs := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		go func() { errs <- s.Acquire("haven", "t2", "opencode") }()
	}
	successes := 0
	for i := 0; i < contenders; i++ {
		if err := <-errs; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d contenders succeeded; exactly one may hold the claim", successes)
	}
	info, ok := s.Peek("haven")
	if !ok || info.Turn != "t2" {
		t.Fatalf("claim after race: ok=%v info=%+v", ok, info)
	}
}

// F03: releasing "alpha" must never touch a project literally named
// "alpha.releasing" — or any other project whose name collides with a
// transient release path.
func TestReleaseSparesCollidingProject(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Acquire("alpha", "t1", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("alpha.releasing", "t9", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("alpha", "t1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// alpha is free again...
	if err := s.Acquire("alpha", "t2", "claude"); err != nil {
		t.Fatalf("alpha not released: %v", err)
	}
	// ...and the colliding project's claim survived intact.
	info, ok := s.Peek("alpha.releasing")
	if !ok || info.Turn != "t9" {
		t.Fatalf("alpha.releasing claim damaged by alpha's release: ok=%v info=%+v", ok, info)
	}
}

// Round-2 findings from the focused re-audit, pinned to the rewritten claim
// package.

// A case-folded ".LOCKS" is the lock directory itself on case-insensitive
// filesystems; a claim under that name would let the reclaim path delete the
// flock files and split serialisation across two inodes.
func TestReservedNameRejectedCaseInsensitively(t *testing.T) {
	s := New(t.TempDir())
	// The lock directory with a live sentinel file inside it.
	lockDir := filepath.Join(s.dir, ".locks")
	if err := os.MkdirAll(filepath.Join(lockDir, "live.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(lockDir, "live.lock", "owner.json")
	if err := os.WriteFile(sentinel, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".LOCKS", ".Locks", ".LoCkS"} {
		if err := s.Acquire(name, "t1", "claude"); err == nil {
			t.Errorf("Acquire(%q) succeeded; case-folded reserved names must be refused", name)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("lock dir contents disturbed: %v", err)
	}
}

// A claim that exists but cannot be read may belong to a live process whose
// metadata rotted; deleting it installs a second owner over a running one.
// Only the never-published crash window is reclaimable by age.
func TestUnreadableClaimIsNeverReclaimed(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Acquire("live", "t1", "claude"); err != nil {
		t.Fatal(err)
	}
	dir, err := s.projectDir("live")
	if err != nil {
		t.Fatal(err)
	}
	claimFile := filepath.Join(dir, "claim.json")

	// Corrupt metadata, aged past every threshold: refused.
	old := time.Now().Add(-5 * time.Minute)
	if err := os.WriteFile(claimFile, []byte(`{"pid": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	err = s.Acquire("live", "t2", "claude")
	if err == nil {
		t.Fatal("a claim with corrupt metadata was reclaimed while its owner may be live")
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatal("the unreadable claim was deleted by the refusal path")
	}

	// Unreadable by permissions, aged: refused, not reclaimed.
	if err := os.Chmod(claimFile, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("live", "t3", "claude"); err == nil {
		t.Fatal("a permission-denied claim was reclaimed while its owner may be live")
	}
	os.Chmod(claimFile, 0o600)

	// The never-published window — no claim.json at all — stays reclaimable
	// when old, so one killed process cannot block a project forever.
	os.Remove(claimFile)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("live", "t4", "claude"); err != nil {
		t.Fatalf("aged never-published claim not reclaimed: %v", err)
	}
}

// Release reports failures honestly: an unreadable claim returns an error
// rather than a silent success that may leave the project blocked.
func TestReleaseReportsUnreadableClaim(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Acquire("alpha", "t1", "claude"); err != nil {
		t.Fatal(err)
	}
	dir, err := s.projectDir("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claim.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("alpha", "t1"); err == nil {
		t.Fatal("release over an unreadable claim reported success")
	}
	// The claim directory survives for inspection.
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("unreadable claim was removed by a failed release")
	}
}

// A name at the API's length limit must still be releasable: the release
// path's own suffix has to stay inside the filesystem component limit.
func TestLongNameAcquireRelease(t *testing.T) {
	s := New(t.TempDir())
	name := strings.Repeat("n", 200)
	if err := s.Acquire(name, "t1", "claude"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.Release(name, "t1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.Acquire(name, "t2", "claude"); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if err := s.Acquire(strings.Repeat("x", 201)+"-toolong", "t1", "claude"); err == nil {
		t.Fatal("names beyond the 200-byte budget were accepted")
	}
}

// A claim whose metadata write fails must not leave the directory behind —
// a mid-write claim blocks retries for the whole stale window.
func TestFailedClaimWriteRollsBack(t *testing.T) {
	s := New(t.TempDir())
	orig := writeClaimInfo
	writeClaimInfo = func(path string, b []byte) error {
		return fmt.Errorf("disk full")
	}
	t.Cleanup(func() { writeClaimInfo = orig })

	err := s.Acquire("alpha", "t1", "claude")
	if err == nil {
		t.Fatal("acquire with a failing write succeeded")
	}
	dir, derr := s.projectDir("alpha")
	if derr != nil {
		t.Fatal(derr)
	}
	if _, serr := os.Stat(dir); serr == nil {
		t.Fatal("failed acquisition left its directory behind")
	}

	// And the next attempt, with the write healthy again, succeeds.
	writeClaimInfo = orig
	if err := s.Acquire("alpha", "t2", "claude"); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}
