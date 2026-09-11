package inbox

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// recDriver records the session id each Send was invoked with, and echoes or
// seeds one — the minimum needed to observe which session a turn ran against.
type recDriver struct {
	mu       sync.Mutex
	got      []string
	dirs     []string
	seedWith string // returned as the session when the caller passed ""
	block    chan struct{}
}

func (d *recDriver) Name() string { return "rec" }

func (d *recDriver) Send(ctx context.Context, dir, sessionID, prompt string) driver.Result {
	d.mu.Lock()
	d.got = append(d.got, sessionID)
	d.dirs = append(d.dirs, dir)
	sid := sessionID
	d.mu.Unlock()
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return driver.Result{SessionID: sid, Status: driver.StatusError, Err: ctx.Err()}
		}
	}
	if sid == "" {
		sid = d.seedWith
	}
	return driver.Result{SessionID: sid, Final: "reply to " + prompt, Status: driver.StatusWaiting}
}

func (d *recDriver) AttachArgs(dir, sessionID string) []string {
	return []string{"echo", sessionID}
}

func (d *recDriver) received() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got...)
}

func (d *recDriver) receivedDirs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dirs...)
}

// F04: a long-lived inbox that wins the claim after another process finished
// a turn must send against the session on disk, not the snapshot it loaded at
// startup — and the merged history must contain both turns.
func TestSendRefreshesSessionUnderClaim(t *testing.T) {
	env := newMultiEnv(t)
	alpha := filepath.Join(env.base, "alpha")
	if err := os.MkdirAll(alpha, 0o700); err != nil {
		t.Fatal(err)
	}

	a := New(
		[]*Project{{Name: "alpha", Tool: "rec", Dir: alpha, Status: driver.StatusIdle}},
		map[string]driver.Driver{"rec": &recDriver{seedWith: "session-A"}},
		env.state,
	)
	t.Cleanup(a.Close)

	// b is the long-lived inbox: constructed before a's turn, so its memory
	// never sees the session a is about to create.
	b := New(
		[]*Project{{Name: "alpha", Tool: "rec", Dir: alpha, Status: driver.StatusIdle}},
		map[string]driver.Driver{"rec": &recDriver{seedWith: "session-B"}},
		env.state,
	)
	t.Cleanup(b.Close)

	if out, ok := a.SendAndWait("alpha", "first turn", 15*time.Second); !ok || out.Err != nil {
		t.Fatalf("a's turn: ok=%v err=%v", ok, out.Err)
	}

	// No RefreshExternal on b — the send itself must refresh under the claim.
	if out, ok := b.SendAndWait("alpha", "second turn", 15*time.Second); !ok || out.Err != nil {
		t.Fatalf("b's turn: ok=%v err=%v", ok, out.Err)
	}

	bd := b.drivers["rec"].(*recDriver)
	got := bd.received()
	if len(got) != 1 || got[0] != "session-A" {
		t.Fatalf("b's driver received session %v, want [session-A] — stale snapshot was used", got)
	}

	hist, err := b.HistoryOf("alpha", 20)
	if err != nil {
		t.Fatal(err)
	}
	prompts := 0
	for _, m := range hist {
		if m.Role == "user" {
			prompts++
		}
	}
	if prompts != 2 {
		t.Fatalf("history has %d user messages, want 2 (both turns)", prompts)
	}
}

// F05: a tool change is serialised against sends by the same claim. A second
// inbox — a separate "process" — cannot switch the tool while a turn is
// in-flight, because the claim is taken in the filesystem where both see it.
func TestToolChangeSerializesWithSend(t *testing.T) {
	env := newMultiEnv(t)
	alpha := filepath.Join(env.base, "alpha")
	if err := os.MkdirAll(alpha, 0o700); err != nil {
		t.Fatal(err)
	}

	slow := &recDriver{seedWith: "s1", block: make(chan struct{})}
	a := New(
		[]*Project{{Name: "alpha", Tool: "rec", Dir: alpha, Status: driver.StatusIdle}},
		map[string]driver.Driver{"rec": slow},
		env.state,
	)
	t.Cleanup(a.Close)

	b := New(
		[]*Project{{Name: "alpha", Tool: "rec", Dir: alpha, Status: driver.StatusIdle}},
		map[string]driver.Driver{"rec": &recDriver{}, "other": &recDriver{}},
		env.state,
	)
	t.Cleanup(b.Close)

	done := make(chan TurnOutcome, 1)
	go func() {
		out, _ := a.SendAndWait("alpha", "slow turn", 30*time.Second)
		done <- out
	}()

	// Wait until a's turn is actually in-flight, then try to switch tools
	// from b. The claim must refuse it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(slow.received()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(slow.received()) != 1 {
		t.Fatal("turn never started")
	}
	if err := b.SetProjectTool(1, "other"); err == nil {
		t.Fatal("tool change succeeded while another process was mid-turn")
	}

	// Let the turn finish; the change now goes through.
	close(slow.block)
	select {
	case out := <-done:
		if out.Err != nil {
			t.Fatalf("turn errored: %v", out.Err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("turn did not finish")
	}
	// b's memory still says the old tool; refresh so the project resolves.
	b.RefreshExternal()
	for i, p := range b.Snapshot() {
		if p.Name == "alpha" {
			if err := b.SetProjectTool(i+1, "other"); err != nil {
				t.Fatalf("tool change after turn: %v", err)
			}
			break
		}
	}
	var got Project
	for _, p := range b.Snapshot() {
		if p.Name == "alpha" {
			got = p
		}
	}
	if got.Tool != "other" || got.SessionID != "" {
		t.Fatalf("after switch: tool=%q session=%q, want other/empty", got.Tool, got.SessionID)
	}
}

// F11: cancelling the requester's context cancels the turn it started — the
// child is killed, the outcome reports cancellation, and no successor turn is
// touched.
func TestRequestCancellationCancelsTurn(t *testing.T) {
	env := newMultiEnv(t)
	alpha := filepath.Join(env.base, "alpha")
	if err := os.MkdirAll(alpha, 0o700); err != nil {
		t.Fatal(err)
	}
	slow := &recDriver{seedWith: "s1", block: make(chan struct{})}
	defer close(slow.block)
	in := New(
		[]*Project{{Name: "alpha", Tool: "rec", Dir: alpha, Status: driver.StatusIdle}},
		map[string]driver.Driver{"rec": slow},
		env.state,
	)
	t.Cleanup(in.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		out TurnOutcome
		ok  bool
	}, 1)
	go func() {
		out, ok := in.SendAndWaitCtx(ctx, "alpha", "slow work", 30*time.Second)
		done <- struct {
			out TurnOutcome
			ok  bool
		}{out, ok}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(slow.received()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(slow.received()) != 1 {
		t.Fatal("turn never started")
	}

	start := time.Now()
	cancel()
	var res struct {
		out TurnOutcome
		ok  bool
	}
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled request never returned")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("cancellation took %s — the turn was not cancelled", d)
	}
	if res.ok || !res.out.Cancelled {
		t.Fatalf("outcome = %+v ok=%v, want a cancelled failure", res.out, res.ok)
	}
}
