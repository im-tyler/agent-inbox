package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/im-tyler/agent-inbox/internal/driver"
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
	b.WriteString("  i  inbox — every session, add one as a project\n")
	b.WriteString("  n  new project from a path\n")
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
		m.view = viewMain
		return m, nil

	case "i":
		return m, m.openInbox()

	case "n":
		m.view = viewNewProject
		cwd, _ := os.Getwd()
		m.np = newProjectModelInitial(cwd)
		m.np.folder.Focus()
		return m, textinput.Blink

	case "d":
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			m.view = viewMain
			m.toast = "no project selected"
			m.toastAt = time.Now()
			return m, nil
		}
		if snap[m.selected-1].Status == driver.StatusWorking {
			m.view = viewMain
			m.toast = "cancel the send first (x)"
			m.toastAt = time.Now()
			return m, nil
		}
		m.view = viewDeleteConfirm
		return m, nil

	case "t":
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			m.view = viewMain
			m.toast = "no project selected"
			m.toastAt = time.Now()
			return m, nil
		}
		if snap[m.selected-1].Status == driver.StatusWorking {
			m.view = viewMain
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
			m.view = viewMain
			m.toast = "no project selected"
			m.toastAt = time.Now()
			return m, nil
		}
		args, dir, err := m.inbox.AttachArgs(m.selected)
		if err != nil {
			m.view = viewMain
			m.toast = err.Error()
			m.toastAt = time.Now()
			return m, nil
		}
		m.attachRequest = &attachArgs{Argv: args, Dir: dir}
		return m, tea.Quit

	case "K":
		// Opens the supervisor's panel. It used to promote whatever was
		// selected, which is why the panel and the dashboard could disagree
		// about who the king was — there is one supervisor now, and it is not
		// one of the projects in this list.
		if m.kingIndex() == 0 {
			m.view = viewMain
			m.toast = "no supervisor configured"
			m.toastAt = time.Now()
			return m, nil
		}
		// The panel opens on the whole fleet, matching the dashboard. It used
		// to open on nothing, so the first thing it showed was "(none — press
		// + to add)" for projects that were already there. Narrowing with
		// +/- still works; it is now a filter rather than a setup step.
		if len(m.connected) == 0 {
			m.connected = m.inbox.FleetNames()
		}
		m.kingInput = textinput.New()
		m.kingInput.CharLimit = 0
		m.kingInput.Width = 60
		m.kingInput.Placeholder = "message"
		m.view = viewKing
		return m, nil

	case "?":
		m.view = viewMain
		m.helpMode = true
		return m, nil
	}

	return m, nil
}
