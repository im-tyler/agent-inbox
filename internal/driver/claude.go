package driver

import (
	"context"
	"encoding/json"
	"fmt"
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

// sessionArgs is how a turn addresses its conversation: a fresh session with
// an id we choose, or a resume of one that exists. Shared by both the
// blocking and streaming paths so they cannot drift.
func (c Claude) sessionArgs(sessionID string) (args []string, id string) {
	if sessionID == "" {
		id = newUUID()
		return []string{"--session-id", id}, id
	}
	return []string{"--resume", sessionID}, sessionID
}

// SendForked starts a NEW session seeded with sourceSessionID's history,
// leaving that session untouched. This is how an adopted Claude Code session
// is picked up: the source is a live process, and resuming it in place would
// put two writers on one transcript. Verified against claude 2.1.220 — the
// fork answers from the original's context and reports a new session id,
// which the caller persists and resumes normally from then on.
func (c Claude) SendForked(ctx context.Context, dir, sourceSessionID, prompt string) Result {
	if sourceSessionID == "" {
		return c.Send(ctx, dir, "", prompt)
	}
	// The fallback id is empty, not the source. Passing the source as the
	// fallback meant a fork that failed — or that succeeded without reporting
	// a new id — returned the *borrowed* session as this project's own. The
	// project then held an id belonging to somebody else's live agent, and
	// resuming it would have put two writers on that transcript.
	res := c.send(ctx, dir, "", prompt, []string{"--resume", sourceSessionID, "--fork-session"})
	if res.Err != nil {
		res.SessionID = ""
		return res
	}
	// A fork that reports no id, or reports the source's, has not given this
	// project a session of its own. Treat that as a failure so ForkFrom is
	// kept and the next attempt tries again, rather than silently adopting a
	// session we do not own.
	if res.SessionID == "" || res.SessionID == sourceSessionID {
		return Result{Status: StatusError,
			Err: fmt.Errorf("claude: fork of %s did not report a new session id", sourceSessionID)}
	}
	return res
}

func (c Claude) Send(ctx context.Context, dir, sessionID, prompt string) Result {
	sessArgs, id := c.sessionArgs(sessionID)
	return c.send(ctx, dir, id, prompt, sessArgs)
}

// send runs one blocking turn. sessionID is what to report back if the CLI
// does not name one itself.
func (c Claude) send(ctx context.Context, dir, sessionID, prompt string, sessArgs []string) Result {
	args := append([]string{"-p", prompt, "--output-format", "json"}, sessArgs...)
	if c.PermissionMode != "" {
		args = append(args, "--permission-mode", c.PermissionMode)
	}

	cmd := startProcess(ctx, "claude", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return Result{SessionID: sessionID, Status: StatusError, Err: turnError(ctx, "claude", err, "")}
		}
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

		var sessArgs []string
		sessArgs, sessionID = c.sessionArgs(sessionID)
		args := append([]string{"-p", prompt, "--output-format", "stream-json", "--verbose"}, sessArgs...)
		if c.PermissionMode != "" {
			args = append(args, "--permission-mode", c.PermissionMode)
		}

		cmd := startProcess(ctx, "claude", args...)
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
		sawTerminal := false
		reportedSkipped := 0
		// jsonlReader rather than bufio.Scanner: a Scanner stops on a line
		// longer than its buffer, and the Wait below would then block forever
		// against a child still trying to write it. See jsonl.go.
		jr := newJSONLReader(stdout, maxJSONLine)
		for {
			raw, err := jr.Next()

			// Diagnostics publish before the next event, never after a
			// terminal one: a progress event emitted after StreamDone made
			// the inbox set a completed project back to working, and its
			// next send was refused.
			if !sawTerminal && jr.Skipped > reportedSkipped {
				ch <- StreamEvent{Kind: StreamToolCall, SessionID: sessionID,
					Activity: fmt.Sprintf("skipped %d oversized event(s)", jr.Skipped)}
				reportedSkipped = jr.Skipped
			}
			if err != nil {
				break
			}
			if sawTerminal {
				continue // drain remaining stdout; publish nothing further
			}
			line := strings.TrimSpace(string(raw))
			if line == "" {
				continue
			}
			if classifyClaudeStreamLine(line, ch, &finalText, &sessionID) {
				sawTerminal = true
			}
		}

		waitErr := cmd.Wait()

		// A result event is the turn's own account of how it went, and it
		// has already been emitted. Anything after it would be a second
		// terminal event on a channel that promises exactly one.
		if sawTerminal {
			return
		}

		// No result event means the CLI died mid-turn — a crash, or the
		// context's deadline killing it. Either way this is a failure, and
		// reporting it as a completed turn (which an earlier version did,
		// because it tested the channel buffer instead of what had been
		// sent) files a truncated answer as though the agent had finished.
		//
		// The same goes for an exit-zero stream that produced assistant text
		// but no validated terminal result: intermediate text is not a
		// completion, and a truncated or malformed terminal frame must not
		// be converted into a successful answer a king watcher could act on.
		if waitErr != nil {
			ch <- StreamEvent{Kind: StreamError, Content: finalText.String(), SessionID: sessionID,
				Err: turnError(ctx, "claude", waitErr, "")}
			return
		}
		ch <- StreamEvent{Kind: StreamError, Content: finalText.String(), SessionID: sessionID,
			Err: fmt.Errorf("claude: stream ended without a result event")}
	}()

	return ch
}

// classifyClaudeStreamLine parses one NDJSON line from stream-json output
// and emits zero or more StreamEvents. finalText accumulates the assistant's
// text chunks for the terminal Done event. Reports whether this line produced
// a terminal event, which is what tells the caller the turn accounted for
// itself and needs no epilogue.
func classifyClaudeStreamLine(line string, ch chan<- StreamEvent, finalText *strings.Builder, sessionID *string) bool {
	// Top-level shape: every line has a "type" field.
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal([]byte(line), &head) != nil {
		return false
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
		return false

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
			return false
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
		return false

	case "result":
		var res claudeResult
		if json.Unmarshal([]byte(line), &res) != nil {
			return false
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
			return true
		}
		// Use the result text as the authoritative final; overwrite accumulated.
		final := strings.TrimSpace(res.Result)
		if final != "" {
			finalText.Reset()
			finalText.WriteString(final)
		}
		if n := len(res.PermissionDenials); n > 0 {
			finalText.WriteString(fmt.Sprintf("\n\n(blocked on %d permission request(s) this turn)", n))
		}
		ch <- StreamEvent{Kind: StreamDone, Content: finalText.String(), SessionID: *sessionID}
		return true
	}
	return false
}

func (Claude) AttachArgs(dir, sessionID string) []string {
	return []string{"claude", "--resume", sessionID}
}
