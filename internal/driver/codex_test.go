package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin puts an executable script on PATH under the given name, so a driver
// test exercises the real argv-building and real output parsing instead of a
// stub standing in for both.
func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The JSONL codex-cli 0.146.0 actually emits, recorded from a real turn.
const codexTurnJSONL = `{"type":"thread.started","thread_id":"019fbff9-843c-7a72-ac28-90e4a1b88b42"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Running the requested command."}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc 'echo hello'","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":28070}}`

// fakeCodex writes the final message where the driver asked for it and prints
// the recorded event stream.
func fakeCodex(t *testing.T, final, jsonl string) {
	t.Helper()
	fakeBin(t, "codex", `
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out="$2"; fi
  shift
done
[ -n "$out" ] && printf '%s' `+shellQuote(final)+` > "$out"
cat <<'EOF'
`+jsonl+`
EOF
`)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// The conversation id lives on thread.started as thread_id. An earlier driver
// looked for session_id, never found it, and so silently started every codex
// turn in a fresh session — this is the regression that must not come back.
func TestCodexCapturesThreadID(t *testing.T) {
	fakeCodex(t, "done", codexTurnJSONL)
	res := Codex{}.Send(context.Background(), t.TempDir(), "", "hi")
	if res.Err != nil {
		t.Fatalf("Send: %v", res.Err)
	}
	if res.SessionID != "019fbff9-843c-7a72-ac28-90e4a1b88b42" {
		t.Errorf("session id = %q, want the thread_id from the stream", res.SessionID)
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want the last-message file's contents", res.Final)
	}
	if res.Status != StatusWaiting {
		t.Errorf("status = %q, want waiting", res.Status)
	}
}

// A resumed turn passes the id through `codex exec resume <id>`, and keeps it
// even though the stream repeats it.
func TestCodexResumeArgs(t *testing.T) {
	args := Codex{Sandbox: "read-only"}.args("thread-7", "/tmp/last")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "resume thread-7") {
		t.Errorf("args do not resume the session: %v", args)
	}
	if !strings.Contains(joined, "--output-last-message /tmp/last") {
		t.Errorf("args do not request the last message: %v", args)
	}
	if strings.Index(joined, "resume") < strings.Index(joined, "--json") {
		t.Errorf("subcommand must follow the options: %v", args)
	}
}

func TestCodexStreamsActivityAndText(t *testing.T) {
	fakeCodex(t, "done", codexTurnJSONL)

	var kinds []StreamEventKind
	var activities, text []string
	var done string
	for ev := range (Codex{}).StreamSend(context.Background(), t.TempDir(), "", "hi") {
		kinds = append(kinds, ev.Kind)
		switch ev.Kind {
		case StreamToolCall:
			activities = append(activities, ev.Activity)
		case StreamText:
			text = append(text, ev.Content)
		case StreamDone:
			done = ev.Content
		case StreamError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}

	if got := strings.Join(activities, ","); got != "Bash" {
		t.Errorf("activities = %q, want Bash", got)
	}
	if got := strings.Join(text, "|"); got != "Running the requested command.|done" {
		t.Errorf("text events = %q", got)
	}
	if done != "done" {
		t.Errorf("done content = %q", done)
	}
	// Exactly one terminal event, and it is last.
	if n := countTerminal(kinds); n != 1 {
		t.Errorf("%d terminal events, want 1: %v", n, kinds)
	}
	if last := kinds[len(kinds)-1]; last != StreamDone {
		t.Errorf("last event = %v, want Done", last)
	}
}

// A turn whose CLI exits non-zero is a failure, not an empty success.
func TestCodexNonZeroExitIsAnError(t *testing.T) {
	fakeBin(t, "codex", "echo 'boom' >&2\nexit 3\n")
	res := Codex{}.Send(context.Background(), t.TempDir(), "", "hi")
	if res.Err == nil {
		t.Fatal("a failed codex run reported success")
	}
	if !strings.Contains(res.Err.Error(), "boom") {
		t.Errorf("stderr not surfaced: %v", res.Err)
	}
}

// A run that exits clean but writes no last message has produced no answer.
// Filing that as a reply would show the user a blank turn from their agent.
func TestCodexEmptyFinalIsAnError(t *testing.T) {
	fakeCodex(t, "", `{"type":"thread.started","thread_id":"t1"}`)
	res := Codex{}.Send(context.Background(), t.TempDir(), "", "hi")
	if res.Err == nil {
		t.Fatal("an empty turn reported success")
	}
	if res.SessionID != "t1" {
		t.Errorf("session id lost on the error path: %q", res.SessionID)
	}
}

func TestCodexActivityLabels(t *testing.T) {
	for in, want := range map[string]string{
		"command_execution": "Bash",
		"file_change":       "Edit",
		"agent_message":     "typing",
		"":                  "working",
		"future_thing":      "future_thing",
	} {
		if got := codexActivity(in); got != want {
			t.Errorf("codexActivity(%q) = %q, want %q", in, got, want)
		}
	}
}

func countTerminal(kinds []StreamEventKind) int {
	n := 0
	for _, k := range kinds {
		if k == StreamDone || k == StreamError {
			n++
		}
	}
	return n
}
