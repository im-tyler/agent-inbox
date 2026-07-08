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
// sidebar (fleet status, left) + conversation (king history, right) + input.
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
	convW := m.width - sidebarW - 4 // borders + padding
	if convW < 20 {
		convW = 20
	}

	sidebar := m.renderSidebar(snap, sidebarW)
	conv := m.renderConversation(snap, convW)

	content := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, " ", conv)

	// Toast line (transient notifications).
	if m.toast != "" && time.Since(m.toastAt) < 6*time.Second {
		content += "\n" + wrapToast(m.toast, m.width-4)
	}

	footer := mutedStyle.Render("enter send  esc clear  ↑↓ scroll  : more  ctrl+c quit")
	return renderFrame(m.width, m.height, "agent-inbox", content, footer)
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

	// Apply scroll.
	availH := m.height - 7 // frame + title + input + footer + padding
	if availH < 3 {
		availH = 3
	}

	if len(lines) > availH {
		max := len(lines) - availH
		if m.mainScroll > max {
			m.mainScroll = max
		}
		if m.mainScroll < 0 {
			m.mainScroll = 0
		}
		start := m.mainScroll
		end := start + availH
		if end > len(lines) {
			end = len(lines)
		}
		lines = lines[start:end]
	} else {
		m.mainScroll = 0
	}

	for _, ln := range lines {
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

	// Input at the bottom.
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("> %s", m.mainInput.View()))

	return lipgloss.NewStyle().Width(width).Padding(0, 1).Render(b.String())
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
		// All non-king projects are connected by default.
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
		// Pin scroll to bottom on send.
		m.mainScroll = 999999
		return m, nil

	case "esc":
		m.mainInput.Reset()
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "pgup":
		m.mainScroll -= 10
		if m.mainScroll < 0 {
			m.mainScroll = 0
		}
		return m, nil

	case "pgdown", " ":
		m.mainScroll += 10
		return m, nil

	case "up":
		if m.mainScroll > 0 {
			m.mainScroll--
		}
		return m, nil

	case "down":
		m.mainScroll++
		return m, nil

	case ":":
		m.view = viewActions
		return m, nil

	case "q":
		// Only quit if input is empty (so users can type messages
		// containing 'q' without accidentally quitting).
		if m.mainInput.Value() == "" {
			return m, tea.Quit
		}
	}

	// Forward printable characters to the text input.
	var cmd tea.Cmd
	m.mainInput, cmd = m.mainInput.Update(msg)
	return m, cmd
}
