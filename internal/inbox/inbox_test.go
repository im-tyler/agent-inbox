package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentinbox/internal/driver"
)

// Helper: build an inbox with N idle projects, no drivers registered.
func testInbox(t *testing.T, n int) (*Inbox, string) {
	t.Helper()
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state.json")

	projects := make([]*Project, n)
	for i := 0; i < n; i++ {
		projects[i] = &Project{
			Name:   "p" + string(rune('1'+i)),
			Tool:   "mock",
			Dir:    "/tmp",
			Status: driver.StatusIdle,
		}
	}
	return New(projects, map[string]driver.Driver{}, statePath), statePath
}

func TestSnapshotIsACopy(t *testing.T) {
	in, _ := testInbox(t, 3)
	snap := in.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}
	// Mutate the snapshot; original should be unaffected.
	snap[0].Name = "MUTATED"
	again := in.Snapshot()
	if again[0].Name == "MUTATED" {
		t.Fatal("Snapshot returned a reference, not a copy")
	}
}

func TestWaitingCount(t *testing.T) {
	in, _ := testInbox(t, 3)
	if in.WaitingCount() != 0 {
		t.Fatal("fresh inbox should have 0 waiting")
	}

	in.mu.Lock()
	in.projects[0].Status = driver.StatusWaiting
	in.projects[1].Status = driver.StatusError
	in.mu.Unlock()

	if got := in.WaitingCount(); got != 2 {
		t.Fatalf("WaitingCount = %d, want 2", got)
	}
}

func TestDetailOutOfRangeFails(t *testing.T) {
	in, _ := testInbox(t, 2)
	if _, err := in.Detail(0); err == nil {
		t.Error("Detail(0) should error (1-based)")
	}
	if _, err := in.Detail(3); err == nil {
		t.Error("Detail(3) should error (out of range)")
	}
	if _, err := in.Detail(1); err != nil {
		t.Errorf("Detail(1) unexpected error: %v", err)
	}
}

func TestAttachArgsRequiresSession(t *testing.T) {
	in, _ := testInbox(t, 1)
	if _, _, err := in.AttachArgs(1); err == nil {
		t.Error("AttachArgs should fail when project has no session")
	}

	in.mu.Lock()
	in.projects[0].SessionID = "abc"
	in.mu.Unlock()

	// No driver registered for "mock" tool in this test — should still fail.
	if _, _, err := in.AttachArgs(1); err == nil {
		t.Error("AttachArgs should fail when no driver registered")
	}
}

func TestStatePersistenceRoundtrip(t *testing.T) {
	in, statePath := testInbox(t, 2)

	// Mutate state and save.
	in.mu.Lock()
	in.projects[0].SessionID = "session-aaa"
	in.projects[0].Status = driver.StatusWaiting
	in.projects[0].LastMessage = "hello world"
	in.projects[1].SessionID = "session-bbb"
	in.mu.Unlock()
	in.save()

	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// Build a fresh project list (as main.go does on startup) and load.
	fresh := []*Project{
		{Name: "p1", Tool: "mock", Dir: "/tmp", Status: driver.StatusIdle},
		{Name: "p2", Tool: "mock", Dir: "/tmp", Status: driver.StatusIdle},
	}
	LoadState(statePath, fresh)

	if fresh[0].SessionID != "session-aaa" {
		t.Errorf("p1 SessionID = %q, want session-aaa", fresh[0].SessionID)
	}
	if fresh[0].LastMessage != "hello world" {
		t.Errorf("p1 LastMessage = %q, want 'hello world'", fresh[0].LastMessage)
	}
	if fresh[0].Status != driver.StatusWaiting {
		t.Errorf("p1 Status = %q, want waiting", fresh[0].Status)
	}
	if fresh[1].SessionID != "session-bbb" {
		t.Errorf("p2 SessionID = %q, want session-bbb", fresh[1].SessionID)
	}
}

func TestLoadStateResetsWorkingAfterRestart(t *testing.T) {
	// A send can't survive a restart — the background goroutine is gone.
	// LoadState should reset StatusWorking -> StatusIdle so the user can
	// send again.
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state.json")
	saved := []Project{
		{Name: "p1", Tool: "mock", Dir: "/tmp", Status: driver.StatusWorking, SessionID: "x"},
	}
	b, _ := json.MarshalIndent(saved, "", "  ")
	_ = os.WriteFile(statePath, b, 0o644)

	fresh := []*Project{{Name: "p1", Tool: "mock", Dir: "/tmp", Status: driver.StatusIdle}}
	LoadState(statePath, fresh)

	if fresh[0].Status != driver.StatusIdle {
		t.Errorf("Status = %q, want idle after restart", fresh[0].Status)
	}
}

func TestSendUnknownToolFails(t *testing.T) {
	in, _ := testInbox(t, 1)
	if err := in.Send(1, "hello"); err == nil {
		t.Error("Send should fail when no driver registered for the tool")
	}
}

func TestSendAlreadyWorkingFails(t *testing.T) {
	in, _ := testInbox(t, 1)
	in.mu.Lock()
	in.projects[0].Status = driver.StatusWorking
	in.mu.Unlock()
	if err := in.Send(1, "hello"); err == nil {
		t.Error("Send should fail when project is already working")
	}
}
