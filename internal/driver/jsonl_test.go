package driver

import (
	"io"
	"strings"
	"testing"
)

// F08: the final chunk of a stream can arrive together with io.EOF, and the
// bytes are the tail of the last record. Below, near, and above the reader's
// internal 64KiB buffer, an unterminated in-limit record must come back
// byte-for-byte, then EOF on the next call.
func TestJSONLReaderReturnsUnterminatedFinalRecord(t *testing.T) {
	for _, size := range []int{20, 1 << 10, 70 << 10} {
		rec := strings.Repeat("x", size-1) + "!"
		jr := newJSONLReader(strings.NewReader(rec), maxJSONLine)
		got, err := jr.Next()
		if err != nil {
			t.Errorf("size %d: Next: %v", size, err)
			continue
		}
		if string(got) != rec {
			t.Errorf("size %d: got %d bytes, want %d (final record truncated or lost)", size, len(got), len(rec))
		}
		if _, err := jr.Next(); err != io.EOF {
			t.Errorf("size %d: second Next err = %v, want io.EOF", size, err)
		}
	}
}

// F08's other half: an oversized record is consumed to its delimiter and the
// reader keeps going — the drain behaviour the scanner never had.
func TestJSONLReaderSkipsOversizedAndKeepsReading(t *testing.T) {
	in := strings.Repeat("a", 50) + "\n" + strings.Repeat("b", 20) + "\n"
	jr := newJSONLReader(strings.NewReader(in), 40)
	got, err := jr.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(got) != strings.Repeat("b", 20)+"\n" {
		t.Fatalf("got %q, want the 20-byte record after the oversized one", got)
	}
	if jr.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", jr.Skipped)
	}
	if _, err := jr.Next(); err != io.EOF {
		t.Fatalf("second Next err = %v, want io.EOF", err)
	}
}
