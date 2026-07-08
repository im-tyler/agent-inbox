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

// renderMain draws the split-pane king-first layout WITHOUT renderFrame.
// Every component is padded to exact line counts so there's no clipping
// or estimation. This is more verbose than renderFrame but deterministic.
func (m Model) renderMain() string {
	snap := m.inbox.Snapshot()

	// Layout heights (exact):
	//   top border      = 1
	//   title           = 1
	//   blank           = 1
	//   body            = bodyH (sidebar + conversation)
	//   blank           = 1
	//   input top       = 1
	//   input text      = 1
	//   input bottom    = 1
	//   hint            = 1
	//   bottom border   = 1
	//   total non-body  = 9
	bodyH := m.height - 9
	if bodyH < 3 {
		bodyH = 3
	}

	// Sidebar width.
	sidebarW := m.width / 4
	if sidebarW < 22 {
		sidebarW = 22
	}
	if sidebarW > 40 {
		sidebarW = 40
	}
	convW := m.width - sidebarW - 4
	if convW < 20 {
		convW = 20
	}

	// Build conversation lines.
	convLines := m.buildConversationLines(snap, convW)

	// Slice to show the last bodyH lines (bottom-relative scroll).
	scrollUp := m.mainScrollFromBottom
	if scrollUp < 0 {
		scrollUp = 0
	}
	endIdx := len(convLines) - scrollUp
	if endIdx > len(convLines) {
		endIdx = len(convLines)
	}
	if endIdx < 0 {
		endIdx = 0
	}
	startIdx := endIdx - bodyH
	if startIdx < 0 {
		startIdx = 0
	}
	visibleConv := convLines[startIdx:endIdx]

	// Pad conversation to exactly bodyH lines.
	for len(visibleConv) < bodyH {
		visibleConv = append(visibleConv, "")
	}

	// Build sidebar lines, padded to exactly bodyH.
	sidebarLines := m.buildSidebarLines(snap, sidebarW)
	for len(sidebarLines) < bodyH {
		sidebarLines = append(sidebarLines, "")
	}
	if len(sidebarLines) > bodyH {
		sidebarLines = sidebarLines[:bodyH]
	}

	// Join side by side, line by line.
	var body strings.Builder
	for i := 0; i < bodyH; i++ {
		convLine := ""
		if i < len(visibleConv) {
			convLine = visibleConv[i]
		}
		sidebarLine := ""
		if i < len(sidebarLines) {
			sidebarLine = sidebarLines[i]
		}
		body.WriteString(padToWidth(convLine, convW))
		body.WriteString("  ")
		body.WriteString(padToWidth(sidebarLine, sidebarW))
		body.WriteString("\n")
	}
	bodyStr := strings.TrimSuffix(body.String(), "\n")

	// Build input bar.
	inputText := m.mainInput.View()
	inputLine := fmt.Sprintf("  %s", inputText)

	// Build hint.
	hint := mutedStyle.Render("  enter send  esc clear  ↑↓ scroll  : more  ctrl+c quit")

	// Assemble the full frame with box-drawing characters.
	var b strings.Builder
	innerW := m.width - 2
	b.WriteString("╭" + strings.Repeat("─", innerW) + "╮\n")
	b.WriteString("│ " + titleStyle.Render("agent-inbox") + padRight("", innerW-12) + " │\n")
	b.WriteString("│" + strings.Repeat(" ", innerW+2) + "│\n")
	for _, ln := range strings.Split(bodyStr, "\n") {
		b.WriteString("│ " + padToWidth(ln, innerW) + " │\n")
	}
	b.WriteString("│" + strings.Repeat(" ", innerW+2) + "│\n")
	b.WriteString("│ " + padToWidth(inputLine, innerW) + " │\n")
	b.WriteString("│ " + padToWidth(hint, innerW) + " │\n")
	b.WriteString("╰" + strings.Repeat("─", innerW) + "╯")
	return b.String()
}

// buildConversationLines returns all conversation lines for the king project.
func (m Model) buildConversationLines(snap []inbox.Project, width int) []string {
	if m.kingProjectIdx < 1 || m.kingProjectIdx > len(snap) {
		return []string{"(no king project)"}
	}
	king := snap[m.kingProjectIdx-1]
	maxW := width - 2
	if maxW < 10 {
		maxW = 10
	}
	trunc := lipgloss.NewStyle().MaxWidth(maxW)

	var lines []string
	lines = append(lines, trunc.Render(headerStyle.Render(fmt.Sprintf("king (%s) %s",
		king.Tool, statusBadge(king.Status, king.Activity)))))
	lines = append(lines, "")

	for _, msg := range king.History {
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

	// Streaming text.
	if king.Status == driver.StatusWorking && king.StreamingText != "" {
		lines = append(lines, trunc.Render(workingStyle.Render("─ generating ─")))
		for _, ln := range strings.Split(king.StreamingText, "\n") {
			lines = append(lines, trunc.Render(ln))
		}
	}

	return lines
}

// buildSidebarLines returns the fleet sidebar as a slice of lines.
func (m Model) buildSidebarLines(snap []inbox.Project, width int) []string {
	maxW := width - 2
	if maxW < 10 {
		maxW = 10
	}
	trunc := lipgloss.NewStyle().MaxWidth(maxW)

	var lines []string
	lines = append(lines, trunc.Render(headerStyle.Render("fleet")))
	lines = append(lines, "")

	waiting, working, fleetCount := 0, 0, 0
	for _, p := range snap {
		switch p.Status {
		case driver.StatusWaiting, driver.StatusError:
			waiting++
		case driver.StatusWorking:
			working++
		}
	}

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
		lines = append(lines, trunc.Render(fmt.Sprintf("%d %-14s %s", i+1, name, badge)))
		msg := truncateOneLine(p.LastMessage, maxW-4)
		if msg != "" {
			lines = append(lines, trunc.Render(mutedStyle.Render("  "+msg)))
		}
	}

	if fleetCount == 0 {
		lines = append(lines, trunc.Render(mutedStyle.Render("(no projects —")))
		lines = append(lines, trunc.Render(mutedStyle.Render(" press : then n)")))
	}

	lines = append(lines, "")
	lines = append(lines, trunc.Render(mutedStyle.Render(fmt.Sprintf("%d projects  %d waiting", fleetCount, waiting))))
	lines = append(lines, trunc.Render(mutedStyle.Render(fmt.Sprintf("%d working", working))))
	return lines
}

// padToWidth ensures a string occupies exactly w visual columns by padding
// with spaces or truncating. Strips trailing newlines first.
func padToWidth(s string, w int) string {
	s = strings.TrimRight(s, "\n\r")
	// Use lipgloss to handle ANSI codes correctly.
	return lipgloss.NewStyle().Width(w).Render(s)
}

// padRight pads s with spaces to width w.
func padRight(s string, w int) string {
	vis := lipgloss.Width(s)
	if vis >= w {
		return s
	}
	return s + strings.Repeat(" ", w-vis)
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
