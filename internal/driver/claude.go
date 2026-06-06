package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Claude drives Claude Code in headless mode. Schema verified against
// claude 2.1.167: `claude -p --output-format json` returns a single result
// object with result/session_id/is_error/permission_denials.
type Claude struct {
	PermissionMode string // "default", "acceptEdits", "plan", "bypassPermissions"
}

func (Claude) Name() string { return "claude" }

type claudeResult struct {
	Result           string            `json:"result"`
	SessionID        string            `json:"session_id"`
	IsError          bool              `json:"is_error"`
	Subtype          string            `json:"subtype"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
}

func (c Claude) Send(ctx context.Context, dir, sessionID, prompt string) Result {
	args := []string{"-p", prompt, "--output-format", "json"}
	if sessionID == "" {
		sessionID = newUUID()
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}
	if c.PermissionMode != "" {
		args = append(args, "--permission-mode", c.PermissionMode)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return Result{SessionID: sessionID, Status: StatusError, Err: wrapExec(err)}
	}

	var r claudeResult
	if err := json.Unmarshal(out, &r); err != nil {
		return Result{SessionID: sessionID, Status: StatusError, Err: fmt.Errorf("parse claude json: %w", err)}
	}
	if r.SessionID != "" {
		sessionID = r.SessionID
	}
	if r.IsError {
		return Result{SessionID: sessionID, Status: StatusError, Final: r.Result, Err: fmt.Errorf("claude error: %s", r.Subtype)}
	}

	final := strings.TrimSpace(r.Result)
	if n := len(r.PermissionDenials); n > 0 {
		final = fmt.Sprintf("%s\n\n(blocked on %d permission request(s) this turn)", final, n)
	}
	return Result{SessionID: sessionID, Final: final, Status: StatusWaiting}
}

func (Claude) AttachArgs(dir, sessionID string) []string {
	return []string{"claude", "--resume", sessionID}
}
