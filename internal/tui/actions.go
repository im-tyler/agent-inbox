package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/driver"
)

// renderActions draws the "more actions" menu. Each letter executes
// the corresponding action and transitions to the appropriate view.
func (m *Model) renderActions() string {
	snap := m.inbox.Snapshot()
	selName := ""
	if m.selected >= 1 && m.selected <= len(snap) {
		selName = snap[m.selected-1].Name
	}

	var b strings.Builder
	b.WriteString(headerStyle.Render("more actions"))
	b.WriteString("\n\n")
	if selName != "" {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  selected: %s", selName)))
		b.WriteString("\n\n")
	}
	b.WriteString("  n  new project\n")
	b.WriteString("  d  delete project\n")
	b.WriteString("  t  change tool\n")
	b.WriteString("  a  attach to session\n")
	b.WriteString("  K  king mode (supervisor)\n")
	b.WriteString("  ?  help\n")

	footer := mutedStyle.Render("press a key to execute  esc to close")
	return renderFrame(m.width, m.height, "actions", b.String(), footer)
}

func (m *Model) handleActionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.view = viewList
		return m, nil

	case "n":
		m.view = viewNewProject
		cwd, _ := os.Getwd()
		m.np = newProjectModelInitial(cwd)
		m.np.folder.Focus()
		return m, textinput.Blink

	case "d":
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			m.view = viewList
			m.toast = "no project selected"
			m.toastAt = time.Now()
			return m, nil
		}
		if snap[m.selected-1].Status == driver.StatusWorking {
			m.view = viewList
			m.toast = "cancel the send first (x)"
			m.toastAt = time.Now()
			return m, nil
		}
		m.view = viewDeleteConfirm
		return m, nil

	case "t":
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			m.view = viewList
			m.toast = "no project selected"
			m.toastAt = time.Now()
			return m, nil
		}
		if snap[m.selected-1].Status == driver.StatusWorking {
			m.view = viewList
			m.toast = "cancel the send first (x)"
			m.toastAt = time.Now()
			return m, nil
		}
		m.pendingTool = ""
		m.view = viewToolPicker
		return m, nil

	case "a":
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			m.view = viewList
			m.toast = "no project selected"
			m.toastAt = time.Now()
			return m, nil
		}
		args, dir, err := m.inbox.AttachArgs(m.selected)
		if err != nil {
			m.view = viewList
			m.toast = err.Error()
			m.toastAt = time.Now()
			return m, nil
		}
		m.attachRequest = &attachArgs{Argv: args, Dir: dir}
		return m, tea.Quit

	case "K":
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			m.view = viewList
			m.toast = "no project selected"
			m.toastAt = time.Now()
			return m, nil
		}
		m.kingIdx = m.selected
		m.kingInput = textinput.New()
		m.kingInput.CharLimit = 0
		m.kingInput.Width = 60
		m.kingInput.Placeholder = "message"
		m.view = viewKing
		return m, nil

	case "?":
		m.view = viewList
		m.helpMode = true
		return m, nil
	}

	return m, nil
}
