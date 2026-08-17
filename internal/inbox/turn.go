package inbox

import (
	"context"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// A turn is one agent invocation, and it has an identity.
//
// Watchers used to wait for a project to stop being Working and then read
// whatever LastMessage held. That is not "wait for the turn I started" — it is
// "wait for the next time this project is idle", and the two differ whenever a
// second turn begins before the first one's watcher wakes up. The supervisor
// could read turn B's answer as the reply to turn A, and dispatch B's
// directives twice. A user's follow-up to a project could be consumed as that
// project's answer to something the supervisor asked.
//
// A TurnHandle resolves exactly once, for exactly the turn that produced it.

// TurnID identifies one agent turn within one process run.
type TurnID uint64

// TurnOutcome is how a turn ended.
type TurnOutcome struct {
	Project   string
	TurnID    TurnID
	SessionID string
	// Final is the assistant's reply. Empty when the turn errored before
	// producing one, or when it was cancelled.
	Final string
	// Partial is text the agent had produced when the turn failed. Preserved
	// so a failure does not throw away work the user watched arrive.
	Partial string
	Status  driver.Status
	Err     error
	// Cancelled distinguishes a turn the user stopped from one that failed.
	Cancelled bool
}

// TurnHandle is a claim on the completion of one specific turn.
type TurnHandle struct {
	Project string
	ID      TurnID
	Done    <-chan TurnOutcome
}

// activeTurn is the inbox's side of a handle.
type activeTurn struct {
	id     TurnID
	cancel context.CancelFunc
	done   chan TurnOutcome
	// resolved guards against double-send on done. Cancel and natural
	// completion race, and both want to be the one to report.
	resolved bool
}

// beginTurn registers a new turn for a project and returns its handle.
// Caller must hold in.mu.
func (in *Inbox) beginTurn(name string, cancel context.CancelFunc) (*activeTurn, TurnHandle) {
	in.nextTurnID++
	t := &activeTurn{
		id:     in.nextTurnID,
		cancel: cancel,
		// Buffered so resolving never blocks on a watcher that has gone away.
		done: make(chan TurnOutcome, 1),
	}
	in.active[name] = t
	return t, TurnHandle{Project: name, ID: t.ID(), Done: t.done}
}

// ID reports the turn's identity.
func (t *activeTurn) ID() TurnID { return t.id }

// resolveTurn completes a turn exactly once and clears it from the active map
// if it is still the current one. Caller must hold in.mu.
func (in *Inbox) resolveTurn(name string, t *activeTurn, out TurnOutcome) {
	if t.resolved {
		return
	}
	t.resolved = true
	out.Project = name
	out.TurnID = t.id
	t.done <- out
	close(t.done)
	if cur, ok := in.active[name]; ok && cur == t {
		delete(in.active, name)
	}
}

// isCurrentTurn reports whether t is still the turn this project is running.
// A turn that has been superseded must not write its result over the one that
// replaced it. Caller must hold in.mu.
func (in *Inbox) isCurrentTurn(name string, t *activeTurn) bool {
	cur, ok := in.active[name]
	return ok && cur == t
}
