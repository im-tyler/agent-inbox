package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Streaming OpenCode.
//
// `opencode run --format json` emits NDJSON as the turn happens. Verified
// against opencode 1.18.18:
//
//	{"type":"step_start","sessionID":"ses_…","part":{…}}
//	{"type":"tool_use","sessionID":"ses_…","part":{"tool":"bash","state":{…}}}
//	{"type":"text","sessionID":"ses_…","part":{"id":"prt_…","text":"…"}}
//	{"type":"step_finish","sessionID":"ses_…","part":{"reason":"stop","tokens":{…}}}
//
// This matters for more than a spinner. Every event carries the session id, and
// that is the thing the blocking path could not get: `run` cannot be told which
// session to create, so the id had to be recovered by diffing `session list`
// around the call, serialised behind a mutex so only one project could start a
// session at a time, with a prompt-correlation fallback for when two appeared
// at once. None of that is needed when the session announces itself.
//
// The reply is read from the stream too, so the export-and-retry dance — and
// the terminal scraping behind it — does not run on this path either.

// ocEvent is one NDJSON line. Only the fields this driver acts on are named;
// opencode sends a good deal more, and decoding what we do not use would make
// every added field upstream a parse decision here.
type ocEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Text   string `json:"text"`
		Tool   string `json:"tool"`
		Reason string `json:"reason"`
		State  struct {
			Status string `json:"status"`
			Title  string `json:"title"`
		} `json:"state"`
	} `json:"part"`
}

// maxEventLine bounds one NDJSON record. A tool_use event carries the tool's
// entire output inline, and a build log arrives as a single line.
const maxEventLine = 8 << 20

// StreamSend runs one turn and reports it as it happens.
func (o *OpenCode) StreamSend(ctx context.Context, dir, sessionID, prompt string) <-chan StreamEvent {
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		o.stream(ctx, ch, dir, sessionID, prompt)
	}()
	return ch
}

func (o *OpenCode) stream(ctx context.Context, ch chan<- StreamEvent, dir, sessionID, prompt string) {
	args := []string{"run", "--format", "json", "--model", o.Model}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	if o.SkipPermissions {
		args = append(args, "--auto")
	}
	args = append(args, prompt)

	cmd := startProcess(ctx, "opencode", args...)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: err}
		return
	}
	// stderr stays separate. Merged, opencode's diagnostics landed inside the
	// reply — and stderr belongs in an error message, not in what the agent
	// said.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Err: wrapExec(err)}
		return
	}

	ch <- StreamEvent{Kind: StreamStarted, SessionID: sessionID, Activity: "starting"}
	acc := newTextAccumulator()
	sawStop := false

	// jsonlReader rather than bufio.Scanner: a Scanner stops dead on a line
	// longer than its buffer, and the Wait below would then block against a
	// child still writing the rest of that line — an oversized frame wedged
	// the whole turn until the deadline. See jsonl.go and F07.
	jr := newJSONLReader(stdout, maxEventLine)
	var readErr error
	for {
		var raw []byte
		raw, readErr = jr.Next()
		if readErr != nil {
			break
		}
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev ocEvent
		if json.Unmarshal(line, &ev) != nil {
			// One unreadable event should not cost the turn. The stream is
			// advisory; the terminal state comes from the process exiting.
			continue
		}
		if ev.SessionID != "" && ev.SessionID != sessionID {
			sessionID = ev.SessionID
		}
		switch ev.Type {
		case "step_start":
			ch <- StreamEvent{Kind: StreamStarted, SessionID: sessionID, Activity: "thinking"}
		case "tool_use":
			activity := ev.Part.Tool
			if activity == "" {
				activity = "tool"
			}
			ch <- StreamEvent{Kind: StreamToolCall, SessionID: sessionID, Activity: activity}
		case "text":
			if delta := acc.add(ev.Part.ID, ev.Part.Text); delta != "" {
				ch <- StreamEvent{Kind: StreamText, SessionID: sessionID, Content: delta}
			}
		case "step_finish":
			// "stop" is the model finishing; "tool-calls" is a step ending so
			// another can begin. Treating either as the end of the turn would
			// cut every tool-using turn off at its first tool call.
			if ev.Part.Reason == "stop" {
				sawStop = true
			}
		}
	}
	if readErr == io.EOF {
		readErr = nil
	}
	waitErr := cmd.Wait()

	final := strings.TrimSpace(cleanReply(acc.text()))
	switch {
	case waitErr != nil:
		if ctx.Err() != nil {
			ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Content: final,
				Err: turnError(ctx, "opencode", waitErr, "")}
			return
		}
		ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Content: final,
			Err: fmt.Errorf("opencode run: %v%s", waitErr, diagSuffix(strings.TrimSpace(stderr.String())))}
	case jr.Skipped > 0:
		// An oversized frame is a protocol violation: something in the turn's
		// record was lost, and a reply assembled from the frames that
		// happened to fit is not the agent's answer. Claiming success here
		// would file a hole as a completion.
		ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Content: final,
			Err: fmt.Errorf("opencode: %d event(s) exceeded %dMiB and were skipped; the turn's record is incomplete", jr.Skipped, maxEventLine>>20)}
	case readErr != nil && !isClosedPipe(readErr):
		ch <- StreamEvent{Kind: StreamError, SessionID: sessionID, Content: final,
			Err: fmt.Errorf("reading opencode events: %w", readErr)}
	case sessionID == "":
		// Without an id this project cannot resume, and the next turn would
		// silently start a new conversation. Better to fail the turn than to
		// lose the thread and not say so.
		ch <- StreamEvent{Kind: StreamError, Content: final,
			Err: fmt.Errorf("opencode: the run reported no session id")}
	case !sawStop && final == "":
		// The process exited cleanly having said nothing and never reported a
		// finished step. Reporting that as an empty successful turn files
		// silence as the agent's answer.
		ch <- StreamEvent{Kind: StreamError, SessionID: sessionID,
			Err: fmt.Errorf("opencode: the run produced no reply%s", diagSuffix(strings.TrimSpace(stderr.String())))}
	default:
		ch <- StreamEvent{Kind: StreamDone, SessionID: sessionID, Content: final}
	}
}

// textAccumulator reassembles the reply from text events.
//
// A text part is re-sent as it grows, carrying the whole text each time rather
// than the new characters, so emitting what arrives would repeat everything
// said so far on every event. Parts are kept in arrival order because the final
// reply is their concatenation.
type textAccumulator struct {
	order []string
	seen  map[string]string
}

func newTextAccumulator() *textAccumulator {
	return &textAccumulator{seen: map[string]string{}}
}

// add records a part's latest text and returns what is new about it.
func (a *textAccumulator) add(id, text string) string {
	if id == "" {
		// No identity to deduplicate against. Appending is the only safe
		// reading, and dropping it would lose the reply outright.
		id = fmt.Sprintf("anon-%d", len(a.order))
	}
	prev, ok := a.seen[id]
	if !ok {
		a.order = append(a.order, id)
	}
	a.seen[id] = text
	if strings.HasPrefix(text, prev) {
		return text[len(prev):]
	}
	// A part that was rewritten rather than extended. Rare, but emitting a
	// bogus delta would corrupt what the user is watching, so re-send it whole.
	return text
}

func (a *textAccumulator) text() string {
	var b strings.Builder
	for _, id := range a.order {
		b.WriteString(a.seen[id])
	}
	return b.String()
}

// isClosedPipe reports the ordinary end-of-stream that follows killing the
// child, which is not a read failure worth reporting as one.
func isClosedPipe(err error) bool {
	return err == io.ErrClosedPipe || strings.Contains(err.Error(), "file already closed")
}
