package inbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// traceDriver streams, so the trace path runs: it reports two tool calls and
// a reply, which is the shape of every real streaming turn at one thousandth
// of the cost.
type traceDriver struct{}

func (traceDriver) Name() string { return "trace" }

func (traceDriver) Send(ctx context.Context, dir, sessionID, prompt string) driver.Result {
	d := traceDriver{}
	for ev := range d.StreamSend(ctx, dir, sessionID, prompt) {
		if ev.Kind == driver.StreamError {
			return driver.Result{Status: driver.StatusError, Err: ev.Err}
		}
	}
	return driver.Result{Status: driver.StatusWaiting}
}

func (traceDriver) AttachArgs(dir, sessionID string) []string { return nil }

func (traceDriver) StreamSend(ctx context.Context, dir, sessionID, prompt string) <-chan driver.StreamEvent {
	ch := make(chan driver.StreamEvent)
	go func() {
		defer close(ch)
		ch <- driver.StreamEvent{Kind: driver.StreamStarted, Activity: "starting", SessionID: "s1"}
		ch <- driver.StreamEvent{Kind: driver.StreamToolCall, Activity: "Bash", SessionID: "s1"}
		ch <- driver.StreamEvent{Kind: driver.StreamText, Content: "thinking about it", SessionID: "s1"}
		ch <- driver.StreamEvent{Kind: driver.StreamToolCall, Activity: "Edit", SessionID: "s1"}
		ch <- driver.StreamEvent{Kind: driver.StreamDone, Content: "done: fixed the thing", SessionID: "s1"}
	}()
	return ch
}

// The trace records what a turn did, in order, and survives the process that
// ran the turn — which is the point: the reader is usually somebody who was
// not watching.
func TestTraceRecordsTheTurn(t *testing.T) {
	env := newMultiEnv(t)
	if err := os.MkdirAll(filepath.Join(env.base, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	in := New(
		[]*Project{{Name: "alpha", Tool: "trace", Dir: filepath.Join(env.base, "alpha"), Status: driver.StatusIdle}},
		map[string]driver.Driver{"trace": traceDriver{}},
		env.state,
	)
	defer in.Close()

	if out, ok := in.SendAndWait("alpha", "fix it", 10*time.Second); !ok || out.Err != nil {
		t.Fatalf("turn: ok=%v err=%v", ok, out.Err)
	}

	var alpha Project
	for _, p := range in.Snapshot() {
		if p.Name == "alpha" {
			alpha = p
		}
	}
	// Tool calls, in order — and the text event is not one of them: the trace
	// is what the agent did, not what it said.
	if len(alpha.Trace) != 2 || alpha.Trace[0].Act != "Bash" || alpha.Trace[1].Act != "Edit" {
		t.Fatalf("trace = %+v", alpha.Trace)
	}

	// Persisted: a second process reads the same trace from state.json.
	saved := readStateFile(env.state)
	if len(saved) != 1 || len(saved[0].Trace) != 2 {
		t.Fatalf("trace not persisted: %+v", saved)
	}

	// And the next turn starts clean.
	if out, ok := in.SendAndWait("alpha", "again", 10*time.Second); !ok || out.Err != nil {
		t.Fatalf("second turn: ok=%v err=%v", ok, out.Err)
	}
	for _, p := range in.Snapshot() {
		if p.Name == "alpha" && len(p.Trace) != 2 {
			t.Fatalf("trace not reset for the new turn: %+v", p.Trace)
		}
	}
}
