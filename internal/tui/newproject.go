package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/config"
)

// newProjectStep tracks where we are in the new-project modal.
type newProjectStep int

const (
	npFolder newProjectStep = iota
	npAgent
	npName
)

// newProjectModel holds the state of the new-project modal. Embedded into
// the main Model when view == viewNewProject.
type newProjectModel struct {
	step    newProjectStep
	folder  textinput.Model
	name    textinput.Model
	agent   string // "claude" | "opencode" | "codex" | "mock"
	errMsg  string
}

func newProjectModelInitial(cwd string) newProjectModel {
	f := textinput.New()
	f.Placeholder = "/absolute/path/to/project"
	f.CharLimit = 0
	f.Width = 60
	if cwd != "" {
		f.SetValue(cwd)
	}

	n := textinput.New()
	n.Placeholder = "project name"
	n.CharLimit = 64
	n.Width = 30

	return newProjectModel{
		step:   npFolder,
		folder: f,
		name:   n,
	}
}

// renderNewProject draws the modal. Used by Model.View when in viewNewProject.
func (m *Model) renderNewProject() string {
	np := m.np
	var b strings.Builder

	b.WriteString(headerStyle.Render("+ new project +"))
	b.WriteString("\n\n")

	switch np.step {
	case npFolder:
		b.WriteString("  folder: ")
		b.WriteString(np.folder.View())
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("  type a path (or relative from $PWD), then press enter"))
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render("  esc cancels"))

	case npAgent:
		b.WriteString("  folder: ")
		b.WriteString(np.folder.Value())
		b.WriteString("\n\n")
		b.WriteString("  agent:  ")
		for i, tool := range config.KnownTools {
			marker := "  "
			if np.agent == tool {
				marker = "> "
			}
			b.WriteString(marker)
			b.WriteString(fmt.Sprintf("[%d] %s   ", i+1, tool))
		}
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("  up/down or 1-4 to highlight, enter to confirm"))

	case npName:
		b.WriteString("  folder: ")
		b.WriteString(np.folder.Value())
		b.WriteString("\n")
		b.WriteString("  agent:  ")
		b.WriteString(np.agent)
		b.WriteString("\n\n")
		b.WriteString("  name:   ")
		b.WriteString(np.name.View())
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("  enter to add  esc to cancel"))
	}

	if np.errMsg != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("  error: " + np.errMsg))
	}

	return b.String()
}

// handleNewProjectKey processes keys while the new-project modal is open.
func (m *Model) handleNewProjectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.np.step {
	case npFolder:
		return m.handleNpFolderKey(msg)
	case npAgent:
		return m.handleNpAgentKey(msg)
	case npName:
		return m.handleNpNameKey(msg)
	}
	return m, nil
}

func (m *Model) handleNpFolderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.view = viewList
		m.np = newProjectModel{}
		return m, nil
	case "enter":
		dir := strings.TrimSpace(m.np.folder.Value())
		// Strip surrounding quotes — macOS Finder's "Copy as Pathname"
		// wraps paths in single quotes: '/Users/.../folder'
		dir = strings.Trim(dir, "'\"")
		if dir == "" {
			m.np.errMsg = "folder is required"
			return m, nil
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			m.np.errMsg = err.Error()
			return m, nil
		}
		m.np.folder.SetValue(abs)
		// Default the name to the folder basename.
		if m.np.name.Value() == "" {
			m.np.name.SetValue(filepath.Base(abs))
		}
		m.np.errMsg = ""
		m.np.step = npAgent
		// Default the agent to first known tool (claude).
		if len(config.KnownTools) > 0 && m.np.agent == "" {
			m.np.agent = config.KnownTools[0]
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.np.folder, cmd = m.np.folder.Update(msg)
	return m, cmd
}

func (m *Model) handleNpAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.view = viewList
		m.np = newProjectModel{}
		return m, nil
	case "enter":
		if m.np.agent == "" {
			m.np.errMsg = "select an agent"
			return m, nil
		}
		m.np.errMsg = ""
		m.np.step = npName
		m.np.name.Focus()
		return m, textinput.Blink
	case "1", "2", "3", "4":
		var n int
		fmt.Sscanf(msg.String(), "%d", &n)
		if n >= 1 && n <= len(config.KnownTools) {
			m.np.agent = config.KnownTools[n-1]
		}
		return m, nil
	case "up", "left", "h":
		i := indexOf(config.KnownTools, m.np.agent)
		if i > 0 {
			m.np.agent = config.KnownTools[i-1]
		}
		return m, nil
	case "down", "right", "l":
		i := indexOf(config.KnownTools, m.np.agent)
		if i >= 0 && i < len(config.KnownTools)-1 {
			m.np.agent = config.KnownTools[i+1]
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) handleNpNameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.view = viewList
		m.np = newProjectModel{}
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.np.name.Value())
		if name == "" {
			name = filepath.Base(m.np.folder.Value())
		}
		tool := m.np.agent
		dir := m.np.folder.Value()
		if err := m.inbox.AddProject(name, tool, dir); err != nil {
			m.np.errMsg = err.Error()
			return m, nil
		}
		// Success — return to list, position selection on the new project.
		snap := m.inbox.Snapshot()
		m.selected = len(snap)
		m.toast = fmt.Sprintf("added %s (%s)", name, tool)
		m.toastAt = time.Now()
		m.view = viewList
		m.np = newProjectModel{}
		return m, nil
	}
	var cmd tea.Cmd
	m.np.name, cmd = m.np.name.Update(msg)
	return m, cmd
}

func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}
