package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// F13: an event is acknowledged (its spool file removed) only after its
// effect is durable in state.json. A failing save must leave the file behind,
// and the retried ingest must not double-apply the event.
func TestIngestCommitsBeforeAcknowledging(t *testing.T) {
	env := newMultiEnv(t)
	in := env.inbox(t, "alpha")
	events := filepath.Join(env.base, "events")
	if err := os.MkdirAll(events, 0o700); err != nil {
		t.Fatal(err)
	}

	// Poison persistence: a directory where the state lock file belongs makes
	// every save fail at the lock, while reads and the claims keep working.
	if err := os.MkdirAll(env.state+".lock", 0o700); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(env.base, "alpha")
	ts := time.Now().UnixNano()
	ev := Event{SessionID: "s1", Dir: dir, Tool: "mock", Message: "the reply", TS: ts}
	p := in.projectSetSession(t, "alpha", "s1")
	_ = p
	if err := WriteEvent(events, ev); err != nil {
		t.Fatal(err)
	}

	updated := in.Ingest(events)
	if len(updated) != 1 || updated[0] != "alpha" {
		t.Fatalf("event not applied in memory: %v", updated)
	}
	entries, _ := os.ReadDir(events)
	if len(entries) != 1 {
		t.Fatalf("event file was removed despite a failed commit: %d entries", len(entries))
	}

	// Repair the store; the next ingest must commit, remove the file, and
	// not apply the event a second time.
	os.RemoveAll(env.state + ".lock")
	in.Ingest(events)
	entries, _ = os.ReadDir(events)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("event file still present after a successful commit: %s", e.Name())
		}
	}
	hist, err := in.HistoryOf("alpha", 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range hist {
		if m.Content == "the reply" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("event message applied %d times, want exactly 1", n)
	}
}

// F17: a damaged state file is not an empty fleet. Refresh must keep the
// current view, surface the damage, and no destructive inference may run.
func TestDamagedStateKeepsMemoryAndSurfaces(t *testing.T) {
	env := newMultiEnv(t)
	in := env.inbox(t, "alpha", "bravo")
	if err := os.WriteFile(env.state, []byte(`{"projects": [broken`), 0o600); err != nil {
		t.Fatal(err)
	}

	in.RefreshExternal()
	for _, want := range []string{"alpha", "bravo"} {
		if _, err := in.projectByName(want); err != nil {
			t.Fatalf("%s vanished after refreshing over a damaged store: %v", want, err)
		}
	}
	if err := in.SaveErr(); err == nil {
		t.Fatal("damage was not surfaced anywhere")
	}

	// A save over the damaged store quarantines it rather than silently
	// overwriting the evidence.
	if err := in.save(); err == nil {
		t.Fatal("save over a damaged store reported success")
	}
	found := false
	for _, e := range mustReadDir(t, env.base) {
		if strings.HasPrefix(e, "state.json.bad-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("damaged store was not quarantined: %v", mustReadDir(t, env.base))
	}
}

// F19: a turn that ran and replied but could not commit is not reported as a
// durable success. The reply is preserved on the outcome; the error says what
// happened.
func TestUnpersistedReplyIsNotASuccess(t *testing.T) {
	env := newMultiEnv(t)
	alpha := filepath.Join(env.base, "alpha")
	if err := os.MkdirAll(alpha, 0o700); err != nil {
		t.Fatal(err)
	}
	in := New(
		[]*Project{{Name: "alpha", Tool: "rec", Dir: alpha, Status: driver.StatusIdle}},
		map[string]driver.Driver{"rec": &recDriver{seedWith: "s1"}},
		env.state,
	)
	t.Cleanup(in.Close)
	in.projectSetSession(t, "alpha", "s1")

	// Poison persistence so the post-turn commit fails: a directory where
	// the state lock belongs breaks every save, deterministically.
	if err := os.MkdirAll(env.state+".lock", 0o700); err != nil {
		t.Fatal(err)
	}

	out, ok := in.SendAndWait("alpha", "do work", 15*time.Second)
	if !ok {
		t.Fatal("turn did not resolve")
	}
	if out.Err == nil || !strings.Contains(out.Err.Error(), "not persisted") {
		t.Fatalf("outcome err = %v, want a persistence failure", out.Err)
	}
	if out.Final == "" {
		t.Fatal("the executed reply was thrown away with the commit")
	}
	os.RemoveAll(env.state + ".lock")
}

func (in *Inbox) projectSetSession(t *testing.T, name, sid string) *Project {
	t.Helper()
	in.mu.Lock()
	defer in.mu.Unlock()
	p, err := in.projectByName(name)
	if err != nil {
		t.Fatalf("project %s: %v", name, err)
	}
	p.SessionID = sid
	return p
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// F20: a freshly loaded Working status is reconciled against the claim, not
// rewritten blindly. A one-shot status reader is usually an observer —
// another live process owns the turn — so a held claim keeps Working, while
// a holderless claim becomes an interrupted idle, not a clean one.
func TestLoadStateReconcilesWorkingAgainstClaim(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state.json")
	writeWorkingState := func(t *testing.T) {
		t.Helper()
		b, _ := json.Marshal([]map[string]any{{
			"name": "alpha", "tool": "mock", "dir": repo, "session_id": "s1",
			"status": "working", "updated_at": time.Now().Format(time.RFC3339),
		}})
		if err := os.WriteFile(state, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newProjects := func() []*Project {
		return []*Project{{Name: "alpha", Tool: "mock", Dir: repo, Status: driver.StatusIdle}}
	}
	claimsDir := filepath.Join(base, "claims", "alpha")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Live foreign owner: another agent-inbox is mid-turn, and this load is
	// only watching. Working must stand.
	sleep := startSleep(t)
	writeWorkingState(t)
	writeClaim(t, claimsDir, sleep.Process.Pid, "t1")
	live := newProjects()
	LoadState(state, live)
	if live[0].Status != driver.StatusWorking {
		t.Fatalf("status = %q, want working — a live owner is mid-turn", live[0].Status)
	}

	// Owner gone: the turn was interrupted, which is not the same as idle
	// from a clean finish.
	sleep.Process.Kill()
	sleep.Wait()
	writeWorkingState(t)
	writeClaim(t, claimsDir, sleep.Process.Pid, "t1")
	gone := newProjects()
	LoadState(state, gone)
	if gone[0].Status != driver.StatusIdle {
		t.Fatalf("status = %q, want idle for a holderless turn", gone[0].Status)
	}
	if !strings.Contains(gone[0].LastErr, "interrupted") {
		t.Fatalf("LastErr = %q, want the interruption recorded", gone[0].LastErr)
	}
}
