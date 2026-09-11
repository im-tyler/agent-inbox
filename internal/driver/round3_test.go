package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F04: an oversized frame before the result must not publish a progress
// event after the terminal one — that flipped a completed project back to
// working and blocked its next send.
func TestAuditClaudeOversizedFrameKeepsTerminalLast(t *testing.T) {
	binDir := t.TempDir()
	script := `#!/bin/sh
printf '%s' '{"type":"user","message":{"content":"'
head -c 16777217 /dev/zero | tr '\000' x
printf '%s\n' '"}}'
printf '%s\n' '{"type":"result","subtype":"success","result":"ok","session_id":"fixture"}'
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	terminal := false
	last := StreamEvent{}
	for ev := range (Claude{}).StreamSend(ctx, t.TempDir(), "fixture", "test") {
		if terminal {
			t.Errorf("event after terminal: %+v", ev)
		}
		if ev.Kind == StreamDone || ev.Kind == StreamError {
			terminal = true
		}
		last = ev
	}
	if !terminal || last.Kind != StreamDone || last.Content != "ok" {
		t.Fatalf("unexpected final event: %+v", last)
	}
}
