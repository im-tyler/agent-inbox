package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Codex drives OpenAI's Codex CLI in headless mode (`codex exec`).
//
// Event schema verified against codex-cli 0.146.0 by running a real turn:
//
//	{"type":"thread.started","thread_id":"019fbff9-..."}
//	{"type":"turn.started"}
//	{"type":"item.started","item":{"id":"item_1","type":"command_execution",...}}
//	{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"..."}}
//	{"type":"turn.completed","usage":{...}}
//
// The conversation id is `thread_id` on thread.started. There is no
// `session_id` field anywhere in the stream — an earlier version of this
// driver looked for one, never found it, and so never persisted an id, which
// silently started every codex turn in a fresh session. `codex exec resume
// <thread_id>` is verified to restore the conversation.
type Codex struct {
	Model         string // optional model override (e.g. "gpt-5", "o3")
	Sandbox       string // "read-only" | "workspace-write" | "danger-full-access"
	SkipApprovals bool   // maps to --dangerously-bypass-approvals-and-sandbox
}

func (Codex) Name() string { return "codex" }

// codexEvent is a permissive shape for one JSONL event. Everything is
// optional: the stream is upstream's to change, and an event we cannot read
// should cost us that event, not the turn.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	// SessionID is not emitted by 0.146.0, but costs nothing to accept in
	// case another build names the same thing the older way.
	SessionID string `json:"session_id"`
	Item      struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// id returns whichever conversation id this event carries, if any.
func (e codexEvent) id() string {
	if e.ThreadID != "" {
		return e.ThreadID
	}
	return e.SessionID
}

// codexActivity turns an item type into the label shown beside the spinner.
// An unknown type falls through as itself rather than being dropped: a new
// item type upstream should read as an odd word, not as a stalled agent.
func codexActivity(itemType string) string {
	switch itemType {
	case "command_execution":
		return "Bash"
	case "file_change", "patch_apply":
		return "Edit"
	case "mcp_tool_call":
		return "MCP"
	case "web_search":
		return "Search"
	case "reasoning":
		return "thinking"
	case "todo_list":
		return "planning"
	case "agent_message":
		return "typing"
	case "":
		return "working"
	}
	return itemType
}

func (c Codex) args(sessionID, tmpPath string) []string {
	args := []string{"exec", "--json", "--output-last-message", tmpPath}
	if c.Model != "" {
		args = append(args, "-m", c.Model)
	}
	switch {
	case c.SkipApprovals:
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	case c.Sandbox != "":
		args = append(args, "-s", c.Sandbox)
	}
	if sessionID != "" {
		args = append(args, "resume", sessionID)
	}
	return args
}

// Send drains StreamSend. Both paths were parsing the same JSONL and only one
// of them was ever right; there is one parser now, and the blocking call is a
// thin collector over it.
func (c Codex) Send(ctx context.Context, dir, sessionID, prompt string) Result {
	res := Result{SessionID: sessionID, Status: StatusError, Err: fmt.Errorf("codex: no completion event")}
	for ev := range c.StreamSend(ctx, dir, sessionID, prompt) {
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Kind {
		case StreamDone:
			res.Final, res.Status, res.Err = ev.Content, StatusWaiting, nil
		case StreamError:
			res.Status, res.Err = StatusError, ev.Err
		}
	}
	return res
}

// StreamSend runs `codex exec --json` and classifies its JSONL into stream
// events. The final text still comes from --output-last-message rather than
// from the last agent_message: that file is what the CLI guarantees, and
// rebuilding it from events would be a second answer to a settled question.
func (c Codex) StreamSend(ctx context.Context, dir, sessionID, prompt string) <-chan StreamEvent {
	ch := make(chan StreamEvent, 16)

	go func() {
		defer close(ch)

		tmp, err := os.CreateTemp("", "agent-inbox-codex-*")
		if err != nil {
			ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: fmt.Errorf("codex: tempfile: %w", err)}
			return
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)

		cmd := exec.CommandContext(ctx, "codex", append(c.args(sessionID, tmpPath), prompt)...)
		cmd.Dir = dir

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: fmt.Errorf("codex: pipe: %w", err)}
			return
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Start(); err != nil {
			ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: wrapExec(err)}
			return
		}
		ch <- StreamEvent{Kind: StreamStarted, Activity: "starting", SessionID: sessionID}

		var failure string
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 || line[0] != '{' {
				continue
			}
			var ev codexEvent
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			if id := ev.id(); id != "" {
				sessionID = id
			}
			switch ev.Type {
			case "thread.started":
				ch <- StreamEvent{Kind: StreamStarted, Activity: "init", SessionID: sessionID}
			case "item.started":
				ch <- StreamEvent{Kind: StreamToolCall, Activity: codexActivity(ev.Item.Type), SessionID: sessionID}
			case "item.completed":
				// agent_message arrives whole rather than as deltas, so one
				// event is a paragraph of the reply, not a token of it.
				if ev.Item.Type == "agent_message" && ev.Item.Text != "" {
					ch <- StreamEvent{Kind: StreamText, Content: ev.Item.Text, SessionID: sessionID}
				}
			case "turn.failed", "error":
				if failure == "" {
					failure = ev.Error.Message
				}
			}
		}

		if waitErr := cmd.Wait(); waitErr != nil {
			ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: turnError(ctx, "codex", waitErr, stderr.String())}
			return
		}
		if failure != "" {
			ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: fmt.Errorf("codex: %s", failure)}
			return
		}

		final, readErr := os.ReadFile(tmpPath)
		if readErr != nil {
			ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: fmt.Errorf("codex: read last-message file: %w", readErr)}
			return
		}
		if text := strings.TrimSpace(string(final)); text != "" {
			ch <- StreamEvent{Kind: StreamDone, Content: text, SessionID: sessionID}
			return
		}
		ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: fmt.Errorf("codex: turn completed but no last message written")}
	}()

	return ch
}

// AttachArgs returns the argv for interactive resume. Codex's interactive
// resume uses the bare `codex resume` subcommand (not `codex exec resume`).
func (Codex) AttachArgs(_, sessionID string) []string {
	return []string{"codex", "resume", sessionID}
}

// turnError explains why a CLI turn ended. A cancelled or timed-out turn
// reports as "signal: killed", which reads as a crash; the context knows
// better and is the only thing that does.
func turnError(ctx context.Context, tool string, waitErr error, stderr string) error {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return fmt.Errorf("%s: turn exceeded the time limit", tool)
	case context.Canceled:
		return fmt.Errorf("%s: cancelled", tool)
	}
	return fmt.Errorf("%s: %v%s", tool, waitErr, diagSuffix(strings.TrimSpace(stderr)))
}

// diagSuffix formats a stderr snippet for inclusion in an error message.
// Truncated to keep errors readable in the TUI toast line.
func diagSuffix(s string) string {
	if s == "" {
		return ""
	}
	const max = 240
	if len(s) > max {
		s = s[:max] + "…"
	}
	return "\n" + s
}
