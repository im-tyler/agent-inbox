package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// F09: an exit-zero Claude stream that produced assistant text but no
// validated terminal result is a failure, not a completed turn — a truncated
// or malformed result frame must not be converted into a successful answer a
// king watcher could act on.
func TestClaudeStreamMissingResultIsAnError(t *testing.T) {
	partial := `{"type":"system","subtype":"init","session_id":"sess-7"}
{"type":"assistant","message":{"content":[{"type":"text","text":"half a reply"}]}}`
	fakeBin(t, "claude", "cat <<'EOF'\n"+partial+"\nEOF\nexit 0\n")

	events := collect(t, Claude{}.StreamSend(context.Background(), t.TempDir(), "", "hi"))
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last.Kind != StreamError {
		t.Fatalf("last event = %+v, want Error — text without a result is not a completion", last)
	}
	if last.Content != "half a reply" {
		t.Errorf("partial text = %q, want it preserved on the error", last.Content)
	}
}

// F07: an oversized OpenCode frame used to stop the scanner with the pipe
// undrained, and Wait then blocked on a child still writing the rest — the
// turn wedged until the deadline. The bounded reader consumes the frame to
// its delimiter and the turn fails promptly with an explicit protocol error.
func TestOpenCodeStreamOversizedFrameFailsFast(t *testing.T) {
	fakeBin(t, "opencode", fmt.Sprintf(`python3 -c 'import sys
sys.stdout.write("{\"type\":\"tool_use\",\"sessionID\":\"ses_1\",\"part\":{\"tool\":\"bash\",\"state\":{\"title\":\"" + "x"*%d + "\"}}}" + chr(10))'
cat <<'EOF'
{"type":"text","sessionID":"ses_1","part":{"id":"p1","type":"text","text":"partial"}}
{"type":"step_finish","sessionID":"ses_1","part":{"reason":"stop"}}
EOF
exit 0
`, maxEventLine+1024))

	start := time.Now()
	events := collect(t, NewOpenCode("", false).StreamSend(context.Background(), t.TempDir(), "", "hi"))
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("turn took %s — the oversized frame wedged the read", d)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last.Kind != StreamError {
		t.Fatalf("last event = %+v, want Error — a skipped frame means the record is incomplete", last)
	}
	if !strings.Contains(last.Err.Error(), "skipped") {
		t.Errorf("error = %v, want it to name the skipped frame", last.Err)
	}
}

// F10: cancellation owns the process group, not just the direct child. The
// fake CLI leaves a grandchild holding stdout and a heartbeat; cancelling the
// turn must stop both and let the driver return, instead of leaving the
// reader blocked on the orphan's pipe.
func TestCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	beat := filepath.Join(dir, "beat.pid")
	fakeBin(t, "opencode", fmt.Sprintf(`sh -c 'echo $$ > %s; while :; do sleep 0.2; done' &
printf '%%s\n' '{"type":"step_start","sessionID":"ses_1","part":{}}'
sleep 30
`, beat))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := NewOpenCode("", false).StreamSend(ctx, dir, "", "hi")

	var first StreamEvent
	select {
	case first = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no first event")
	}
	if first.Kind != StreamStarted {
		t.Fatalf("first event = %+v, want Started", first)
	}

	// The grandchild registers itself before we cancel; cancelling into a
	// group whose member has not spawned yet proves nothing.
	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		if _, err := os.Stat(beat); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(beat); err != nil {
		t.Fatalf("grandchild never started: %v", err)
	}

	cancel()
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("driver still blocked after cancellation — the orphan holds the pipe")
	}

	// The grandchild must die with the group. TERM is immediate; KILL
	// escalates after killGrace, so allow for both.
	b, err := os.ReadFile(beat)
	if err != nil {
		t.Fatalf("grandchild never wrote its pid: %v", err)
	}
	pid := 0
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid)
	if pid <= 0 {
		t.Fatalf("bad pid %q", b)
	}
	deadline := time.Now().Add(killGrace + 5*time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone — the group died with the turn
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived cancellation", pid)
}
