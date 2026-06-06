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

// Driver adapts a single CLI agent (Claude Code, OpenCode, ...) behind a
// common interface. Vendor-specific flags and output parsing live only here.
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
