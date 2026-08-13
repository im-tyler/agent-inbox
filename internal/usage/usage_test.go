package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rec(ts time.Time, msgID, reqID, model string, in, out, cc, cr int64) string {
	return fmt.Sprintf(
		`{"type":"assistant","requestId":%q,"timestamp":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		reqID, ts.UTC().Format(time.RFC3339), msgID, model, in, out, cc, cr) + "\n"
}

func writeTranscript(t *testing.T, root, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	body := strings.Join(lines, "")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// The same message is written more than once — twice in a row in one file, and
// again when a session is resumed into a new transcript. This is not a
// hypothetical: it is what real transcripts contain, and summing without
// deduplicating inflates every number here silently.
func TestDuplicateRecordsAreCountedOnce(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	line := rec(now.Add(-time.Minute), "msg_1", "req_1", "claude-opus-5", 10, 20, 30, 40)
	writeTranscript(t, root, "a.jsonl", line, line) // repeated in place
	writeTranscript(t, root, "b.jsonl", line)       // and in a resumed session
	writeTranscript(t, root, "c.jsonl",             //
		rec(now.Add(-2*time.Minute), "msg_2", "req_2", "claude-opus-5", 1, 2, 3, 4))

	c := &Claude{Root: root, Now: fixedClock(now)}
	s, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Week.Input, int64(11); got != want {
		t.Errorf("input = %d, want %d — the duplicate was counted", got, want)
	}
	if got, want := s.Week.Total(), int64(10+20+30+40+1+2+3+4); got != want {
		t.Errorf("total = %d, want %d", got, want)
	}
}

// Cache reads are billed differently but they still consume the window, so
// they count toward the total.
func TestTotalIncludesCache(t *testing.T) {
	w := Window{Input: 1, Output: 2, CacheCreate: 4, CacheRead: 8}
	if got := w.Total(); got != 15 {
		t.Errorf("Total() = %d, want 15", got)
	}
}

// "no data" and "no usage" are the same number and opposite facts.
func TestEmptyWindowIsDistinguishable(t *testing.T) {
	if !(Window{}).Empty() {
		t.Error("a zero window did not report empty")
	}
	if (Window{Output: 1}).Empty() {
		t.Error("a window with usage reported empty")
	}
}

// Reading is incremental: a file is re-parsed only from where the last read
// stopped, and appending must not re-count what came before.
func TestIncrementalReadDoesNotDoubleCount(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	path := writeTranscript(t, root, "a.jsonl",
		rec(now.Add(-time.Minute), "msg_1", "req_1", "m", 5, 0, 0, 0))

	c := &Claude{Root: root, Now: fixedClock(now)}
	if _, err := c.Read(); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(rec(now.Add(-30*time.Second), "msg_2", "req_2", "m", 7, 0, 0, 0))
	f.Close()

	s, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Week.Input; got != 12 {
		t.Errorf("input = %d, want 12", got)
	}
}

// A rotated or replaced file is shorter than the offset recorded for it, so
// reading from that offset would land in the middle of a record.
func TestShrunkFileIsReReadFromTheStart(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	path := writeTranscript(t, root, "a.jsonl",
		rec(now.Add(-time.Minute), "msg_1", "req_1", "m", 100, 0, 0, 0))

	c := &Claude{Root: root, Now: fixedClock(now)}
	if _, err := c.Read(); err != nil {
		t.Fatal(err)
	}
	// Replaced with a shorter file holding a different message.
	if err := os.WriteFile(path, []byte(rec(now, "msg_9", "req_9", "m", 3, 0, 0, 0)), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	// msg_1 is still retained from the first read; msg_9 is new.
	if got := s.Week.Input; got != 103 {
		t.Errorf("input = %d, want 103", got)
	}
}

// A record still being written has no trailing newline. Consuming it would
// skip it permanently once the rest arrives.
func TestPartialFinalRecordIsPickedUpLater(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	full := rec(now.Add(-time.Minute), "msg_1", "req_1", "m", 42, 0, 0, 0)
	path := writeTranscript(t, root, "a.jsonl")
	if err := os.WriteFile(path, []byte(full[:len(full)/2]), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Claude{Root: root, Now: fixedClock(now)}
	if s, _ := c.Read(); s.Week.Input != 0 {
		t.Fatalf("a half-written record was counted: %d", s.Week.Input)
	}
	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if s.Week.Input != 42 {
		t.Errorf("input = %d, want 42 once the record was complete", s.Week.Input)
	}
}

// Records with no usage block, and records that are not assistant turns, are
// not billable and must not become zero-token entries.
func TestNonBillableRecordsAreIgnored(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	writeTranscript(t, root, "a.jsonl",
		`{"type":"user","message":{"content":"hi"}}`+"\n",
		`{"type":"assistant","requestId":"r","timestamp":"`+now.Format(time.RFC3339)+`","message":{"id":"m","usage":{}}}`+"\n",
		"not json at all\n",
		"\n",
		rec(now.Add(-time.Minute), "msg_1", "req_1", "m", 5, 0, 0, 0),
	)
	c := &Claude{Root: root, Now: fixedClock(now)}
	s, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if s.Week.Input != 5 {
		t.Errorf("input = %d, want 5", s.Week.Input)
	}
}

// The block runs five hours from the first message after a gap, not from a
// wall-clock boundary — that is how the limit itself resets.
func TestBlockRunsFromTheFirstMessage(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 12, 15, 30, 0, 0, time.UTC)
	writeTranscript(t, root, "a.jsonl",
		// An old block, long expired.
		rec(now.Add(-30*time.Hour), "old", "req_old", "m", 1000, 0, 0, 0),
		// The current block starts here.
		rec(now.Add(-2*time.Hour), "msg_1", "req_1", "m", 10, 0, 0, 0),
		rec(now.Add(-time.Hour), "msg_2", "req_2", "m", 20, 0, 0, 0),
	)
	c := &Claude{Root: root, Now: fixedClock(now)}
	s, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if s.Block.Input != 30 {
		t.Errorf("block input = %d, want 30 — the expired block leaked in", s.Block.Input)
	}
	if s.Week.Input != 1030 {
		t.Errorf("week input = %d, want 1030", s.Week.Input)
	}
	wantStart := now.Add(-2 * time.Hour).Truncate(time.Hour)
	if !s.Block.Start.Equal(wantStart) {
		t.Errorf("block start = %s, want %s", s.Block.Start, wantStart)
	}
	if !s.ResetsAt.Equal(wantStart.Add(BlockLength)) {
		t.Errorf("ResetsAt = %s, want %s", s.ResetsAt, wantStart.Add(BlockLength))
	}
}

// With no activity inside the window there is no block in progress, and
// reporting one that reset hours ago would be worse than reporting none.
func TestExpiredBlockIsNotReported(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	writeTranscript(t, root, "a.jsonl",
		rec(now.Add(-20*time.Hour), "msg_1", "req_1", "m", 10, 0, 0, 0))

	c := &Claude{Root: root, Now: fixedClock(now)}
	s, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Block.Empty() {
		t.Errorf("an expired block was reported: %+v", s.Block)
	}
	if !s.ResetsAt.IsZero() {
		t.Errorf("ResetsAt = %s, want zero", s.ResetsAt)
	}
	if s.Week.Input != 10 {
		t.Errorf("week input = %d — the week should still hold it", s.Week.Input)
	}
}

// Which model did the work changes what a token count means, so the number is
// not interpretable without it.
func TestModelsAreReported(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	writeTranscript(t, root, "a.jsonl",
		rec(now.Add(-time.Minute), "m1", "r1", "claude-opus-5", 1, 0, 0, 0),
		rec(now.Add(-time.Minute), "m2", "r2", "claude-haiku-4-5", 1, 0, 0, 0),
	)
	c := &Claude{Root: root, Now: fixedClock(now)}
	s, _ := c.Read()
	if len(s.Block.Models) != 2 || s.Block.Models[0] != "claude-haiku-4-5" {
		t.Errorf("models = %v, want both, sorted", s.Block.Models)
	}
}

// A missing root is an ordinary state — Claude Code may not be installed — and
// must not be an error.
func TestMissingRootIsNotAnError(t *testing.T) {
	c := &Claude{Root: filepath.Join(t.TempDir(), "nope"), Now: fixedClock(time.Now())}
	s, err := c.Read()
	if err != nil {
		t.Errorf("missing root returned an error: %v", err)
	}
	if !s.Week.Empty() {
		t.Error("a missing root produced usage")
	}
	if !s.Estimated {
		t.Error("Estimated must always be set — the number is derived, not reported")
	}
}

// Entries outside every window are dropped, and the dedup keys with them, or
// both grow for as long as the process runs.
func TestOldEntriesArePruned(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	writeTranscript(t, root, "a.jsonl",
		rec(now.Add(-time.Minute), "m1", "r1", "m", 1, 0, 0, 0))
	c := &Claude{Root: root, Now: fixedClock(now)}
	if _, err := c.Read(); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 1 || len(c.seen) != 1 {
		t.Fatalf("setup: entries=%d seen=%d", len(c.entries), len(c.seen))
	}
	// Now ten days later, the entry is outside retention.
	c.Now = fixedClock(now.Add(10 * 24 * time.Hour))
	if _, err := c.Read(); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 0 {
		t.Errorf("entries = %d after pruning, want 0", len(c.entries))
	}
	if len(c.seen) != 0 {
		t.Errorf("seen = %d after pruning, want 0", len(c.seen))
	}
}
