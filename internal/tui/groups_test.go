package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/inbox"
)

// tabsFixture is two supervisors over three projects.
func tabsFixture(t *testing.T) Model {
	t.Helper()
	projects := []*inbox.Project{
		{Name: "sup-infra", Tool: "claude", Dir: "/si", Status: driver.StatusIdle},
		{Name: "sup-product", Tool: "claude", Dir: "/sp", Status: driver.StatusIdle},
		{Name: "teploy", Tool: "claude", Dir: "/t", Status: driver.StatusWaiting, LastMessage: "done"},
		{Name: "infra", Tool: "claude", Dir: "/i", Status: driver.StatusIdle},
		{Name: "neutron", Tool: "claude", Dir: "/n", Status: driver.StatusWorking},
	}
	in := inbox.New(projects, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json")).
		WithGroups([]inbox.Group{
			{Name: "infra", King: "sup-infra", Projects: []string{"teploy", "infra"}},
			{Name: "product", King: "sup-product", Projects: []string{"neutron"}},
		})
	t.Cleanup(in.Close)
	m := New(in, "")
	m.width, m.height = 100, 30
	m.resetSidebarCursor()
	return m
}

// A tab shows its own supervisor and its own fleet, and nothing from the other
// tab. That separation is the whole point of splitting a fleet.
func TestSidebarShowsOnlyTheActiveGroup(t *testing.T) {
	m := tabsFixture(t)
	snap := m.inbox.Snapshot()

	infra := strings.Join(m.buildSidebarLines(snap, 24), "\n")
	for _, want := range []string{"sup-infra", "teploy", "infra"} {
		if !strings.Contains(infra, want) {
			t.Errorf("group infra sidebar missing %q:\n%s", want, infra)
		}
	}
	for _, unwanted := range []string{"sup-product", "neutron"} {
		if strings.Contains(infra, unwanted) {
			t.Errorf("group infra sidebar leaked %q:\n%s", unwanted, infra)
		}
	}

	m.selectGroup(1)
	product := strings.Join(m.buildSidebarLines(m.inbox.Snapshot(), 24), "\n")
	if !strings.Contains(product, "neutron") {
		t.Errorf("group product sidebar missing neutron:\n%s", product)
	}
	if strings.Contains(product, "teploy") {
		t.Errorf("group product sidebar leaked teploy:\n%s", product)
	}
}

// The count under the sidebar is this tab's fleet, not the whole roster.
// Reporting all five under a sidebar drawing two would be the same bug the
// supervisor once caused in this block.
func TestFleetCountIsPerGroup(t *testing.T) {
	m := tabsFixture(t)
	joined := strings.Join(m.buildSidebarLines(m.inbox.Snapshot(), 24), "\n")
	if !strings.Contains(joined, "2 projects") {
		t.Errorf("want 2 projects (teploy and infra), got:\n%s", joined)
	}
	m.selectGroup(1)
	joined = strings.Join(m.buildSidebarLines(m.inbox.Snapshot(), 24), "\n")
	if !strings.Contains(joined, "1 project") {
		t.Errorf("want 1 project (neutron), got:\n%s", joined)
	}
}

// Switching tabs moves the cursor to something the new tab is drawing.
func TestSelectGroupMovesTheCursorIntoTheNewTab(t *testing.T) {
	m := tabsFixture(t)
	m.selectGroup(1)
	snap := m.inbox.Snapshot()
	if snap[m.sidebarCursor-1].Name != "neutron" {
		t.Errorf("cursor = %q, want neutron", snap[m.sidebarCursor-1].Name)
	}
}

// Tabs wrap, so a two-group fleet toggles rather than sticking at one end.
func TestSelectGroupWraps(t *testing.T) {
	m := tabsFixture(t)
	m.selectGroup(2)
	if m.activeGroup != 0 {
		t.Errorf("activeGroup = %d after wrapping past the end, want 0", m.activeGroup)
	}
	m.selectGroup(-1)
	if m.activeGroup != 1 {
		t.Errorf("activeGroup = %d after wrapping below zero, want 1", m.activeGroup)
	}
}

// j/k walk this tab's fleet and stop at its ends. Walking into the next group's
// projects would act on rows the sidebar is not showing.
func TestSidebarNavigationStaysInTheGroup(t *testing.T) {
	m := tabsFixture(t)
	snap := m.inbox.Snapshot()
	for range 5 {
		m.moveSidebar(snap, +1)
	}
	if got := snap[m.sidebarCursor-1].Name; got != "infra" {
		t.Errorf("cursor ran to %q, want it to stop at infra", got)
	}
	for range 5 {
		m.moveSidebar(snap, -1)
	}
	if got := snap[m.sidebarCursor-1].Name; got != "teploy" {
		t.Errorf("cursor ran to %q, want it to stop at teploy", got)
	}
}

// The supervisor's row is a label for the conversation on screen, not a
// destination — a/d/t/x on it would all be refused.
func TestSidebarNavigationSkipsTheSupervisor(t *testing.T) {
	m := tabsFixture(t)
	snap := m.inbox.Snapshot()
	for range 4 {
		m.moveSidebar(snap, -1)
		if m.sidebarCursor == m.kingIndex() {
			t.Fatalf("cursor landed on the supervisor")
		}
	}
}

// The strip names every group and marks which one is open, with the count of
// what is waiting in the ones that are not.
func TestTabLineNamesEveryGroup(t *testing.T) {
	m := tabsFixture(t)
	line := m.buildTabLine(m.inbox.Snapshot(), 80)
	for _, want := range []string{"infra", "product"} {
		if !strings.Contains(line, want) {
			t.Errorf("tab line missing %q: %q", want, line)
		}
	}
	// teploy is waiting in group infra.
	if !strings.Contains(line, "1●") {
		t.Errorf("tab line does not carry the waiting count: %q", line)
	}
}

// One supervisor draws no tab strip. Most installs are that, and they should
// keep the layout they had.
func TestNoTabStripForASingleGroup(t *testing.T) {
	m := sidebarFixture(t)
	if line := m.buildTabLine(m.inbox.Snapshot(), 80); line != "" {
		t.Errorf("a single-group fleet drew a tab strip: %q", line)
	}
}

// The frame is built from fixed-width rows; a tab strip that overflowed would
// push the right border off the line.
func TestTabStripHoldsTheFrameAt80Columns(t *testing.T) {
	m := tabsFixture(t)
	for _, w := range []int{62, 80, 100} {
		m.width, m.height = w, 24
		for i, ln := range strings.Split(m.renderMain(), "\n") {
			if got := lipgloss.Width(ln); got > w {
				t.Errorf("width %d: line %d overflows (%d cols): %q", w, i, got, ln)
			}
		}
	}
}

// shift+tab cycles groups from the composer, where every printable key belongs
// to the draft.
func TestShiftTabCyclesGroups(t *testing.T) {
	m := tabsFixture(t)
	next, _ := m.handleMainKey(tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := next.(Model).activeGroup; got != 1 {
		t.Errorf("activeGroup = %d after shift+tab, want 1", got)
	}
}
