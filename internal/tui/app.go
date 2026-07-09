package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentinbox/internal/driver"
	"agentinbox/internal/inbox"
)

// renderMain draws the split-pane king-first layout with EXACT width/height
// math. Every interior line is padded to exactly (terminalWidth - 4)
// characters so nothing wraps. bodyH = terminalHeight - 7 (the exact
// count of non-body lines: top border, title, blank, blank, input, hint,
// bottom border).
func (m Model) renderMain() string {
	snap := m.inbox.Snapshot()
	W := m.width
	H := m.height

	// Content width inside the frame: │ content │
	// │ = 1, space = 1, content = W-4, space = 1, │ = 1 → total W
	contentW := W - 4
	if contentW < 20 {
		contentW = 20
	}

	// Body height: 7 non-body lines (top, title, blank, blank, input, hint, bottom)
	bodyH := H - 7
	if bodyH < 3 {
		bodyH = 3
	}

	// Sidebar width: ~25% of content, clamped.
	sidebarW := contentW / 4
	if sidebarW < 20 {
		sidebarW = 20
	}
	if sidebarW > 35 {
		sidebarW = 35
	}
	// Conversation gets the rest minus the 2-char separator.
	convW := contentW - sidebarW - 2
	if convW < 20 {
		convW = 20
	}

	// Build conversation lines.
	convLines := m.buildConversationLines(snap, convW)

	// Slice to show the last bodyH lines (bottom-relative scroll).
	endIdx := len(convLines) - m.mainScrollFromBottom
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

	// Join side by side, line by line, padded to exact widths.
	var bodyLines []string
	for i := 0; i < bodyH; i++ {
		convLn := ""
		if i < len(visibleConv) {
			convLn = visibleConv[i]
		}
		sidebarLn := ""
		if i < len(sidebarLines) {
			sidebarLn = sidebarLines[i]
		}
		line := clampWidth(convLn, convW) + "  " + clampWidth(sidebarLn, sidebarW)
		bodyLines = append(bodyLines, clampWidth(line, contentW))
	}

	// Build input line.
	inputText := m.mainInput.View()
	inputPrefix := "  "
	if m.focusSidebar {
		inputPrefix = "  " + mutedStyle.Render("(chat not focused — press Tab) ")
	}
	inputLine := clampWidth(inputPrefix+inputText, contentW)

	// Contextual footer based on focus.
	var footerText string
	if m.focusSidebar {
		footerText = "  ↑↓ navigate  enter detail  n new  d delete  t tool  a attach  x cancel  tab chat"
	} else if m.helpMode {
		footerText = "  ? close help"
	} else {
		footerText = "  enter send  tab fleet  ↑↓ scroll  ? help  ctrl+c quit"
	}
	hintLine := clampWidth(mutedStyle.Render(footerText), contentW)

	// Assemble the frame.
	var b strings.Builder
	dash := strings.Repeat("─", contentW+2)
	b.WriteString("╭" + dash + "╮\n")
	b.WriteString("│ " + clampWidth(titleStyle.Render("agent-inbox"), contentW) + " │\n")
	b.WriteString("│" + strings.Repeat(" ", contentW+2) + "│\n")
	for _, ln := range bodyLines {
		b.WriteString("│ " + ln + " │\n")
	}
	b.WriteString("│" + strings.Repeat(" ", contentW+2) + "│\n")
	b.WriteString("│ " + inputLine + " │\n")
	b.WriteString("│ " + hintLine + " │\n")
	b.WriteString("╰" + dash + "╯")
	return b.String()
}

// clampWidth ensures a string occupies EXACTLY w visual columns by
// truncating (with …) or padding with spaces. Handles ANSI codes.
func clampWidth(s string, w int) string {
	s = strings.TrimRight(s, "\n\r")
	if w < 1 {
		return ""
	}
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Render(s)
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

	if king.Status == driver.StatusWorking && king.StreamingText != "" {
		lines = append(lines, trunc.Render(workingStyle.Render("─ generating ─")))
		for _, ln := range strings.Split(king.StreamingText, "\n") {
			lines = append(lines, trunc.Render(ln))
		}
	}

	return lines
}

// buildSidebarLines returns the fleet sidebar as a slice of lines.
// When focusSidebar is true, the selected project is highlighted.
func (m Model) buildSidebarLines(snap []inbox.Project, width int) []string {
	maxW := width - 2
	if maxW < 10 {
		maxW = 10
	}
	trunc := lipgloss.NewStyle().MaxWidth(maxW)
	cursorStyle := lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("15"))

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
		// Cursor marker when sidebar is focused and this is the selected project.
		marker := "  "
		if m.focusSidebar && i+1 == m.sidebarCursor {
			marker = "▶ "
		}
		entry := fmt.Sprintf("%s%d %-14s %s", marker, i+1, name, badge)
		if m.focusSidebar && i+1 == m.sidebarCursor {
			lines = append(lines, trunc.Render(cursorStyle.Render(entry)))
		} else {
			lines = append(lines, trunc.Render(entry))
		}
		msg := truncateOneLine(p.LastMessage, maxW-4)
		if msg != "" {
			lines = append(lines, trunc.Render(mutedStyle.Render("  "+msg)))
		}
	}

	if fleetCount == 0 {
		lines = append(lines, trunc.Render(mutedStyle.Render("(no projects —")))
		lines = append(lines, trunc.Render(mutedStyle.Render(" press n to add)")))
	}

	lines = append(lines, "")
	lines = append(lines, trunc.Render(mutedStyle.Render(fmt.Sprintf("%d projects  %d waiting", fleetCount, waiting))))
	lines = append(lines, trunc.Render(mutedStyle.Render(fmt.Sprintf("%d working", working))))
	return lines
}

// handleMainKey processes keys in the king-first main view.
func (m Model) handleMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// If sidebar is focused, route to sidebar handler.
	if m.focusSidebar {
		return m.handleSidebarKey(msg)
	}

	// Chat-focused keys.
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
		}
		m.mainAutoScroll = true
		m.mainScrollFromBottom = 0
		return m, nil

	case "tab":
		// Focus the sidebar.
		m.focusSidebar = true
		m.mainInput.Blur()
		// Ensure sidebarCursor points to a valid non-king project.
		snap := m.inbox.Snapshot()
		if m.sidebarCursor < 1 || m.sidebarCursor > len(snap) || m.sidebarCursor == m.kingProjectIdx {
			for i := 1; i <= len(snap); i++ {
				if i != m.kingProjectIdx {
					m.sidebarCursor = i
					break
				}
			}
		}
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

	case "?":
		// Help overlay — only when input is empty.
		if m.mainInput.Value() == "" {
			m.helpMode = !m.helpMode
			return m, nil
		}
	}

	// Forward printable characters to the text input.
	var cmd tea.Cmd
	m.mainInput, cmd = m.mainInput.Update(msg)
	return m, cmd
}

// handleSidebarKey processes keys when the sidebar (fleet list) is focused.
func (m Model) handleSidebarKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	snap := m.inbox.Snapshot()

	switch msg.String() {
	case "tab", "esc":
		// Return focus to chat.
		m.focusSidebar = false
		m.mainInput.Focus()
		return m, textinput.Blink

	case "ctrl+c":
		return m, tea.Quit

	case "j", "down":
		// Next non-king project.
		for i := m.sidebarCursor + 1; i <= len(snap); i++ {
			if i != m.kingProjectIdx {
				m.sidebarCursor = i
				break
			}
		}
		return m, nil

	case "k", "up":
		// Previous non-king project.
		for i := m.sidebarCursor - 1; i >= 1; i-- {
			if i != m.kingProjectIdx {
				m.sidebarCursor = i
				break
			}
		}
		return m, nil

	case "n":
		// New project modal.
		m.focusSidebar = false
		m.mainInput.Focus()
		m.view = viewNewProject
		cwd, _ := os.Getwd()
		m.np = newProjectModelInitial(cwd)
		m.np.folder.Focus()
		return m, textinput.Blink

	case "d":
		// Delete selected project.
		if m.sidebarCursor >= 1 && m.sidebarCursor <= len(snap) {
			if snap[m.sidebarCursor-1].Status == driver.StatusWorking {
				m.toast = "cancel the send first (x)"
				m.toastAt = time.Now()
				return m, nil
			}
		}
		m.selected = m.sidebarCursor
		m.view = viewDeleteConfirm
		return m, nil

	case "t":
		// Change tool for selected project.
		if m.sidebarCursor >= 1 && m.sidebarCursor <= len(snap) {
			if snap[m.sidebarCursor-1].Status == driver.StatusWorking {
				m.toast = "cancel the send first (x)"
				m.toastAt = time.Now()
				return m, nil
			}
		}
		m.selected = m.sidebarCursor
		m.pendingTool = ""
		m.view = viewToolPicker
		return m, nil

	case "a":
		// Attach to selected project.
		args, dir, err := m.inbox.AttachArgs(m.sidebarCursor)
		if err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
			return m, nil
		}
		m.attachRequest = &attachArgs{Argv: args, Dir: dir}
		return m, tea.Quit

	case "x":
		// Cancel selected project's send.
		if err := m.inbox.Cancel(m.sidebarCursor); err != nil {
			m.toast = err.Error()
		} else {
			m.toast = "cancelled"
		}
		m.toastAt = time.Now()
		return m, nil

	case "enter":
		// Drill into project detail — use the existing viewDetail.
		if m.sidebarCursor >= 1 && m.sidebarCursor <= len(snap) {
			m.selected = m.sidebarCursor
			m.view = viewDetail
			m.detailScroll = m.detailMaxScroll()
		}
		return m, nil
	}

	return m, nil
}
