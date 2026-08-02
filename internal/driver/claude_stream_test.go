package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeStreamJSONL = `{"type":"system","subtype":"init","session_id":"sess-9"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking now."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}
{"type":"result","subtype":"success","result":"all clear","session_id":"sess-9"}`

func collect(t *testing.T, ch <-chan StreamEvent) []StreamEvent {
	t.Helper()
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// The contract StreamingDriver states: exactly one Done or Error, and it is
// last. An earlier version tested the channel's buffer instead of what it had
// sent, so every successful turn emitted a second Done after the result event.
func TestClaudeStreamEmitsOneTerminalEvent(t *testing.T) {
	fakeBin(t, "claude", "cat <<'EOF'\n"+claudeStreamJSONL+"\nEOF\n")

	events := collect(t, Claude{}.StreamSend(context.Background(), t.TempDir(), "", "hi"))
	var kinds []StreamEventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	if n := countTerminal(kinds); n != 1 {
		t.Fatalf("%d terminal events, want 1: %v", n, kinds)
	}
	last := events[len(events)-1]
	if last.Kind != StreamDone || last.Content != "all clear" {
		t.Errorf("last event = %+v, want Done with the result text", last)
	}
	if last.SessionID != "sess-9" {
		t.Errorf("session id = %q, want sess-9", last.SessionID)
	}
}

// A CLI that dies mid-turn has not answered. Reporting the partial text as a
// completed turn — which the old buffer-probing epilogue did — files a
// truncated answer as though the agent had finished saying it.
func TestClaudeStreamDeathIsAnError(t *testing.T) {
	partial := `{"type":"system","subtype":"init","session_id":"sess-4"}
{"type":"assistant","message":{"content":[{"type":"text","text":"I was halfway thr"}]}}`
	fakeBin(t, "claude", "cat <<'EOF'\n"+partial+"\nEOF\nexit 1\n")

	events := collect(t, Claude{}.StreamSend(context.Background(), t.TempDir(), "", "hi"))
	last := events[len(events)-1]
	if last.Kind != StreamError {
		t.Fatalf("last event = %+v, want Error", last)
	}
	// The partial text rides along so the inbox can keep what was said.
	if !strings.Contains(last.Content, "halfway") {
		t.Errorf("partial text lost: %q", last.Content)
	}
}

// A turn killed by its deadline reports as "signal: killed", which reads as a
// crash. The context is the only thing that knows better.
func TestClaudeStreamTimeoutSaysSo(t *testing.T) {
	fakeBin(t, "claude", "sleep 5\n")
	ctx, cancel := context.WithCancel(context.Background())
	ch := Claude{}.StreamSend(ctx, t.TempDir(), "", "hi")
	// Started arrives before the CLI produces anything; cancel from there.
	<-ch
	cancel()

	var last StreamEvent
	for ev := range ch {
		last = ev
	}
	if last.Kind != StreamError {
		t.Fatalf("last event = %+v, want Error", last)
	}
	if !strings.Contains(last.Err.Error(), "cancelled") {
		t.Errorf("error does not explain the cancellation: %v", last.Err)
	}
}

// A fork addresses the source session and asks for a new id, rather than
// resuming in place — the source is a live process and its transcript is not
// ours to write to.
func TestClaudeForkArgs(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv")
	fakeBin(t, "claude", `printf '%s' "$*" > `+shellQuote(argvPath)+`
echo '{"result":"ok","session_id":"new-1"}'`)

	res := Claude{}.SendForked(context.Background(), t.TempDir(), "live-7", "status?")
	if res.Err != nil {
		t.Fatalf("SendForked: %v", res.Err)
	}
	if res.SessionID != "new-1" {
		t.Errorf("session id = %q, want the forked session's own id", res.SessionID)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("fake claude never ran: %v", err)
	}
	got := string(argv)
	if !strings.Contains(got, "--resume live-7") {
		t.Errorf("fork does not address the source session: %s", got)
	}
	if !strings.Contains(got, "--fork-session") {
		t.Errorf("fork resumes in place instead of forking: %s", got)
	}
	if strings.Contains(got, "--session-id") {
		t.Errorf("fork must let the CLI name the new session: %s", got)
	}
}

func TestClaudeSessionArgs(t *testing.T) {
	args, id := Claude{}.sessionArgs("")
	if len(args) != 2 || args[0] != "--session-id" || args[1] != id || id == "" {
		t.Errorf("new session args = %v, id = %q", args, id)
	}
	args, id = Claude{}.sessionArgs("existing")
	if len(args) != 2 || args[0] != "--resume" || args[1] != "existing" || id != "existing" {
		t.Errorf("resume args = %v, id = %q", args, id)
	}
}
