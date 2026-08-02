package inbox

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentinbox/internal/driver"
)

// forkDriver records which of the two entry points a turn came through, so a
// test can tell an inherited session from a fresh one.
type forkDriver struct {
	mu       sync.Mutex
	forked   []string // source session ids passed to SendForked
	resumed  []string // session ids passed to Send
	failFork bool
}

func (f *forkDriver) Name() string { return "fork" }

func (f *forkDriver) Send(_ context.Context, _, sessionID, _ string) driver.Result {
	f.mu.Lock()
	f.resumed = append(f.resumed, sessionID)
	f.mu.Unlock()
	return driver.Result{SessionID: "own-1", Final: "hi", Status: driver.StatusWaiting}
}

func (f *forkDriver) SendForked(_ context.Context, _, source, _ string) driver.Result {
	f.mu.Lock()
	f.forked = append(f.forked, source)
	fail := f.failFork
	f.mu.Unlock()
	if fail {
		return driver.Result{Status: driver.StatusError, Err: context.DeadlineExceeded}
	}
	return driver.Result{SessionID: "forked-1", Final: "inherited", Status: driver.StatusWaiting}
}

func (f *forkDriver) AttachArgs(string, string) []string { return []string{"true"} }

func (f *forkDriver) calls() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forked...), append([]string(nil), f.resumed...)
}

func forkFixture(t *testing.T, d driver.Driver, p *Project) *Inbox {
	t.Helper()
	in := New([]*Project{p}, map[string]driver.Driver{"fork": d, "plain": d}, filepath.Join(t.TempDir(), "state.json"))
	in.pollEvery = time.Millisecond
	t.Cleanup(in.Close)
	return in
}

// An adopted session is inherited on the first send and owned from the second.
func TestForkOnFirstSendThenResume(t *testing.T) {
	d := &forkDriver{}
	p := &Project{Name: "neutron", Tool: "fork", Dir: "/repo", ForkFrom: "live-7", Status: driver.StatusIdle}
	in := forkFixture(t, d, p)

	if err := in.Send(1, "status?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "the fork turn", func() bool {
		forked, _ := d.calls()
		return len(forked) == 1
	})
	forked, resumed := d.calls()
	if forked[0] != "live-7" {
		t.Errorf("forked from %q, want live-7", forked[0])
	}
	if len(resumed) != 0 {
		t.Errorf("also sent an ordinary turn: %v", resumed)
	}

	snap := in.Snapshot()[0]
	if snap.SessionID != "forked-1" {
		t.Errorf("session = %q, want the fork's own id", snap.SessionID)
	}
	// Spent: the project has a session of its own now, and forking again
	// would throw away everything this turn just added to it.
	if snap.ForkFrom != "" {
		t.Errorf("fork source survived a successful turn: %q", snap.ForkFrom)
	}

	if err := in.Send(1, "and now?"); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	waitFor(t, "the resumed turn", func() bool {
		_, r := d.calls()
		return len(r) == 1
	})
	if _, r := d.calls(); r[0] != "forked-1" {
		t.Errorf("second turn resumed %q, want forked-1", r[0])
	}
}

// A fork that failed has produced no session, so the next attempt must still
// inherit rather than silently start blank.
func TestFailedForkKeepsItsSource(t *testing.T) {
	d := &forkDriver{failFork: true}
	p := &Project{Name: "neutron", Tool: "fork", Dir: "/repo", ForkFrom: "live-7", Status: driver.StatusIdle}
	in := forkFixture(t, d, p)

	if err := in.Send(1, "status?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "the failed fork", func() bool {
		return in.Snapshot()[0].Status == driver.StatusError
	})
	if got := in.Snapshot()[0].ForkFrom; got != "live-7" {
		t.Errorf("fork source = %q, want it kept for the retry", got)
	}
}

// A driver that cannot fork starts clean rather than resuming an id it has no
// claim to — resuming somebody else's session is how you get two writers on
// one transcript.
func TestForkFallsBackToAFreshSession(t *testing.T) {
	d := &plainDriver{}
	p := &Project{Name: "neutron", Tool: "plain", Dir: "/repo", ForkFrom: "live-7", Status: driver.StatusIdle}
	in := New([]*Project{p}, map[string]driver.Driver{"plain": d}, filepath.Join(t.TempDir(), "state.json"))
	in.pollEvery = time.Millisecond
	t.Cleanup(in.Close)

	if err := in.Send(1, "status?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "the fallback turn", func() bool { return len(d.seen()) == 1 })
	if got := d.seen()[0]; got != "" {
		t.Errorf("resumed %q, want a fresh session", got)
	}
}

// plainDriver implements Driver and nothing else.
type plainDriver struct {
	mu   sync.Mutex
	sids []string
}

func (p *plainDriver) Name() string { return "plain" }

func (p *plainDriver) Send(_ context.Context, _, sessionID, _ string) driver.Result {
	p.mu.Lock()
	p.sids = append(p.sids, sessionID)
	p.mu.Unlock()
	return driver.Result{SessionID: "own-1", Final: "hi", Status: driver.StatusWaiting}
}

func (p *plainDriver) AttachArgs(string, string) []string { return []string{"true"} }

func (p *plainDriver) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sids...)
}
