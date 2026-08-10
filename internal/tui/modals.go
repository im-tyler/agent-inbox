package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/im-tyler/agent-inbox/internal/config"
)

// renderDeleteConfirm draws the delete-confirmation prompt.
func (m *Model) renderDeleteConfirm() string {
	snap := m.inbox.Snapshot()
	name := "?"
	if m.selected >= 1 && m.selected <= len(snap) {
		name = snap[m.selected-1].Name
	}
	var b strings.Builder
	b.WriteString(headerStyle.Render("! delete project ?"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  delete %s from agent-inbox?\n", name))
	b.WriteString(mutedStyle.Render("  (history and session id will be lost; the project directory on disk is untouched)"))
	b.WriteString("\n\n")
	b.WriteString("  ")
	b.WriteString(errorStyle.Render("y"))
	b.WriteString(" to confirm, ")
	b.WriteString("any other key")
	b.WriteString(" to cancel")
	return b.String()
}

// normalizeSelection brings both cursors back into range after the project
// list changes, and keeps the sidebar off the supervisor — which is not one of
// the projects those keys act on.
func (m *Model) normalizeSelection() {
	snap := m.inbox.Snapshot()
	n := len(snap)
	if n == 0 {
		m.selected, m.sidebarCursor = 1, 1
		return
	}
	clamp := func(v int) int {
		if v > n {
			return n
		}
		if v < 1 {
			return 1
		}
		return v
	}
	m.selected = clamp(m.selected)
	m.sidebarCursor = clamp(m.sidebarCursor)
	// Prefer a non-supervisor row; fall back to the supervisor only when it is
	// the only thing left.
	if m.inbox.IsKing(snap[m.sidebarCursor-1].Name) {
		for i, p := range snap {
			if !m.inbox.IsKing(p.Name) {
				m.sidebarCursor = i + 1
				break
			}
		}
	}
}

func (m *Model) handleDeleteConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		idx := m.selected
		if err := m.inbox.RemoveProject(idx); err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
			m.view = viewMain
			return m, nil
		}
		// Clamp both selections to the new list length. sidebarCursor was
		// missed here, and it is the one the delete was launched from: after
		// removing the last fleet project it could still point past the end,
		// leaving no row highlighted and every subsequent a/d/t/x aimed at an
		// index that no longer exists.
		m.normalizeSelection()
		m.toast = "deleted"
		m.toastAt = time.Now()
		m.view = viewMain
		return m, nil
	default:
		// Any other key cancels.
		m.view = viewMain
		return m, nil
	}
}

// renderToolPicker draws the tool-change modal.
func (m *Model) renderToolPicker() string {
	snap := m.inbox.Snapshot()
	name := "?"
	current := ""
	if m.selected >= 1 && m.selected <= len(snap) {
		p := snap[m.selected-1]
		name = p.Name
		current = p.Tool
	}
	pending := m.pendingTool
	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf("+ change tool for %s +", name)))
	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render(fmt.Sprintf("  current: %s", current)))
	b.WriteString("\n\n")
	for i, tool := range config.KnownTools {
		marker := "  "
		if tool == pending {
			marker = "> "
		} else if tool == current && pending == "" {
			marker = "~ "
		}
		b.WriteString(fmt.Sprintf("  %s[%d] %s\n", marker, i+1, tool))
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("  1-4 to highlight, enter to confirm, esc to cancel"))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("  (changing tool clears the session id — the new tool can't resume the old session)"))
	return b.String()
}

func (m *Model) handleToolPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.view = viewMain
		m.pendingTool = ""
		return m, nil
	case "enter":
		if m.pendingTool == "" {
			m.toast = "press 1-4 to pick a tool first"
			m.toastAt = time.Now()
			return m, nil
		}
		snap := m.inbox.Snapshot()
		name := ""
		if m.selected >= 1 && m.selected <= len(snap) {
			name = snap[m.selected-1].Name
		}
		if err := m.inbox.SetProjectTool(m.selected, m.pendingTool); err != nil {
			m.toast = err.Error()
		} else {
			m.toast = fmt.Sprintf("switched %s to %s", name, m.pendingTool)
		}
		m.toastAt = time.Now()
		m.pendingTool = ""
		m.view = viewMain
		return m, nil
	case "1", "2", "3", "4":
		var n int
		fmt.Sscanf(msg.String(), "%d", &n)
		if n >= 1 && n <= len(config.KnownTools) {
			m.pendingTool = config.KnownTools[n-1]
		}
		return m, nil
	}
	return m, nil
}
