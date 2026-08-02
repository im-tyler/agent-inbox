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

func TestStripDirectivesRemovesMachineSyntax(t *testing.T) {
	got := stripDirectives("On it.\n[send to omni: check for bugs]\n[note: omni runs on glm-5.2]\nI'll report back.")
	want := "On it.\nI'll report back."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Prose that merely mentions the syntax is not a directive — the parsers read
// whole lines, and the display has to agree with them.
func TestStripDirectivesKeepsProse(t *testing.T) {
	s := "you can write [note: something] on its own line to remember it"
	if got := stripDirectives(s); got != s {
		t.Errorf("stripped prose: %q", got)
	}
}

// A turn that was nothing but dispatch has no words to show.
func TestStripDirectivesCanEmptyAMessage(t *testing.T) {
	if got := stripDirectives("[send to omni: go]\n[send to akiroo: go]"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// A count cut to "2 waitin" is not a number. At the sidebar's narrowest the
// parts get a row each rather than being truncated onto one.
func TestFleetSummaryWrapsRatherThanTruncates(t *testing.T) {
	narrow := fleetSummary(4, 2, 2, 18)
	if len(narrow) != 3 {
		t.Fatalf("got %v, want the parts split across rows", narrow)
	}
	for _, ln := range narrow {
		if lipgloss.Width(ln) > 18 {
			t.Errorf("%q is wider than the sidebar", ln)
		}
	}
	// With room, one line is tidier than three.
	if wide := fleetSummary(4, 2, 2, 40); len(wide) != 2 {
		t.Errorf("got %v, want two rows when it fits", wide)
	}
}

// The count includes the king. A total that silently excluded it never
// matched the rows drawn above it.
func TestFleetSummaryCountsTheKing(t *testing.T) {
	if got := fleetSummary(4, 0, 0, 40); got[0] != "4 projects" {
		t.Errorf("got %q, want 4 projects", got[0])
	}
	// Nothing in flight means no second line at all.
	if got := fleetSummary(4, 0, 0, 40); len(got) != 1 {
		t.Errorf("got %v, want just the total", got)
	}
}

// Agents answer in markdown and a terminal cannot render it, so the markers
// come off and the structure they carried is expressed in ways a terminal has.
func TestWrapBodyStripsMarkdownNoise(t *testing.T) {
	got := strings.Join(wrapBody("## Akiroo — Recent Activity\n**No commits** in 2 days\n- modules untouched\n* 12 commits", 60), "\n")
	for _, unwanted := range []string{"##", "**"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q survived:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{"Akiroo — Recent Activity", "No commits in 2 days", "• modules untouched", "• 12 commits"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// A heading is bolded after wrapping, never before — styling first would put
// escape codes through the wrapper's width arithmetic.
func TestDemarkdownReportsHeadings(t *testing.T) {
	text, heading := demarkdown("### Summary")
	if text != "Summary" || !heading {
		t.Errorf("got %q heading=%v", text, heading)
	}
	if text, heading := demarkdown("not # a heading"); heading || text != "not # a heading" {
		t.Errorf("got %q heading=%v", text, heading)
	}
}

func TestPlural(t *testing.T) {
	if got := fleetSummary(1, 0, 0, 40); got[0] != "1 project" {
		t.Errorf("got %q, want 1 project", got[0])
	}
}

// A preview is a quote from a thread, so it gets the same cleanup the thread
// does — otherwise the sidebar and the list show markers the chat strips.
func TestPreviewTextIsCleanAndFlat(t *testing.T) {
	got := previewText("On it.\n[send to omni: check]\n## Findings\n**three** issues")
	for _, unwanted := range []string{"[send to", "##", "**", "\n"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q survived in %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "Findings") || !strings.Contains(got, "three issues") {
		t.Errorf("content was lost: %q", got)
	}
}

// A message that was nothing but directives has no preview to show.
func TestPreviewTextOfDirectivesOnly(t *testing.T) {
	if got := previewText("[send to omni: go]"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
