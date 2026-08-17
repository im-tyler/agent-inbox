package termtext

import (
	"strings"
	"testing"
)

// Agent replies, feed titles and recovered terminal output all reach the
// terminal as bytes. A terminal reads some of those bytes as instructions.
func TestCleanRemovesTerminalControlSequences(t *testing.T) {
	cases := []struct {
		name, in, mustNotContain string
	}{
		{"clear screen", "before\x1b[2Jafter", "\x1b["},
		{"cursor move", "a\x1b[10;10Hb", "\x1b["},
		{"colour", "\x1b[31mred\x1b[0m", "\x1b["},
		{"OSC 52 clipboard write", "x\x1b]52;c;cGF5bG9hZA==\x07y", "\x1b]"},
		{"OSC 8 hyperlink", "\x1b]8;;https://evil.example\x1b\\text\x1b]8;;\x1b\\", "\x1b]"},
		{"window title", "\x1b]0;pwned\x07ok", "\x1b]"},
		{"DCS", "a\x1bPq#0;2;0;0;0\x1b\\b", "\x1bP"},
		{"APC", "a\x1b_payload\x1b\\b", "\x1b_"},
		{"C1 CSI", "a31mb", ""},
		{"bell", "a\x07b", "\x07"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Clean(c.in)
			if strings.Contains(got, c.mustNotContain) {
				t.Errorf("control sequence survived: %q -> %q", c.in, got)
			}
			if strings.ContainsRune(got, 0x1b) {
				t.Errorf("ESC survived: %q -> %q", c.in, got)
			}
		})
	}
}

func TestCleanKeepsReadableContent(t *testing.T) {
	got := Clean("\x1b[31mred\x1b[0m text")
	if !strings.Contains(got, "red") || !strings.Contains(got, "text") {
		t.Fatalf("content was lost: %q", got)
	}
	// Unicode is text, not control.
	for _, s := range []string{"héllo", "日本語", "🎉", "é"} {
		if Clean(s) != s {
			t.Errorf("%q was altered to %q", s, Clean(s))
		}
	}
}

// A bare carriage return lets later text overwrite earlier text on the same
// line, which is a way to hide output rather than show it.
func TestCleanNeutralisesCarriageReturnOverwrite(t *testing.T) {
	got := Clean("visible\rhidden")
	if strings.ContainsRune(got, '\r') {
		t.Errorf("carriage return survived: %q", got)
	}
	if !strings.Contains(got, "visible") {
		t.Errorf("overwritten text should still be shown: %q", got)
	}
	if got := Clean("a\r\nb"); got != "a\nb" {
		t.Errorf("CRLF should collapse to one newline, got %q", got)
	}
}

func TestCleanKeepsNewlinesAndTabs(t *testing.T) {
	if got := Clean("a\nb\tc"); got != "a\nb\tc" {
		t.Errorf("got %q", got)
	}
}

// Width has to be measured in terminal cells: a CJK ideograph occupies two,
// and byte or rune counting misjudges both by different amounts.
func TestWidthCountsTerminalCells(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"日本", 4},
		{"é", 1}, // combining acute adds no width
	}
	for _, c := range cases {
		if got := Width(c.in); got != c.want {
			t.Errorf("Width(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// Byte slicing could cut a rune in half and emit invalid UTF-8.
func TestTruncateNeverExceedsTheRequestedCells(t *testing.T) {
	for _, s := range []string{"abcdefghij", "日本語のテキスト", "🎉🎉🎉🎉🎉", "héllo wörld"} {
		for n := range 12 {
			got := Truncate(s, n)
			if w := Width(got); w > n {
				t.Errorf("Truncate(%q, %d) = %q, width %d exceeds %d", s, n, got, w, n)
			}
			if !isValidUTF8(got) {
				t.Errorf("Truncate(%q, %d) produced invalid UTF-8: %q", s, n, got)
			}
		}
	}
}

func TestPadIsExactlyTheRequestedCells(t *testing.T) {
	for _, s := range []string{"", "ab", "日本語", "🎉"} {
		for _, n := range []int{1, 3, 5, 10} {
			if got := Pad(s, n); Width(got) != n {
				t.Errorf("Pad(%q, %d) has width %d", s, n, Width(got))
			}
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
