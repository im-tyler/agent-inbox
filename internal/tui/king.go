package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/driver"
	"agentinbox/internal/inbox"
)

// King view state on the Model:
//   kingIdx           — 1-based index of the project acting as king
//   connected         — names of projects the king supervises
//   kingSendMode      — when true, kingInput is active for typing a message
//   kingInput         — text input for the king's send prompt
//   kingAddMode       — when true, showing the "add connected" picker
//   kingRemoveMode    — when true, showing the "remove connected" picker

func (m *Model) renderKing() string {
	snap := m.inbox.Snapshot()
	if m.kingIdx < 1 || m.kingIdx > len(snap) {
		m.view = viewList
		return m.viewList()
	}
	king := snap[m.kingIdx-1]

	var b strings.Builder

	// Connected projects.
	b.WriteString(workingStyle.Render("connected:"))
	b.WriteString("\n")
	if len(m.connected) == 0 {
		b.WriteString(mutedStyle.Render("  (none — press + to add)"))
		b.WriteString("\n")
	} else {
		connSet := make(map[string]bool, len(m.connected))
		for _, n := range m.connected {
			connSet[n] = true
		}
		for _, p := range snap {
			if !connSet[p.Name] {
				continue
			}
			badge := statusBadge(p.Status, p.Activity)
			msg := truncateOneLine(p.LastMessage, 50)
			if msg == "" && p.LastErr != "" {
				msg = "err: " + truncateOneLine(p.LastErr, 40)
			}
			b.WriteString(fmt.Sprintf("  %-16s %-8s %s %s\n",
				p.Name, p.Tool, badge, mutedStyle.Render(msg)))
		}
	}

	// Conversation.
	b.WriteString("\n")
	b.WriteString(workingStyle.Render("conversation:"))
	b.WriteString("\n")
	if len(king.History) == 0 {
		b.WriteString(mutedStyle.Render("  (no messages — press s to send)"))
		b.WriteString("\n")
	} else {
		start := 0
		const show = 6
		if len(king.History) > show {
			start = len(king.History) - show
		}
		for _, msg := range king.History[start:] {
			label := msg.Role
			style := mutedStyle
			switch msg.Role {
			case "user":
				label = "you"
				style = workingStyle
			case "assistant":
				label = king.Tool
				style = waitingStyle
			case "error":
				label = "error"
				style = errorStyle
			}
			ts := msg.Timestamp.Format(time.Kitchen)
			b.WriteString(style.Render(fmt.Sprintf("  [%s %s]", label, ts)))
			b.WriteString("\n")
			b.WriteString(indent(truncateOneLine(msg.Content, 200), "    "))
			b.WriteString("\n")
		}
	}

	// Title and footer.
	statusStr := string(king.Status)
	if king.Activity != "" {
		statusStr += ":" + king.Activity
	}
	title := fmt.Sprintf("king: %s (%s)  [%s]", king.Name, king.Tool, statusStr)

	var footer string
	if m.kingSendMode {
		footer = fmt.Sprintf("send to king: %s\nenter to send  esc to cancel", m.kingInput.View())
	} else if m.kingAddMode {
		footer = m.renderKingAddPicker(snap)
	} else if m.kingRemoveMode {
		footer = m.renderKingRemovePicker()
	} else {
		footer = mutedStyle.Render("s send  + add  - remove  x cancel  esc back  q quit")
	}

	return renderFrame(m.width, m.height, title, b.String(), footer)
}

func (m *Model) renderKingAddPicker(snap []inbox.Project) string {
	var b strings.Builder
	b.WriteString("  add connected project:\n")
	connSet := make(map[string]bool, len(m.connected))
	for _, n := range m.connected {
		connSet[n] = true
	}
	idx := 0
	for i, p := range snap {
		if i+1 == m.kingIdx {
			continue // skip king itself
		}
		if connSet[p.Name] {
			continue // already connected
		}
		idx++
		b.WriteString(fmt.Sprintf("    [%d] %s (%s)\n", idx, p.Name, p.Tool))
	}
	if idx == 0 {
		b.WriteString(mutedStyle.Render("    (all projects already connected)"))
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("  type the name and press enter  esc to cancel"))
	return b.String()
}

func (m *Model) renderKingRemovePicker() string {
	var b strings.Builder
	b.WriteString("  remove connected project:\n")
	if len(m.connected) == 0 {
		b.WriteString(mutedStyle.Render("    (none connected)"))
	} else {
		for i, n := range m.connected {
			b.WriteString(fmt.Sprintf("    [%d] %s\n", i+1, n))
		}
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("  type the name or number and press enter  esc to cancel"))
	return b.String()
}

func (m *Model) handleKingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Sub-modes take priority.
	if m.kingSendMode {
		return m.handleKingSendKey(msg)
	}
	if m.kingAddMode {
		return m.handleKingAddKey(msg)
	}
	if m.kingRemoveMode {
		return m.handleKingRemoveKey(msg)
	}

	switch msg.String() {
	case "esc", "q":
		m.view = viewList
		return m, nil

	case "s":
		snap := m.inbox.Snapshot()
		if m.kingIdx < 1 || m.kingIdx > len(snap) {
			return m, nil
		}
		if snap[m.kingIdx-1].Status == driver.StatusWorking {
			m.toast = "king is already working — wait or press x to cancel"
			m.toastAt = time.Now()
			return m, nil
		}
		m.kingSendMode = true
		m.kingInput.Focus()
		return m, textinput.Blink

	case "+":
		m.kingAddMode = true
		m.kingInput.Reset()
		m.kingInput.Placeholder = "project name"
		m.kingInput.Focus()
		return m, textinput.Blink

	case "-":
		if len(m.connected) == 0 {
			m.toast = "no connected projects to remove"
			m.toastAt = time.Now()
			return m, nil
		}
		m.kingRemoveMode = true
		m.kingInput.Reset()
		m.kingInput.Placeholder = "name or number"
		m.kingInput.Focus()
		return m, textinput.Blink

	case "x":
		// Cancel king's in-flight send.
		if err := m.inbox.Cancel(m.kingIdx); err != nil {
			m.toast = err.Error()
		} else {
			m.toast = "cancelled king"
		}
		m.toastAt = time.Now()
	}

	return m, nil
}

func (m *Model) handleKingSendKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(m.kingInput.Value())
		m.kingSendMode = false
		m.kingInput.Blur()
		m.kingInput.Reset()
		if text == "" {
			return m, nil
		}
		err := m.inbox.KingSend(m.kingIdx, text, m.connected)
		if err != nil {
			m.toast = err.Error()
		} else {
			m.toast = "sent to king — will dispatch to connected projects"
		}
		m.toastAt = time.Now()
		return m, nil

	case "esc":
		m.kingSendMode = false
		m.kingInput.Blur()
		m.kingInput.Reset()
		return m, nil
	}

	var cmd tea.Cmd
	m.kingInput, cmd = m.kingInput.Update(msg)
	return m, cmd
}

func (m *Model) handleKingAddKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		input := strings.TrimSpace(m.kingInput.Value())
		m.kingAddMode = false
		m.kingInput.Blur()
		m.kingInput.Reset()
		m.kingInput.Placeholder = "message"

		if input == "" {
			return m, nil
		}
		// Try number first, then name.
		snap := m.inbox.Snapshot()
		connSet := make(map[string]bool, len(m.connected))
		for _, n := range m.connected {
			connSet[n] = true
		}
		var candidates []string
		for i, p := range snap {
			if i+1 == m.kingIdx || connSet[p.Name] {
				continue
			}
			candidates = append(candidates, p.Name)
		}
		var name string
		var n int
		if _, err := fmt.Sscanf(input, "%d", &n); err == nil && n >= 1 && n <= len(candidates) {
			name = candidates[n-1]
		} else {
			for _, c := range candidates {
				if strings.EqualFold(c, input) {
					name = c
					break
				}
			}
		}
		if name == "" {
			m.toast = fmt.Sprintf("no unconnected project matching %q", input)
		} else {
			m.connected = append(m.connected, name)
			m.toast = "connected " + name
		}
		m.toastAt = time.Now()
		return m, nil

	case "esc":
		m.kingAddMode = false
		m.kingInput.Blur()
		m.kingInput.Reset()
		m.kingInput.Placeholder = "message"
		return m, nil
	}

	var cmd tea.Cmd
	m.kingInput, cmd = m.kingInput.Update(msg)
	return m, cmd
}

func (m *Model) handleKingRemoveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		input := strings.TrimSpace(m.kingInput.Value())
		m.kingRemoveMode = false
		m.kingInput.Blur()
		m.kingInput.Reset()
		m.kingInput.Placeholder = "message"

		if input == "" || len(m.connected) == 0 {
			return m, nil
		}
		var idx int = -1
		var n int
		if _, err := fmt.Sscanf(input, "%d", &n); err == nil && n >= 1 && n <= len(m.connected) {
			idx = n - 1
		} else {
			for i, c := range m.connected {
				if strings.EqualFold(c, input) {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			m.toast = fmt.Sprintf("no connected project matching %q", input)
		} else {
			removed := m.connected[idx]
			m.connected = append(m.connected[:idx], m.connected[idx+1:]...)
			m.toast = "removed " + removed
		}
		m.toastAt = time.Now()
		return m, nil

	case "esc":
		m.kingRemoveMode = false
		m.kingInput.Blur()
		m.kingInput.Reset()
		m.kingInput.Placeholder = "message"
		return m, nil
	}

	var cmd tea.Cmd
	m.kingInput, cmd = m.kingInput.Update(msg)
	return m, cmd
}

// (Removed the bad type alias — we import inbox.Project directly.)
