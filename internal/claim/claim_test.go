package claim

import (
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
	dir := s.projectDir("maccel")
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
	dir := s.projectDir("fylun")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No claim.json at all, directory just created: mid-write, so refuse.
	if err := s.Acquire("fylun", "t1", "claude"); err == nil {
		t.Fatal("fresh unreadable claim was reclaimed; a live writer could be unlocked")
	}
}
