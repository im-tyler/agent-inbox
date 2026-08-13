package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

func reasonFixture(t *testing.T) (*Inbox, string) {
	t.Helper()
	dir := t.TempDir()
	projects := []*Project{
		{Name: "supervisor", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "omni", Tool: "claude", Dir: dir, SessionID: "sess-1", Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json")).
		WithKing("supervisor")
	t.Cleanup(in.Close)
	return in, dir
}

func spool(t *testing.T, in *Inbox, ev Event) []string {
	t.Helper()
	dir := t.TempDir()
	if err := WriteEvent(dir, ev); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	return in.Ingest(dir)
}

// An event with no reason is what every hook wrote before this field existed.
// Rejecting it, or treating it as unknown, would drop events written by an
// older binary that is still installed.
func TestEventWithNoReasonIsDone(t *testing.T) {
	in, dir := reasonFixture(t)
	updated := spool(t, in, Event{
		SessionID: "sess-1", Dir: dir, Tool: "claude",
		Message: "finished", TS: time.Now().UnixNano(),
	})
	if len(updated) != 1 {
		t.Fatalf("updated = %v, want [omni]", updated)
	}
	p := projectNamed(t, in, "omni")
	if p.Status != driver.StatusWaiting {
		t.Errorf("status = %s, want waiting", p.Status)
	}
	if p.WaitReason != ReasonDone {
		t.Errorf("WaitReason = %q, want done", p.WaitReason)
	}
}

// Blocked on a permission prompt is not the same as finished, and the whole
// point of the reason is that the two call for opposite actions.
func TestPermissionEventIsDistinguishable(t *testing.T) {
	in, dir := reasonFixture(t)
	spool(t, in, Event{
		SessionID: "sess-1", Dir: dir, Tool: "claude",
		Reason: ReasonPermission, Detail: "Bash(rm -rf build)",
		TS: time.Now().UnixNano(),
	})
	p := projectNamed(t, in, "omni")
	if p.WaitReason != ReasonPermission {
		t.Fatalf("WaitReason = %q, want permission", p.WaitReason)
	}
	if !p.WaitReason.Blocking() {
		t.Error("a permission prompt did not report as blocking")
	}
	if p.WaitDetail != "Bash(rm -rf build)" {
		t.Errorf("WaitDetail = %q", p.WaitDetail)
	}
	// The session said nothing, so the thread has to say why it stopped.
	last := p.History[len(p.History)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "permission") {
		t.Errorf("no line explaining the block: %+v", last)
	}
}

func TestErrorEventSetsErrorStatus(t *testing.T) {
	in, dir := reasonFixture(t)
	spool(t, in, Event{
		SessionID: "sess-1", Dir: dir, Tool: "claude",
		Reason: ReasonError, Detail: "provider refused", TS: time.Now().UnixNano(),
	})
	p := projectNamed(t, in, "omni")
	if p.Status != driver.StatusError {
		t.Errorf("status = %s, want error", p.Status)
	}
	if p.LastErr != "provider refused" {
		t.Errorf("LastErr = %q", p.LastErr)
	}
}

// A reason nobody here knows about is still an event that really happened.
// Discarding it over a label would lose it entirely.
func TestUnknownReasonDegradesToDone(t *testing.T) {
	if got := ParseReason("elaborate-new-thing"); got != ReasonDone {
		t.Errorf("ParseReason = %q, want done", got)
	}
	if got := ParseReason("PERMISSION"); got != ReasonPermission {
		t.Errorf("ParseReason is case sensitive: %q", got)
	}
}

// Sending supersedes whatever it was blocked on. A reason left behind keeps
// the badge up through every turn that follows.
func TestSendingClearsTheBlockedReason(t *testing.T) {
	in, dir := reasonFixture(t)
	spool(t, in, Event{
		SessionID: "sess-1", Dir: dir, Tool: "claude",
		Reason: ReasonPermission, Detail: "Bash(ls)", TS: time.Now().UnixNano(),
	})
	in.mu.Lock()
	p, _ := in.projectByName("omni")
	p.Tool = "mock" // so the send actually runs
	in.mu.Unlock()

	if err := in.Send(2, "go ahead"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := projectNamed(t, in, "omni").WaitReason; got != "" {
		t.Errorf("WaitReason = %q after a send, want empty", got)
	}
}

// Dismissing the badge clears the reason with it, or the badge comes straight
// back on the next render.
func TestCancelClearsTheBlockedReason(t *testing.T) {
	in, dir := reasonFixture(t)
	spool(t, in, Event{
		SessionID: "sess-1", Dir: dir, Tool: "claude",
		Reason: ReasonPermission, TS: time.Now().UnixNano(),
	})
	if err := in.Cancel(2); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := projectNamed(t, in, "omni").WaitReason; got != "" {
		t.Errorf("WaitReason = %q after dismissal, want empty", got)
	}
}

// A project blocked on a prompt is still blocked after a restart. Coming back
// as an undifferentiated "waiting" loses the fact that decides what to do.
func TestBlockedReasonSurvivesRestart(t *testing.T) {
	in, dir := reasonFixture(t)
	spool(t, in, Event{
		SessionID: "sess-1", Dir: dir, Tool: "claude",
		Reason: ReasonPermission, Detail: "Write(/etc/hosts)", TS: time.Now().UnixNano(),
	})
	in.save()

	reloaded := []*Project{{Name: "omni", Tool: "claude", Dir: dir}}
	LoadState(in.statePath, reloaded)
	if reloaded[0].WaitReason != ReasonPermission {
		t.Errorf("WaitReason = %q after reload, want permission", reloaded[0].WaitReason)
	}
	if reloaded[0].WaitDetail != "Write(/etc/hosts)" {
		t.Errorf("WaitDetail = %q after reload", reloaded[0].WaitDetail)
	}
}

// The supervisor has to be able to tell "it answered" from "it is stuck", and
// the status line is the only place it can.
func TestKingContextNamesTheBlock(t *testing.T) {
	in, dir := reasonFixture(t)
	spool(t, in, Event{
		SessionID: "sess-1", Dir: dir, Tool: "claude",
		Reason: ReasonPermission, Detail: "Bash(git push)", TS: time.Now().UnixNano(),
	})
	ctx := in.formatKingState([]string{"omni"})
	if !strings.Contains(ctx, "waiting:permission") {
		t.Errorf("status does not name the block:\n%s", ctx)
	}
	if !strings.Contains(ctx, "Bash(git push)") {
		t.Errorf("the specific ask is missing:\n%s", ctx)
	}
}

func TestWriteEventRoundTripsTheReason(t *testing.T) {
	dir := t.TempDir()
	if err := WriteEvent(dir, Event{
		SessionID: "s", Dir: "/d", Tool: "claude",
		Reason: ReasonQuestion, Detail: "which database?", TS: 42,
	}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("wrote %d files", len(entries))
	}
	b, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	for _, want := range []string{`"reason": "question"`, `"detail": "which database?"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in:\n%s", want, b)
		}
	}
}

func projectNamed(t *testing.T, in *Inbox, name string) Project {
	t.Helper()
	for _, p := range in.Snapshot() {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no project %q", name)
	return Project{}
}
