package logbook

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeFileAppend(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

func TestAppendReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logbook.jsonl")
	if got := Read(path, 20); len(got) != 0 {
		t.Fatalf("missing logbook read as %d entries, want silence", len(got))
	}
	if err := Append(path, "cli", "asked neutron to finish the client by Friday"); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, "mcp", "teploy parked until then"); err != nil {
		t.Fatal(err)
	}
	got := Read(path, 20)
	if len(got) != 2 {
		t.Fatalf("read %d entries, want 2", len(got))
	}
	if got[0].Author != "cli" || got[1].Author != "mcp" {
		t.Fatalf("order or authors wrong: %+v", got)
	}
	if got[0].T.After(got[1].T) {
		t.Fatal("entries not in write order")
	}
}

func TestReadBoundsToLastN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logbook.jsonl")
	for i := 0; i < 30; i++ {
		if err := Append(path, "cli", strings.Repeat("x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	got := Read(path, 5)
	if len(got) != 5 {
		t.Fatalf("read %d, want 5", len(got))
	}
	// The newest five, not the oldest five.
	if got[0].Text != strings.Repeat("x", 26) {
		t.Fatalf("got the wrong end: %q", got[0].Text)
	}
}

func TestAppendTruncatesAndSkipsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logbook.jsonl")
	long := strings.Repeat("a", maxEntryLen+500)
	if err := Append(path, "cli", long); err != nil {
		t.Fatal(err)
	}
	got := Read(path, 1)
	if len(got) != 1 || len(got[0].Text) > maxEntryLen+len("\n[truncated]") {
		t.Fatalf("entry not bounded: %d", len(got[0].Text))
	}
	if err := Append(path, "cli", "   "); err != nil {
		t.Fatal(err)
	}
	if len(Read(path, 20)) != 1 {
		t.Fatal("whitespace-only entry was recorded")
	}
}

// A damaged line — a crash mid-append, say — must cost that line, not the
// log. Everything after it still reads.
func TestReadSkipsDamagedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logbook.jsonl")
	if err := Append(path, "cli", "first"); err != nil {
		t.Fatal(err)
	}
	// Write a truncated JSON line directly, as a killed process would leave.
	if err := writeFileAppend(path, []byte(`{"t":"2026-09-10T`)); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, "cli", "last"); err != nil {
		t.Fatal(err)
	}
	got := Read(path, 20)
	if len(got) != 2 || got[0].Text != "first" || got[1].Text != "last" {
		t.Fatalf("damaged line cost more than itself: %+v", got)
	}
}

func TestConcurrentAppendsDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logbook.jsonl")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Append(path, "cli", strings.Repeat("y", 200)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := Read(path, 100)
	if len(got) != 20 {
		t.Fatalf("%d intact entries, want 20 — lines interleaved", len(got))
	}
}
