package driver

import "context"

// Status is the normalized lifecycle state of a project's agent session,
// identical regardless of which underlying CLI produced it.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusWorking Status = "working"
	StatusWaiting Status = "waiting"
	StatusError   Status = "error"
)

// Result is the normalized outcome of one turn.
type Result struct {
	SessionID string
	Final     string
	Status    Status
	Err       error
}

// Driver adapts a single CLI agent (Claude Code, OpenCode, Codex, ...) behind
// a common interface. Vendor-specific flags and output parsing live only here.
type Driver interface {
	Name() string

	// Send delivers a prompt to the project's session, creating it on first
	// call. An empty sessionID starts a new session; the returned Result
	// carries the (possibly new) session id to persist.
	Send(ctx context.Context, dir, sessionID, prompt string) Result

	// AttachArgs returns the argv for an interactive drop-in to the session,
	// to be exec'd with the terminal handed over to the child.
	AttachArgs(dir, sessionID string) []string
}

// StreamEventKind classifies what's happening in a streaming turn.
type StreamEventKind int

const (
	StreamStarted StreamEventKind = iota
	StreamText
	StreamToolCall
	StreamDone
	StreamError
)

// StreamEvent is one observable event during a streaming Send.
//
//   - Started: the underlying CLI has begun; Activity may carry "init" or
//     the first observable signal. SessionID is set if known.
//   - Text: the assistant produced a chunk of text (Content carries it).
//   - ToolCall: the assistant invoked a tool (Activity carries its name).
//   - Done: turn completed; Content carries the full final text.
//   - Error: turn failed; Err carries the error.
//
// SessionID is opportunistically populated; not every event carries one.
type StreamEvent struct {
	Kind      StreamEventKind
	Content   string
	Activity  string
	SessionID string
	Err       error
}

// StreamingDriver is an optional interface a Driver may implement when the
// underlying CLI supports real-time event streaming. The inbox checks for
// this and prefers StreamSend over Send when available, so the UI can show
// live "working (Bash)" or "working (typing)" status instead of a silent
// "working" indicator that only flips when the turn finishes.
//
// Implementations close the channel when the turn is done (or errored) and
// emit exactly one StreamDone or StreamError as the final event before close.
type StreamingDriver interface {
	Driver
	StreamSend(ctx context.Context, dir, sessionID, prompt string) <-chan StreamEvent
}
