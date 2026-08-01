package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"agentinbox/internal/driver"
)

func TestSpeakerRoles(t *testing.T) {
	if _, label, _ := speaker("user", "claude"); label != "you" {
		t.Errorf("user label = %q, want you", label)
	}
	// The assistant is named by the tool actually answering, not "assistant".
	if _, label, _ := speaker("assistant", "opencode"); label != "opencode" {
		t.Errorf("assistant label = %q, want opencode", label)
	}
	// Any other role is a fleet project answering the king by name.
	glyph, label, _ := speaker("omilator", "claude")
	if label != "omilator" || glyph != "▸" {
		t.Errorf("fleet speaker = %q %q, want ▸ omilator", glyph, label)
	}
}

func TestSpeakerLinePinsRightEdge(t *testing.T) {
	line := speakerLine("›", "you", "3:04PM", userStyle, 40)
	if w := lipgloss.Width(line); w != 40 {
		t.Errorf("width = %d, want 40", w)
	}
	if !strings.HasSuffix(line, "3:04PM") {
		t.Errorf("timestamp not at the right edge: %q", line)
	}
}

// A narrow pane must not produce a negative pad and panic.
func TestSpeakerLineSurvivesNarrowPane(t *testing.T) {
	line := speakerLine("●", "a-very-long-tool-name", "3:04PM", userStyle, 8)
	if line == "" {
		t.Error("empty line")
	}
}

func TestWrapBodyIndentsEveryLine(t *testing.T) {
	got := wrapBody("one two three four five six seven", 20)
	if len(got) < 2 {
		t.Fatalf("expected a wrap, got %d line(s): %q", len(got), got)
	}
	for i, ln := range got {
		if !strings.HasPrefix(ln, "  ") {
			t.Errorf("line %d not indented: %q", i, ln)
		}
	}
}

func TestWrapBodyKeepsExplicitNewlines(t *testing.T) {
	got := wrapBody("first\nsecond", 40)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(got), got)
	}
}

func TestWorkingLabel(t *testing.T) {
	if got := workingLabel(""); got != "working" {
		t.Errorf("got %q", got)
	}
	if got := workingLabel("Bash"); got != "working · Bash" {
		t.Errorf("got %q", got)
	}
}

// The sidebar glyph has to stay one column wide or the name column it shares
// a line with starts overflowing again.
func TestStatusGlyphIsOneColumn(t *testing.T) {
	for _, s := range []driver.Status{driver.StatusIdle, driver.StatusWorking, driver.StatusWaiting, driver.StatusError} {
		if w := lipgloss.Width(statusGlyph(s, "⠋")); w != 1 {
			t.Errorf("%s glyph is %d columns wide, want 1", s, w)
		}
	}
}

func TestFrameCyclesWithoutOverflow(t *testing.T) {
	m := Model{}
	seen := map[string]bool{}
	for i := 0; i < len(spinnerFrames)*3; i++ {
		m.spin = i
		f := m.frame()
		if lipgloss.Width(f) != 1 {
			t.Fatalf("frame %q is not one column", f)
		}
		seen[f] = true
	}
	if len(seen) != len(spinnerFrames) {
		t.Errorf("cycled through %d frames, want %d", len(seen), len(spinnerFrames))
	}
}
