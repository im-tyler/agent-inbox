package inbox

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// gateDriver lets a test decide exactly when a turn finishes, so the races
// these tests are about are scheduled rather than hoped for.
type gateDriver struct {
	mu      sync.Mutex
	release []chan string
}

func (g *gateDriver) Name() string { return "gate" }

// next hands out the channel that will complete the Nth turn.
func (g *gateDriver) next() chan string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan string, 1)
	g.release = append(g.release, ch)
	return ch
}

func (g *gateDriver) Send(ctx context.Context, dir, sessionID, prompt string) driver.Result {
	g.mu.Lock()
	var ch chan string
	if len(g.release) > 0 {
		ch = g.release[0]
		g.release = g.release[1:]
	}
	g.mu.Unlock()
	if ch == nil {
		return driver.Result{Final: prompt, Status: driver.StatusWaiting}
	}
	select {
	case text := <-ch:
		return driver.Result{Final: text, Status: driver.StatusWaiting}
	case <-ctx.Done():
		return driver.Result{Status: driver.StatusError, Err: ctx.Err()}
	}
}

func (g *gateDriver) AttachArgs(dir, sessionID string) []string { return []string{"gate"} }

func gateInbox(t *testing.T) (*Inbox, *gateDriver) {
	t.Helper()
	g := &gateDriver{}
	in := New([]*Project{
		{Name: "king", Tool: "gate", Dir: t.TempDir(), Status: driver.StatusIdle},
	}, map[string]driver.Driver{"gate": g}, filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(in.Close)
	return in, g
}

// A handle resolves for the turn that produced it, not for whatever the
// project happens to finish next.
//
// Watchers used to poll for "not working" and then read LastMessage. With two
// turns in flight in sequence, turn A's watcher could wake after turn B had
// started and finished, and read B's answer as A's — then dispatch B's
// directives a second time.
func TestATurnHandleResolvesWithItsOwnResult(t *testing.T) {
	in, g := gateInbox(t)

	gateA := g.next()
	handleA, err := in.startSend(func() (*Project, error) { return in.project(1) }, "a", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	gateA <- "answer-A"

	outA := waitOutcome(t, handleA)
	if outA.Final != "answer-A" {
		t.Fatalf("handle A resolved with %q", outA.Final)
	}

	// A second turn, started after the first completed but before anything
	// read A's handle a second time.
	gateB := g.next()
	handleB, err := in.startSend(func() (*Project, error) { return in.project(1) }, "b", "b", true)
	if err != nil {
		t.Fatal(err)
	}
	gateB <- "answer-B"

	outB := waitOutcome(t, handleB)
	if outB.Final != "answer-B" {
		t.Fatalf("handle B resolved with %q", outB.Final)
	}
	if outA.TurnID == outB.TurnID {
		t.Fatal("two turns shared an id")
	}
}

// Cancelling resolves the waiter immediately and says why, rather than
// leaving it to time out against a project that is now idle.
func TestCancellingATurnResolvesItsHandleAsCancelled(t *testing.T) {
	in, g := gateInbox(t)
	g.next() // never released

	handle, err := in.startSend(func() (*Project, error) { return in.project(1) }, "x", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Cancel(1); err != nil {
		t.Fatal(err)
	}
	out := waitOutcome(t, handle)
	if !out.Cancelled {
		t.Fatalf("expected a cancelled outcome, got %+v", out)
	}
}

// A superseded turn finishing late must not overwrite the turn that replaced
// it, and must not resolve as though it were that turn.
func TestALateResultDoesNotOverwriteANewerTurn(t *testing.T) {
	in, g := gateInbox(t)

	gateA := g.next()
	if _, err := in.startSend(func() (*Project, error) { return in.project(1) }, "a", "a", true); err != nil {
		t.Fatal(err)
	}
	// Cancel A, which ends its claim on the project.
	if err := in.Cancel(1); err != nil {
		t.Fatal(err)
	}

	gateB := g.next()
	handleB, err := in.startSend(func() (*Project, error) { return in.project(1) }, "b", "b", true)
	if err != nil {
		t.Fatal(err)
	}
	gateB <- "answer-B"
	if out := waitOutcome(t, handleB); out.Final != "answer-B" {
		t.Fatalf("B resolved with %q", out.Final)
	}

	// Now let the cancelled turn return. It must change nothing.
	gateA <- "answer-A"
	time.Sleep(100 * time.Millisecond)

	p, err := in.Detail(1)
	if err != nil {
		t.Fatal(err)
	}
	if p.LastMessage != "answer-B" {
		t.Fatalf("a superseded turn overwrote the current one: LastMessage = %q", p.LastMessage)
	}
}

// Close must release anyone waiting on a turn rather than leaving them to
// block until their own deadline.
func TestCloseResolvesOutstandingHandles(t *testing.T) {
	g := &gateDriver{}
	in := New([]*Project{
		{Name: "king", Tool: "gate", Dir: t.TempDir(), Status: driver.StatusIdle},
	}, map[string]driver.Driver{"gate": g}, filepath.Join(t.TempDir(), "state.json"))
	g.next() // never released

	handle, err := in.startSend(func() (*Project, error) { return in.project(1) }, "x", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	go in.Close()
	out := waitOutcome(t, handle)
	if !out.Cancelled {
		t.Fatalf("expected shutdown to cancel the turn, got %+v", out)
	}
}

// track must refuse work once Close has begun, or a watcher scheduling its
// next round can outlive the Close that promised to wait for it.
func TestTrackRefusesWorkAfterClose(t *testing.T) {
	in, _ := gateInbox(t)
	in.Close()
	if in.track(func() {}) {
		t.Fatal("work was accepted after Close")
	}
}

func waitOutcome(t *testing.T, h TurnHandle) TurnOutcome {
	t.Helper()
	select {
	case out := <-h.Done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("turn handle never resolved")
		return TurnOutcome{}
	}
}
