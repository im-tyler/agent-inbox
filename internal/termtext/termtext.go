// Package termtext makes untrusted text safe to put on a terminal, and
// measures it the way a terminal will.
//
// Running an argv without a shell stops a substituted value becoming a second
// command. It does nothing about the bytes themselves. Every string this
// program renders — an agent's reply, a title from an HTTP feed, recovered
// terminal output — is written straight to the user's terminal, and a terminal
// treats some byte sequences as instructions: clear the screen, move the
// cursor, retitle the window, write the system clipboard (OSC 52), emit a
// hyperlink pointing anywhere (OSC 8).
//
// So terminal control is its own trust boundary, separate from command
// execution, and this is where it is enforced: sanitise the raw text first,
// then apply our own styling to the result.
package termtext

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// Clean strips terminal control sequences and control characters from s,
// preserving printable Unicode. Newline and tab survive; everything else in
// the C0/C1 ranges does not.
func Clean(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // invalid byte: drop it
			continue
		}
		switch {
		case r == 0x1b: // ESC — start of a control sequence
			i += escapeLen(s[i:])
			continue
		case r == '\n', r == '\t':
			b.WriteRune(r)
		case r == '\r':
			// A bare carriage return lets text overwrite what is already on
			// the line, which is how output gets hidden rather than shown.
			// Treat CRLF as one newline and a lone CR as a newline too.
			if i+size < len(s) && s[i+size] == '\n' {
				i += size
				continue
			}
			b.WriteRune('\n')
		case r < 0x20, r == 0x7f: // other C0 and DEL
		case r >= 0x80 && r <= 0x9f: // C1 — includes 8-bit CSI/OSC introducers
		case unicode.Is(unicode.Cf, r):
			// Format characters: zero-width joiners, bidi overrides. Bidi in
			// particular can make text render in an order other than the one
			// it is stored in.
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// escapeLen returns the byte length of the escape sequence starting at s[0],
// which the caller has already established is ESC.
//
// The point is to consume the whole sequence. Dropping only the ESC would
// leave its parameter bytes as visible garbage; dropping too little would
// leave the terminal to interpret the remainder.
func escapeLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[': // CSI: params, then a byte in @..~
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']': // OSC: runs to BEL or ST (ESC \)
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	case 'P', '_', '^', 'X': // DCS, APC, PM, SOS: run to ST
		for i := 2; i < len(s); i++ {
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			if s[i] == 0x07 {
				return i + 1
			}
		}
		return len(s)
	default:
		// Two-byte escape, or a charset selector like ESC ( B.
		if s[1] == '(' || s[1] == ')' || s[1] == '*' || s[1] == '+' {
			if len(s) >= 3 {
				return 3
			}
			return len(s)
		}
		return 2
	}
}

// OneLine cleans s and collapses it to a single line, for a list row or a
// status field where an embedded newline would break the layout.
func OneLine(s string) string {
	return strings.Join(strings.Fields(Clean(s)), " ")
}

// Width is the number of terminal cells s occupies. Not len, which counts
// bytes, and not the rune count either: a CJK ideograph or an emoji occupies
// two cells, and a combining mark occupies none.
func Width(s string) int { return runewidth.StringWidth(s) }

// Truncate shortens s to at most n terminal cells, appending an ellipsis when
// it had to cut.
//
// Byte slicing was the previous approach, which could cut a multi-byte rune in
// half and emit invalid UTF-8, and which mismeasured every wide glyph.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if Width(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return runewidth.Truncate(s, n, "…")
}

// Pad returns s padded with spaces to exactly n cells, truncating if it is
// wider. Used for columns, where a field that measures its own width wrongly
// shunts every following column out of alignment.
func Pad(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = Truncate(s, n)
	if w := Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}
