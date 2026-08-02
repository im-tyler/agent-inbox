package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"agentinbox/internal/driver"
	"agentinbox/internal/feed"
	"agentinbox/internal/inbox"
)

func row(source, id, cwd string) feed.Item {
	return feed.Item{Source: source, ID: id, Context: map[string]string{"cwd": cwd}}
}

func TestToolFor(t *testing.T) {
	cases := map[string]string{
		"claude-code": "claude",
		"claude":      "claude",
		"opencode":    "opencode",
		"codex":       "codex",
		"teploy-ship": "",
		"":            "",
	}
	for source, want := range cases {
		if got := toolFor(source); got != want {
			t.Errorf("toolFor(%q) = %q, want %q", source, got, want)
		}
	}
}

// A live Claude Code session is somebody else's to write to, so it is forked
// rather than resumed. opencode and codex come off disk and are adopted whole.
func TestAdoptSessionForksClaude(t *testing.T) {
	sess, fork := adoptSession("claude-code", "abc")
	if sess != "" {
		t.Errorf("claude id adopted for resume: %q", sess)
	}
	if fork != "abc" {
		t.Errorf("fork source = %q, want abc", fork)
	}
	for _, source := range []string{"opencode", "codex"} {
		sess, fork := adoptSession(source, "abc")
		if sess != "abc" || fork != "" {
			t.Errorf("%s: session = %q, fork = %q, want abc/empty", source, sess, fork)
		}
	}
}

func TestCandidateFromDrivableRow(t *testing.T) {
	c, ok := candidateFrom(row("opencode", "ses_1", "/repo/lab"))
	if !ok {
		t.Fatal("opencode row was not adoptable")
	}
	if c.Tool != "opencode" || c.Dir != "/repo/lab" || c.SessionID != "ses_1" || c.ForkFrom != "" {
		t.Errorf("candidate = %+v", c)
	}
	if c.Name() != "lab" {
		t.Errorf("Name() = %q, want lab", c.Name())
	}
}

func TestCandidateFromClaudeForksSession(t *testing.T) {
	c, ok := candidateFrom(row("claude-code", "abc", "/repo/neutron"))
	if !ok {
		t.Fatal("claude row was not adoptable")
	}
	if c.SessionID != "" {
		t.Errorf("session = %q, want empty (a live claude session can't be resumed)", c.SessionID)
	}
	// Dropping the id entirely was the old behaviour and it threw away the
	// context that made adoption worth doing.
	if c.ForkFrom != "abc" {
		t.Errorf("fork source = %q, want abc", c.ForkFrom)
	}
	if c.Tool != "claude" || c.Name() != "neutron" {
		t.Errorf("candidate = %+v", c)
	}
}

func TestCandidateFromRejectsUndrivableRows(t *testing.T) {
	// A deploy has no driver behind it.
	if _, ok := candidateFrom(row("teploy-ship", "1", "/repo/a")); ok {
		t.Error("a non-agent source was offered as a project")
	}
	// A session that never said where it lives cannot be adopted.
	if _, ok := candidateFrom(row("opencode", "1", "")); ok {
		t.Error("a row with no cwd was offered as a project")
	}
}

func TestCandidateNameFallsBackToDir(t *testing.T) {
	if got := (candidate{Dir: "/"}).Name(); got != "/" {
		t.Errorf("Name() = %q, want /", got)
	}
}

// Every sidebar row is marker + name + glyph with a two-column marker, so the
// king's name and the fleet's share a left edge. A per-row index used to sit
// between them, selecting nothing and shifting the fleet two columns right.
func TestSidebarNamesShareALeftEdge(t *testing.T) {
	m := sidebarFixture(t)
	lines := m.buildSidebarLines(m.inbox.Snapshot(), 24)

	var rows []string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "★ ") || strings.HasPrefix(ln, "  king") ||
			strings.HasPrefix(ln, "  omni") || strings.HasPrefix(ln, "  akiroo") {
			rows = append(rows, ln)
		}
	}
	if len(rows) < 2 {
		t.Fatalf("expected several project rows, got %q", rows)
	}
	for _, ln := range rows {
		if r := []rune(ln); len(r) < 3 || r[1] != ' ' {
			t.Errorf("row %q does not use a two-column marker", ln)
		}
	}
}

func sidebarFixture(t *testing.T) Model {
	t.Helper()
	projects := []*inbox.Project{
		{Name: "king", Tool: "claude", Dir: "/k", Status: driver.StatusWorking},
		{Name: "omni", Tool: "opencode", Dir: "/o", Status: driver.StatusWaiting, LastMessage: "done"},
		{Name: "akiroo", Tool: "codex", Dir: "/a", Status: driver.StatusError, LastErr: "boom"},
	}
	in := inbox.New(projects, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json"))
	m := New(in, "")
	m.width, m.height = 100, 30
	return m
}

// A failed project has to show why, or the ✗ is a fact with no cause attached.
func TestSidebarShowsTheErrorReason(t *testing.T) {
	m := sidebarFixture(t)
	joined := strings.Join(m.buildSidebarLines(m.inbox.Snapshot(), 24), "\n")
	if !strings.Contains(joined, "boom") {
		t.Errorf("the error reason never appears:\n%s", joined)
	}
}

// The composer grows with the draft and stops at maxInputLines, so a long
// prompt scrolls inside itself instead of eating the conversation.
func TestComposerGrowsThenStops(t *testing.T) {
	m := sidebarFixture(t)
	if got := m.mainInput.Height(); got != 1 {
		t.Errorf("empty composer is %d rows, want 1", got)
	}
	m.mainInput.SetValue("one\ntwo\nthree")
	m.syncInputHeight()
	if got := m.mainInput.Height(); got != 3 {
		t.Errorf("3-line draft gave %d rows", got)
	}
	m.mainInput.SetValue(strings.Repeat("line\n", 40))
	m.syncInputHeight()
	if got := m.mainInput.Height(); got != maxInputLines {
		t.Errorf("long draft gave %d rows, want the cap of %d", got, maxInputLines)
	}
	// Sending clears it and the box shrinks back.
	m.mainInput.Reset()
	m.syncInputHeight()
	if got := m.mainInput.Height(); got != 1 {
		t.Errorf("composer stayed %d rows after reset", got)
	}
}

// The frame must not be broken by a footer wider than the terminal.
func TestFooterNeverWrapsTheFrame(t *testing.T) {
	for _, w := range []int{40, 62, 76, 100} {
		m := sidebarFixture(t)
		m.width, m.height = w, 24
		for _, focus := range []bool{false, true} {
			m.focusSidebar = focus
			for _, ln := range strings.Split(m.renderMain(), "\n") {
				if lipgloss.Width(ln) > w {
					t.Errorf("width %d focus=%v: line overflows (%d cols): %q", w, focus, lipgloss.Width(ln), ln)
				}
			}
		}
	}
}

// The detail view is where the full replies live, so it is the last place
// that should hand back raw markdown and directive syntax.
func TestDetailViewSharesTheChatRendering(t *testing.T) {
	projects := []*inbox.Project{{
		Name: "king", Tool: "opencode", Dir: "/k", Status: driver.StatusIdle,
		History: []inbox.Message{
			{Role: "assistant", Content: "On it.\n[send to omni: check]", Timestamp: time.Now()},
			{Role: "omni", Content: "## Findings\n**three** issues\n- one\n- two", Timestamp: time.Now()},
		},
	}}
	in := inbox.New(projects, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json"))
	m := New(in, "")
	m.width, m.height = 80, 40
	m.selected = 1

	got := m.viewDetail()
	for _, unwanted := range []string{"[send to", "## ", "**"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q reached the detail view:\n%s", unwanted, got)
		}
	}
	// And the fleet speaker is drawn as one, not as an unlabelled role.
	if !strings.Contains(got, "▸ omni") {
		t.Errorf("fleet reply is not marked as one:\n%s", got)
	}
	for _, want := range []string{"Findings", "three issues", "• one"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}
