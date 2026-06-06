package driver

import (
	"context"
	"fmt"
	"time"
)

// Mock simulates an agent so the inbox UX can be driven without spending API
// calls. It is the default driver until real adapters are wired per project.
type Mock struct{}

func (Mock) Name() string { return "mock" }

func (Mock) Send(ctx context.Context, dir, sessionID, prompt string) Result {
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return Result{SessionID: sessionID, Status: StatusError, Err: ctx.Err()}
	}
	if sessionID == "" {
		sessionID = newUUID()
	}
	return Result{
		SessionID: sessionID,
		Final:     fmt.Sprintf("[mock] handled %q. Recommend: run tests, then commit. What next?", prompt),
		Status:    StatusWaiting,
	}
}

func (Mock) AttachArgs(dir, sessionID string) []string {
	return []string{"echo", "mock attach to", sessionID}
}
