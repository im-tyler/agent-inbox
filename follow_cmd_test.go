package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/inbox"
)

// diffFleet is follow's judgment: what counts as a change and what does not.
// Tested directly because the stakes are attention — a wake that fires on
// bookkeeping teaches a harness to ignore wakes.
func TestDiffFleetReportsStatusChangesOnly(t *testing.T) {
	base := map[string]fleetPin{
		"alpha": {Status: "idle"},
		"bravo": {Status: "working"},
	}
	now := map[string]fleetPin{
		"alpha": {Status: "idle"},                        // unchanged
		"bravo": {Status: "waiting", WaitReason: "done"}, // changed
		"gamma": {Status: "idle"},                        // new
	}
	env := newFleetDiffInbox(t, "alpha", "bravo", "gamma")
	defer env.Close()

	changes := diffFleet(base, now, env, nil)
	if len(changes) != 2 {
		t.Fatalf("%d changes, want 2 (bravo changed, gamma new): %+v", len(changes), changes)
	}
	byName := map[string]fleetChangeRow{}
	for _, c := range changes {
		byName[c.Project] = c
	}
	if byName["bravo"].From != "working" || byName["bravo"].To != "waiting" {
		t.Fatalf("bravo transition wrong: %+v", byName["bravo"])
	}
	if byName["gamma"].From != "" {
		t.Fatalf("a new project has no From: %+v", byName["gamma"])
	}
	// The --projects filter limits reporting, not the watching.
	changes = diffFleet(base, now, env, map[string]bool{"gamma": true})
	if len(changes) != 1 || changes[0].Project != "gamma" {
		t.Fatalf("filter ignored: %+v", changes)
	}
}

// follow end to end: a send from another process wakes it, with the delta it
// was woken for.
func TestFollowWakesOnAnotherProcessSend(t *testing.T) {
	fleetEnv(t)
	var out, errOut bytes.Buffer
	var wg sync.WaitGroup
	var followErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		followErr = runFleet([]string{"follow", "--timeout", "15s", "--json"}, &out, &errOut)
	}()
	// Give follow its baseline, then change the fleet the way a harness
	// would: a real send from a real second process.
	time.Sleep(1500 * time.Millisecond)
	var sendOut bytes.Buffer
	if err := runFleet([]string{"send", "alpha", "hello"}, &sendOut, &sendOut); err != nil {
		t.Fatalf("send: %v", err)
	}
	wg.Wait()

	if followErr != nil {
		t.Fatalf("follow did not wake: %v (out: %s err: %s)", followErr, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), `"changed"`) || !strings.Contains(out.String(), "alpha") {
		t.Fatalf("wake delta missing alpha: %s", out.String())
	}
}

func TestFollowTimeoutReportsNoChange(t *testing.T) {
	fleetEnv(t)
	var out bytes.Buffer
	err := runFleet([]string{"follow", "--timeout", "1s"}, &out, &out)
	if err == nil {
		t.Fatal("timeout reported as a change")
	}
	if !errors.Is(err, errNoChange) {
		t.Fatalf("timeout error is not errNoChange: %v", err)
	}
}

// newFleetDiffInbox builds the minimal inbox diffFleet reads last messages
// from; it exists so the test does not pay fleetInbox's config load.
func newFleetDiffInbox(t *testing.T, names ...string) *inbox.Inbox {
	t.Helper()
	projects := make([]*inbox.Project, len(names))
	for i, n := range names {
		projects[i] = &inbox.Project{Name: n, Tool: "mock", Status: "idle"}
	}
	in := inbox.New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, "")
	t.Cleanup(in.Close)
	return in
}

// A full round trip between two polls — waiting, working, waiting — leaves
// the status fingerprint exactly where it started, so status alone never
// wakes on it. The revision is what sees the completed turn (F21).
func TestDiffFleetSeesCompletedTurnBetweenPolls(t *testing.T) {
	mk := func(rev uint64) map[string]fleetPin {
		return fleetFingerprint([]inbox.Project{
			{Name: "alpha", Status: driver.StatusWaiting, WaitReason: "done", Revision: rev},
		})
	}
	env := newFleetDiffInbox(t, "alpha")
	defer env.Close()

	if changes := diffFleet(mk(7), mk(7), env, nil); len(changes) != 0 {
		t.Fatalf("bookkeeping-only snapshot woke follow: %+v", changes)
	}
	changes := diffFleet(mk(7), mk(8), env, nil)
	if len(changes) != 1 || changes[0].Project != "alpha" {
		t.Fatalf("a completed turn between polls was missed: %+v", changes)
	}
}

// End to end: a hook event that lands after the baseline and applies between
// polls must wake follow, even when neither status nor wait reason moves.
func TestFollowWakesOnHookEventBetweenPolls(t *testing.T) {
	dd := fleetEnv(t)
	repo := dd + "/repo"
	ts := time.Now().Format(time.RFC3339)
	seed := `[{"name":"alpha","tool":"mock","dir":"` + repo + `","session_id":"sess-pre",` +
		`"status":"waiting","wait_reason":"done","last_message":"old","revision":5,"updated_at":"` + ts + `"}]`
	if err := os.WriteFile(dd+"/state.json", []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	var wg sync.WaitGroup
	var followErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		followErr = runFleet([]string{"follow", "--timeout", "15s", "--json"}, &out, &errOut)
	}()
	// Baseline first; then a completed external turn arrives between polls.
	time.Sleep(2000 * time.Millisecond)
	if err := inbox.WriteEvent(dd+"/events", inbox.Event{
		SessionID: "sess-pre", Dir: repo, Tool: "mock",
		Message: "turn done", TS: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("write event: %v", err)
	}
	wg.Wait()

	if followErr != nil {
		t.Fatalf("follow missed an applied event: %v (out: %s err: %s)", followErr, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), `"changed"`) || !strings.Contains(out.String(), "alpha") {
		t.Fatalf("wake delta missing alpha: %s", out.String())
	}
}
