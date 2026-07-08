package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/driver"
	"agentinbox/internal/inbox"
)

// renderMain draws the split-pane king-first layout:
// sidebar (fleet status, left) + conversation (king history, right)
// + fixed input bar at the bottom (always visible).
func (m Model) renderMain() string {
	snap := m.inbox.Snapshot()

	// Sidebar width: ~25% of terminal, clamped.
	sidebarW := m.width / 4
	if sidebarW < 22 {
		sidebarW = 22
	}
	if sidebarW > 40 {
		sidebarW = 40
	}

	// Conversation gets the rest.
	convW := m.width - sidebarW - 4
	if convW < 20 {
		convW = 20
	}

	sidebar := m.renderSidebar(snap, sidebarW)
	conv := m.renderConversation(snap, convW)

	// Body = conversation (left) + sidebar (right), side by side.
	content := lipgloss.JoinHorizontal(lipgloss.Top, conv, " ", sidebar)

	// Toast line (transient notifications).
	if m.toast != "" && time.Since(m.toastAt) < 6*time.Second {
		content += "\n" + wrapToast(m.toast, m.width-4)
	}

	// Input bar — fixed at the bottom, full width, visually distinct.
	// This goes in the "footer" position of renderFrame so it's always
	// visible regardless of conversation scroll position.
	inputBar := m.renderInputBar()

	return renderFrame(m.width, m.height, "agent-inbox", content, inputBar)
}

// renderInputBar draws the always-visible input box with a distinct border.
func (m Model) renderInputBar() string {
	// Determine king status for the label.
	snap := m.inbox.Snapshot()
	statusLabel := ""
	if m.kingProjectIdx >= 1 && m.kingProjectIdx <= len(snap) {
		king := snap[m.kingProjectIdx-1]
		if king.Status == driver.StatusWorking {
			statusLabel = mutedStyle.Render(fmt.Sprintf(" (king is %s...)", king.Activity))
		}
	}

	// The input box itself — rounded border, blue accent, padded.
	inputContent := m.mainInput.View()
	inputBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")). // blue accent
		Padding(0, 1).
		Width(m.width - 6).
		Render(inputContent)

	// Below the input box: keybindings hint.
	hint := mutedStyle.Render("enter send  esc clear  ↑↓ scroll  : more  ctrl+c quit")

	// Stack: input box + hint.
	return lipgloss.JoinVertical(lipgloss.Left, inputBox, hint) + statusLabel
}

func (m Model) renderSidebar(snap []inbox.Project, width int) string {
	var b strings.Builder

	waiting := 0
	working := 0
	for _, p := range snap {
		switch p.Status {
		case driver.StatusWaiting, driver.StatusError:
			waiting++
		case driver.StatusWorking:
			working++
		}
	}

	b.WriteString(headerStyle.Render("fleet"))
	b.WriteString("\n\n")

	for i, p := range snap {
		if i+1 == m.kingProjectIdx {
			continue // skip the king itself
		}
		badge := statusBadge(p.Status, p.Activity)
		name := p.Name
		if len(name) > 14 {
			name = name[:13] + "…"
		}
		b.WriteString(fmt.Sprintf("%d %-14s %s\n", i+1, name, badge))
		msg := truncateOneLine(p.LastMessage, width-6)
		if msg == "" && p.LastErr != "" {
			msg = mutedStyle.Render("err: " + truncateOneLine(p.LastErr, width-10))
		} else if msg != "" {
			b.WriteString(mutedStyle.Render("  " + msg))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(mutedStyle.Render(fmt.Sprintf("%d projects  %d waiting  %d working",
		len(snap)-1, waiting, working)))

	return lipgloss.NewStyle().Width(width).Padding(0, 1).Render(b.String())
}

func (m Model) renderConversation(snap []inbox.Project, width int) string {
	if m.kingProjectIdx < 1 || m.kingProjectIdx > len(snap) {
		return lipgloss.NewStyle().Width(width).Render("(no king project)")
	}
	king := snap[m.kingProjectIdx-1]

	var b strings.Builder

	b.WriteString(headerStyle.Render(fmt.Sprintf("king (%s) %s",
		king.Tool, statusBadge(king.Status, king.Activity))))
	b.WriteString("\n\n")

	// Render king history as a conversation.
	lines := renderConversationMessages(king, width)

	// Available height for the conversation body.
	availH := m.height - 10
	if availH < 3 {
		availH = 3
	}

	// Apply scroll (clamping is done in handleMainKey/tick — here we
	// just read the current offset). Defensive clamp only.
	scroll := m.mainScroll
	maxScroll := len(lines) - availH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}

	start := scroll
	end := start + availH
	if end > len(lines) {
		end = len(lines)
	}
	for _, ln := range lines[start:end] {
		b.WriteString(ln)
		b.WriteString("\n")
	}

	// Streaming text (if king is working).
	if king.Status == driver.StatusWorking && king.StreamingText != "" {
		b.WriteString(workingStyle.Render("─ generating ─"))
		b.WriteString("\n")
		b.WriteString(indent(truncateOneLine(king.StreamingText, width-2), "  "))
		b.WriteString("\n")
	}

	return lipgloss.NewStyle().Width(width).Padding(0, 1).Render(b.String())
}

// mainMaxScroll estimates the maximum scroll offset for the king conversation.
func (m Model) mainMaxScroll() int {
	snap := m.inbox.Snapshot()
	if m.kingProjectIdx < 1 || m.kingProjectIdx > len(snap) {
		return 0
	}
	king := snap[m.kingProjectIdx-1]
	lines := 0
	for _, msg := range king.History {
		lines += 2 + strings.Count(msg.Content, "\n") + 1
	}
	availH := m.height - 10
	if availH < 1 {
		availH = 1
	}
	max := lines - availH
	if max < 0 {
		return 0
	}
	return max
}

// clampMainScroll ensures scroll is within bounds.
func (m *Model) clampMainScroll() {
	max := m.mainMaxScroll()
	if m.mainScroll > max {
		m.mainScroll = max
	}
	if m.mainScroll < 0 {
		m.mainScroll = 0
	}
}

// renderConversationMessages turns a project's History into display lines.
func renderConversationMessages(p inbox.Project, width int) []string {
	var lines []string
	for _, msg := range p.History {
		label := msg.Role
		style := mutedStyle
		switch msg.Role {
		case "user":
			label = "you"
			style = workingStyle
		case "assistant":
			label = p.Tool
			style = waitingStyle
		case "error":
			label = "error"
			style = errorStyle
		case "system":
			label = "system"
			style = mutedStyle
		}
		ts := msg.Timestamp.Format(time.Kitchen)
		lines = append(lines, style.Render(fmt.Sprintf("[%s %s]", label, ts)))
		// Word-wrap content to width.
		for _, ln := range strings.Split(msg.Content, "\n") {
			lines = append(lines, ln)
		}
		lines = append(lines, "")
	}
	return lines
}

// handleMainKey processes keys in the king-first main view.
// The text input is always focused; Enter sends to the king,
// Esc clears, arrow keys scroll the conversation.
func (m Model) handleMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(m.mainInput.Value())
		if text == "" {
			return m, nil
		}
		m.mainInput.Reset()
		snap := m.inbox.Snapshot()
		var connected []string
		for i, p := range snap {
			if i+1 != m.kingProjectIdx {
				connected = append(connected, p.Name)
			}
		}
		err := m.inbox.KingSend(m.kingProjectIdx, text, connected)
		if err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
		} else {
			m.toast = "sent to king"
			m.toastAt = time.Now()
		}
		// Jump to bottom on send.
		m.mainAutoScroll = true
		m.mainScroll = m.mainMaxScroll()
		return m, nil

	case "esc":
		m.mainInput.Reset()
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "pgup":
		m.mainAutoScroll = false
		m.mainScroll -= 10
		m.clampMainScroll()
		return m, nil

	case "pgdown", " ":
		m.mainScroll += 10
		if m.mainScroll >= m.mainMaxScroll() {
			m.mainAutoScroll = true
		}
		m.clampMainScroll()
		return m, nil

	case "up":
		m.mainAutoScroll = false
		if m.mainScroll > 0 {
			m.mainScroll--
		}
		return m, nil

	case "down":
		m.mainScroll++
		if m.mainScroll >= m.mainMaxScroll() {
			m.mainAutoScroll = true
		}
		m.clampMainScroll()
		return m, nil

	case ":":
		m.view = viewActions
		return m, nil

	case "q":
		if m.mainInput.Value() == "" {
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.mainInput, cmd = m.mainInput.Update(msg)
	return m, cmd
}
