package inbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

func eventInbox(t *testing.T, p *Project) *Inbox {
	t.Helper()
	in := New([]*Project{p}, map[string]driver.Driver{"mock": driver.Mock{}},
		filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(in.Close)
	return in
}

// A Stop hook fires for whichever tool wrote it. Labelling that event with the
// *project's* configured tool let a Claude session run by hand inside a Codex
// project overwrite that project's Codex session id with a Claude one.
func TestAnEventFromAnotherToolIsIgnored(t *testing.T) {
	p := &Project{Name: "api", Tool: "codex", Dir: t.TempDir(),
		SessionID: "codex-1", Status: driver.StatusIdle}
	in := eventInbox(t, p)

	if _, ok := in.applyEvent(Event{
		SessionID: "claude-9", Dir: p.Dir, Tool: "claude",
		Message: "hello", TS: time.Now().UnixNano(),
	}); ok {
		t.Fatal("a claude event was applied to a codex project")
	}
	got, _ := in.Detail(1)
	if got.SessionID != "codex-1" {
		t.Fatalf("session id was replaced: %q", got.SessionID)
	}
}

// Two Claude sessions can share a directory. Only the one this project owns
// speaks for it.
func TestAnEventFromADifferentSessionInTheSameDirIsIgnored(t *testing.T) {
	p := &Project{Name: "api", Tool: "claude", Dir: t.TempDir(),
		SessionID: "mine", Status: driver.StatusIdle}
	in := eventInbox(t, p)

	if _, ok := in.applyEvent(Event{
		SessionID: "someone-elses", Dir: p.Dir, Tool: "claude",
		Message: "not mine", TS: time.Now().UnixNano(),
	}); ok {
		t.Fatal("an unrelated session's event was applied")
	}
	got, _ := in.Detail(1)
	if got.SessionID != "mine" || got.LastMessage == "not mine" {
		t.Fatalf("state was taken over: %+v", got)
	}
}

// An event mid-turn would flip the project to waiting, let the UI send again,
// and leave the running subprocess to have its result discarded.
func TestAnEventCannotInterruptAManagedTurn(t *testing.T) {
	p := &Project{Name: "api", Tool: "claude", Dir: t.TempDir(),
		SessionID: "mine", Status: driver.StatusWorking}
	in := eventInbox(t, p)

	if _, ok := in.applyEvent(Event{
		SessionID: "mine", Dir: p.Dir, Tool: "claude",
		Message: "done", TS: time.Now().UnixNano(),
	}); ok {
		t.Fatal("an event was applied to a project mid-turn")
	}
	got, _ := in.Detail(1)
	if got.Status != driver.StatusWorking {
		t.Fatalf("status changed to %q", got.Status)
	}
}

// The matching case still works, or the feature does nothing.
func TestAMatchingEventUpdatesTheProject(t *testing.T) {
	p := &Project{Name: "api", Tool: "claude", Dir: t.TempDir(),
		SessionID: "mine", Status: driver.StatusIdle}
	in := eventInbox(t, p)

	name, ok := in.applyEvent(Event{
		SessionID: "mine", Dir: p.Dir, Tool: "claude",
		Message: "finished", TS: time.Now().UnixNano(),
	})
	if !ok || name != "api" {
		t.Fatalf("a matching event was rejected (ok=%v name=%q)", ok, name)
	}
	got, _ := in.Detail(1)
	if got.Status != driver.StatusWaiting || got.LastMessage != "finished" {
		t.Fatalf("event not applied: %+v", got)
	}
}

// A hook that is slow to write can arrive after a turn completed here.
func TestAStaleEventDoesNotRegressNewerState(t *testing.T) {
	now := time.Now()
	p := &Project{Name: "api", Tool: "claude", Dir: t.TempDir(),
		SessionID: "mine", Status: driver.StatusIdle,
		LastMessage: "newer", UpdatedAt: now}
	in := eventInbox(t, p)

	if _, ok := in.applyEvent(Event{
		SessionID: "mine", Dir: p.Dir, Tool: "claude",
		Message: "older", TS: now.Add(-time.Hour).UnixNano(),
	}); ok {
		t.Fatal("a stale event was applied")
	}
	got, _ := in.Detail(1)
	if got.LastMessage != "newer" {
		t.Fatalf("state went backwards: %q", got.LastMessage)
	}
}

// Two events from one session in the same second used to produce the same
// filename, and the second silently overwrote the first.
func TestTwoEventsInTheSameSecondBothSurvive(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().Truncate(time.Second).UnixNano()
	for _, msg := range []string{"first", "second"} {
		if err := WriteEvent(dir, Event{SessionID: "ses_abc", Dir: "/x", Tool: "claude", Message: msg, TS: ts}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected both events on disk, found %d", n)
	}
}

// Malformed input is quarantined, not deleted as though it had been handled.
func TestAMalformedEventIsQuarantinedRatherThanDropped(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "1-1-abc.json")
	if err := os.WriteFile(bad, []byte(`{"session_id": "trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
	in := eventInbox(t, &Project{Name: "api", Tool: "claude", Dir: t.TempDir(), Status: driver.StatusIdle})
	in.Ingest(dir)

	if _, err := os.Stat(bad + ".bad"); err != nil {
		t.Fatalf("malformed event was not quarantined: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("the original should have been renamed")
	}
}

func TestEventFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := filepath.Join(t.TempDir(), "events")
	if err := WriteEvent(dir, Event{SessionID: "s", Dir: "/x", Tool: "claude", Message: "secret", TS: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s has mode %04o", e.Name(), mode)
		}
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := di.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("events dir has mode %04o", mode)
	}
}
