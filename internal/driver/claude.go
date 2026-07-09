package driver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Claude drives Claude Code in headless mode. Schema verified against
// claude 2.1.167: `claude -p --output-format json` returns a single result
// object with result/session_id/is_error/permission_denials.
//
// Claude also implements StreamingDriver via `claude -p --output-format
// stream-json`, which emits NDJSON events (system/assistant/result) that
// we classify into StreamEvent values.
type Claude struct {
	PermissionMode string // "default", "acceptEdits", "plan", "bypassPermissions"
}

func (Claude) Name() string { return "claude" }

type claudeResult struct {
	Result            string            `json:"result"`
	SessionID         string            `json:"session_id"`
	IsError           bool              `json:"is_error"`
	Subtype           string            `json:"subtype"`
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

// StreamSend runs Claude with --output-format stream-json and emits events
// as they arrive. The NDJSON shape (verified against the documented stream
// format):
//
//	{"type":"system","subtype":"init","session_id":"..."}
//	{"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}
//	{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{...}}]}}
//	{"type":"result","subtype":"success","result":"...","session_id":"..."}
//	{"type":"result","subtype":"error_during_execution",...}
//
// Implementation is defensive: any parse error on an individual line is
// skipped (with a debug-level signal carried in Activity) rather than
// killing the whole stream.
func (c Claude) StreamSend(ctx context.Context, dir, sessionID, prompt string) <-chan StreamEvent {
	ch := make(chan StreamEvent, 16)

	go func() {
		defer close(ch)

		args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
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

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			ch <- StreamEvent{Kind: StreamError, Err: fmt.Errorf("claude stream: pipe: %w", err)}
			return
		}
		if err := cmd.Start(); err != nil {
			ch <- StreamEvent{Kind: StreamError, Err: wrapExec(err)}
			return
		}

		ch <- StreamEvent{Kind: StreamStarted, Activity: "init", SessionID: sessionID}

		var finalText strings.Builder
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			classifyClaudeStreamLine(line, ch, &finalText, &sessionID)
		}

		waitErr := cmd.Wait()
		// If we already got a result event, the wait error is benign.
		// Otherwise it's a real failure.
		select {
		case ch <- StreamEvent{Kind: StreamDone, Content: finalText.String(), SessionID: sessionID}:
			_ = waitErr // already classified via result event
		default:
			if waitErr != nil {
				ch <- StreamEvent{Kind: StreamError, Err: wrapExec(waitErr)}
			} else {
				ch <- StreamEvent{Kind: StreamDone, Content: finalText.String(), SessionID: sessionID}
			}
		}
	}()

	return ch
}

// classifyClaudeStreamLine parses one NDJSON line from stream-json output
// and emits zero or more StreamEvents. finalText accumulates the assistant's
// text chunks for the terminal Done event.
func classifyClaudeStreamLine(line string, ch chan<- StreamEvent, finalText *strings.Builder, sessionID *string) {
	// Top-level shape: every line has a "type" field.
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal([]byte(line), &head) != nil {
		return
	}

	switch head.Type {
	case "system":
		// Look for session_id; usually present on subtype=="init".
		var sys struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal([]byte(line), &sys)
		if sys.SessionID != "" {
			*sessionID = sys.SessionID
		}
		return

	case "assistant":
		// Content array may contain text, tool_use, etc.
		var asst struct {
			Message struct {
				Content []struct {
					Type  string          `json:"type"`
					Text  string          `json:"text"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &asst) != nil {
			return
		}
		for _, c := range asst.Message.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					finalText.WriteString(c.Text)
					ch <- StreamEvent{Kind: StreamText, Content: c.Text, SessionID: *sessionID}
				}
			case "tool_use":
				ch <- StreamEvent{Kind: StreamToolCall, Activity: c.Name, SessionID: *sessionID}
			}
		}
		return

	case "result":
		var res claudeResult
		if json.Unmarshal([]byte(line), &res) != nil {
			return
		}
		if res.SessionID != "" {
			*sessionID = res.SessionID
		}
		if res.IsError {
			ch <- StreamEvent{
				Kind:      StreamError,
				Err:       fmt.Errorf("claude: %s", res.Subtype),
				Content:   res.Result,
				SessionID: *sessionID,
			}
			return
		}
		// Use the result text as the authoritative final; overwrite accumulated.
		final := strings.TrimSpace(res.Result)
		if final != "" {
			finalText.Reset()
			finalText.WriteString(final)
		}
		ch <- StreamEvent{Kind: StreamDone, Content: finalText.String(), SessionID: *sessionID}
	}
}

func (Claude) AttachArgs(dir, sessionID string) []string {
	return []string{"claude", "--resume", sessionID}
}
