package inbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// The tests in this file are about more than one process. They cannot easily
// be actual separate processes, so they use the next best thing: two Inbox
// instances over the same files, each with its own memory — which is exactly
// the relationship two processes have. Everything the merge, the claims and
// RefreshExternal coordinate is observable through those two memories and the
// state file underneath them.

// multiTestEnv is the shared world two "processes" contend over: one state
// file and one directory per project. The directories are shared because real
// front-ends derive them from one config — two inboxes that disagreed about
// where a project lived would not be two views of one fleet.
type multiTestEnv struct {
	state string
	base  string
}

func newMultiEnv(t *testing.T) multiTestEnv {
	t.Helper()
	base := t.TempDir()
	return multiTestEnv{state: filepath.Join(base, "state.json"), base: base}
}

func (e multiTestEnv) inbox(t *testing.T, names ...string) *Inbox {
	t.Helper()
	projects := make([]*Project, len(names))
	for i, n := range names {
		dir := filepath.Join(e.base, n)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		projects[i] = &Project{Name: n, Tool: "mock", Dir: dir, Status: driver.StatusIdle}
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, e.state)
	t.Cleanup(in.Close)
	return in
}

// A turn filed by one inbox must survive a save from a second inbox whose
// memory never saw it. This is the failure the merge exists for: before it,
// the second save wrote a whole snapshot from stale memory and silently
// reverted the first inbox's session id and reply.
func TestSaveDoesNotRevertAnotherInboxTurn(t *testing.T) {
	env := newMultiEnv(t)
	a := env.inbox(t, "alpha")
	b := env.inbox(t, "alpha", "bravo")

	out, ok := a.SendAndWait("alpha", "hello", 10*time.Second)
	if !ok || out.Err != nil {
		t.Fatalf("alpha turn: ok=%v err=%v", ok, out.Err)
	}

	// b holds alpha from before the turn — the stale-memory case — and has
	// its own reason to save: bravo changed.
	b.mu.Lock()
	if p, err := b.projectByName("bravo"); err != nil {
		t.Fatal(err)
	} else {
		p.LastMessage = "bravo's own update"
		p.UpdatedAt = time.Now()
	}
	b.mu.Unlock()
	b.save()

	saved := readStateFileOrFatal(t, env.state)
	var alpha Project
	for _, p := range saved {
		if p.Name == "alpha" {
			alpha = p
		}
	}
	if alpha.LastMessage != out.Final {
		t.Fatalf("alpha's reply was reverted by b's save: %q != %q", alpha.LastMessage, out.Final)
	}
	if alpha.SessionID == "" {
		t.Fatal("alpha's session id was reverted to empty")
	}
}

// What one inbox writes, the other adopts on RefreshExternal — the dashboard
// showing a harness-driven turn while it runs rather than at next restart.
func TestRefreshExternalAdoptsAnotherInboxTurn(t *testing.T) {
	env := newMultiEnv(t)
	a := env.inbox(t, "alpha")
	b := env.inbox(t, "alpha")

	if out, ok := b.SendAndWait("alpha", "hello", 10*time.Second); !ok || out.Err != nil {
		t.Fatalf("turn: ok=%v err=%v", ok, out.Err)
	}

	a.RefreshExternal()
	var got Project
	for _, p := range a.Snapshot() {
		if p.Name == "alpha" {
			got = p
		}
	}
	if got.LastMessage == "" || got.SessionID == "" {
		t.Fatalf("external turn not adopted: %+v", got)
	}
	if got.Status != driver.StatusWaiting {
		t.Fatalf("adopted status = %q, want waiting", got.Status)
	}
}

// Membership changes flow too: a project added by another front-end appears,
// one removed disappears — without which a removal issued by a harness would
// need a dashboard restart to be respected, and an addition would never be.
func TestRefreshExternalAdoptsMembership(t *testing.T) {
	env := newMultiEnv(t)
	a := env.inbox(t, "alpha")
	b := env.inbox(t, "alpha", "bravo")

	// b's save writes bravo's entry; a adopts the new member.
	b.mu.Lock()
	if p, err := b.projectByName("bravo"); err != nil {
		t.Fatal(err)
	} else {
		p.LastMessage = "here"
		p.UpdatedAt = time.Now()
	}
	b.mu.Unlock()
	b.save()

	a.RefreshExternal()
	if _, err := a.projectByName("bravo"); err != nil {
		t.Fatalf("added project not adopted: %v", err)
	}

	// b removes bravo explicitly; a adopts the removal.
	if err := b.RemoveProject(len(b.Snapshot())); err != nil {
		t.Fatalf("remove: %v", err)
	}
	a.RefreshExternal()
	if _, err := a.projectByName("bravo"); err == nil {
		t.Fatal("removed project still present after refresh")
	}
}

// A claim from a live foreign process refuses the send; a claim whose holder
// is dead is reclaimed. The live foreign pid is a real child process, because
// "probably nobody has that pid" is exactly the assumption the guard exists
// to avoid making.
func TestSendRespectsForeignClaim(t *testing.T) {
	env := newMultiEnv(t)
	in := env.inbox(t, "alpha")

	sleep := startSleep(t)
	dir := filepath.Join(filepath.Dir(env.state), "claims", "alpha")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaim(t, dir, sleep.Process.Pid, "foreign-turn")

	_, ok := in.SendAndWait("alpha", "hello", 2*time.Second)
	if ok {
		t.Fatal("send proceeded despite a live foreign claim")
	}

	// Holder gone: the claim is reclaimed, not obeyed forever.
	sleep.Process.Kill()
	sleep.Wait()
	writeClaim(t, dir, sleep.Process.Pid, "dead-turn")
	if out, ok := in.SendAndWait("alpha", "hello", 10*time.Second); !ok || out.Err != nil {
		t.Fatalf("dead claim not reclaimed: ok=%v err=%v", ok, out.Err)
	}
}

// SendAndWait resolves with the turn's own reply and adopts the session id
// the turn produced — the primitive every headless front-end stands on.
func TestSendAndWaitRoundTrip(t *testing.T) {
	env := newMultiEnv(t)
	in := env.inbox(t, "alpha")

	out, ok := in.SendAndWait("alpha", "do the thing", 15*time.Second)
	if !ok {
		t.Fatal("turn did not complete")
	}
	if out.Err != nil {
		t.Fatalf("turn errored: %v", out.Err)
	}
	if out.Final == "" {
		t.Fatal("no reply")
	}
	p, err := in.Detail(len(in.Snapshot()))
	if err != nil {
		t.Fatal(err)
	}
	if p.SessionID == "" {
		t.Fatal("session id not adopted")
	}

	// Unknown target is an error in the outcome, not a panic or a hang.
	if out, ok := in.SendAndWait("nobody", "x", time.Second); ok || out.Err == nil {
		t.Fatalf("unknown project: ok=%v err=%v", ok, out.Err)
	}
}

// Notes survive being written by two inboxes in sequence: the second re-reads
// inside the lock rather than saving its stale copy over the first's work.
func TestNotesAcrossInboxes(t *testing.T) {
	env := newMultiEnv(t)
	notes := filepath.Join(env.base, "notes.json")
	a := env.inbox(t, "alpha")
	a.WithNotesPath(notes)
	b := env.inbox(t, "alpha")
	b.WithNotesPath(notes)

	a.AddNotes([]string{"teploy depends on neutron's client"})
	b.AddNotes([]string{"omni's provider key expired"})

	got := b.Notes()
	if len(got) != 2 {
		t.Fatalf("second writer dropped the first's note: %d notes", len(got))
	}
	// And a drop from the first is not resurrected by the second: b's next
	// write re-reads inside the lock, so the dropped note stays dropped.
	if n := a.DropNotes([]string{"provider key"}); n != 1 {
		t.Fatalf("drop removed %d, want 1", n)
	}
	b.AddNotes([]string{"another fact"})
	if len(b.Notes()) != 2 {
		t.Fatalf("drop was resurrected: %v", b.Notes())
	}
}

func TestHistoryOf(t *testing.T) {
	env := newMultiEnv(t)
	in := env.inbox(t, "alpha")
	if _, ok := in.SendAndWait("alpha", "hello", 15*time.Second); !ok {
		t.Fatal("turn did not complete")
	}
	msgs, err := in.HistoryOf("alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("history has %d messages, want 2 (prompt + reply)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("roles = %s, %s", msgs[0].Role, msgs[1].Role)
	}
}

// startSleep produces a genuinely live foreign pid: a real process the claim
// logic can find alive and later find dead.
func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd
}

func writeClaim(t *testing.T, dir string, pid int, turn string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"pid":   pid,
		"turn":  turn,
		"taken": time.Now().Format(time.RFC3339),
	})
	if err := os.WriteFile(filepath.Join(dir, "claim.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readStateFileOrFatal adapts the error-returning state read for tests that
// only want the slice.
func readStateFileOrFatal(t *testing.T, path string) []Project {
	t.Helper()
	disk, err := readStateFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return disk
}
