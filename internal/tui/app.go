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

	fleetCount := 0
	for i, p := range snap {
		if i+1 == m.kingProjectIdx {
			continue
		}
		fleetCount++
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

	if fleetCount == 0 {
		b.WriteString(mutedStyle.Render("(no projects — press :"))
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(" then n to add one)"))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(mutedStyle.Render(fmt.Sprintf("%d projects  %d waiting  %d working",
		fleetCount, waiting, working)))

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

	// Build ALL conversation lines (history + streaming text).
	lines := renderConversationMessages(king, width)
	if king.Status == driver.StatusWorking && king.StreamingText != "" {
		maxW := width - 2
		if maxW < 10 {
			maxW = 10
		}
		trunc := lipgloss.NewStyle().MaxWidth(maxW)
		lines = append(lines, trunc.Render(workingStyle.Render("─ generating ─")))
		for _, ln := range strings.Split(king.StreamingText, "\n") {
			lines = append(lines, trunc.Render(ln))
		}
		lines = append(lines, "")
	}

	// Available height for the conversation body.
	availH := m.height - 10
	if availH < 3 {
		availH = 3
	}

	// Slice the visible window. mainScrollFromBottom = 0 means "show the
	// last availH lines" (pinned to bottom). Higher values scroll up.
	// This is exact — no line-count estimation needed.
	endIdx := len(lines) - m.mainScrollFromBottom
	if endIdx < 0 {
		endIdx = 0
	}
	if endIdx > len(lines) {
		endIdx = len(lines)
	}
	startIdx := endIdx - availH
	if startIdx < 0 {
		startIdx = 0
	}

	for _, ln := range lines[startIdx:endIdx] {
		b.WriteString(ln)
		b.WriteString("\n")
	}

	return lipgloss.NewStyle().Width(width).Padding(0, 1).Render(b.String())
}

// mainMaxScroll and clampMainScroll removed — no longer needed.
// The bottom-relative scroll approach doesn't require line-count estimation.

// renderConversationMessages turns a project's History into display lines.
// Each line is truncated to maxW so that 1 slice entry = 1 visual terminal
// row. Without this, long lines wrap and the scroll count doesn't match
// what the terminal actually shows, causing the bottom to be clipped.
func renderConversationMessages(p inbox.Project, width int) []string {
	maxW := width - 2
	if maxW < 10 {
		maxW = 10
	}
	trunc := lipgloss.NewStyle().MaxWidth(maxW)

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
		lines = append(lines, trunc.Render(style.Render(fmt.Sprintf("[%s %s]", label, ts))))
		for _, ln := range strings.Split(msg.Content, "\n") {
			lines = append(lines, trunc.Render(ln))
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
		m.mainScrollFromBottom = 0
		return m, nil

	case "esc":
		m.mainInput.Reset()
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "pgup":
		m.mainAutoScroll = false
		m.mainScrollFromBottom += 10
		return m, nil

	case "pgdown":
		m.mainScrollFromBottom -= 10
		if m.mainScrollFromBottom <= 0 {
			m.mainScrollFromBottom = 0
			m.mainAutoScroll = true
		}
		return m, nil

	case "up":
		m.mainAutoScroll = false
		m.mainScrollFromBottom++
		return m, nil

	case "down":
		if m.mainScrollFromBottom > 0 {
			m.mainScrollFromBottom--
		}
		if m.mainScrollFromBottom == 0 {
			m.mainAutoScroll = true
		}
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
