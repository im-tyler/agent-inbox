package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"

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
		footerText = "  ↑↓ navigate  enter detail  i inbox  n new  d delete  t tool  a attach  x cancel  tab chat"
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
	lines = append(lines, trunc.Render(headerStyle.Render("king")+mutedStyle.Render("  "+king.Tool)))
	lines = append(lines, "")

	for _, msg := range king.History {
		body := stripDirectives(msg.Content)
		if body == "" {
			// A turn that was nothing but directives has already been shown
			// as the dispatch it caused. An empty bubble adds nothing.
			continue
		}
		glyph, label, style := speaker(msg.Role, king.Tool)
		lines = append(lines, speakerLine(glyph, label, msg.Timestamp.Format(time.Kitchen), style, maxW))
		lines = append(lines, wrapBody(body, maxW)...)
		lines = append(lines, "")
	}

	// A turn in flight gets the same speaker line as a finished one, so the
	// reply appears to grow in place rather than jumping to a new shape when
	// it lands.
	if king.Status == driver.StatusWorking {
		lines = append(lines, speakerLine(m.frame(), king.Tool, workingLabel(king.Activity), workingStyle, maxW))
		if king.StreamingText != "" {
			lines = append(lines, wrapBody(king.StreamingText, maxW)...)
		}
	}

	return lines
}

// speaker maps a history role to how it is drawn. Roles that are not one of
// the four known kinds are fleet projects answering the king by name.
func speaker(role, tool string) (glyph, label string, style lipgloss.Style) {
	switch role {
	case "user":
		return "›", "you", userStyle
	case "assistant":
		return "●", tool, assistantStyle
	case "error":
		return "✗", "error", errorStyle
	case "system":
		return "·", "system", mutedStyle
	default:
		return "▸", role, fleetStyle
	}
}

// speakerLine draws "‹glyph› label ............ right", the right-hand text
// pushed to the far edge so the eye can ignore it.
func speakerLine(glyph, label, right string, style lipgloss.Style, width int) string {
	left := glyph + " " + label
	pad := width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return style.Render(left) + strings.Repeat(" ", pad) + mutedStyle.Render(right)
}

// working describes what the agent is doing right now, when the driver told
// us. "working" alone is still better than a blank line.
func workingLabel(activity string) string {
	if activity == "" {
		return "working"
	}
	return "working · " + activity
}

// wrapBody word-wraps message content and indents it under its speaker line.
func wrapBody(content string, width int) []string {
	inner := width - 2
	if inner < 10 {
		inner = 10
	}
	var out []string
	for _, ln := range strings.Split(content, "\n") {
		for _, w := range strings.Split(wordwrap.String(ln, inner), "\n") {
			out = append(out, "  "+w)
		}
	}
	return out
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
		isKing := i+1 == m.kingProjectIdx
		if !isKing {
			fleetCount++
		}
		// King gets a crown marker; fleet projects get number + cursor.
		var marker string
		if isKing {
			marker = "★ "
		} else if m.focusSidebar && i+1 == m.sidebarCursor {
			marker = "▶ "
		} else {
			marker = "  "
		}
		idxLabel := ""
		if !isKing {
			idxLabel = fmt.Sprintf("%d ", i+1)
		}
		// The status glyph is pinned to the right edge; the name takes
		// whatever is left rather than a fixed column that overflows.
		nameW := maxW - lipgloss.Width(marker+idxLabel) - 2
		if nameW < 4 {
			nameW = 4
		}
		name := truncateOneLine(p.Name, nameW)
		entry := fmt.Sprintf("%s%s%-*s %s", marker, idxLabel, nameW, name,
			statusGlyph(p.Status, m.frame()))
		if m.focusSidebar && i+1 == m.sidebarCursor && !isKing {
			lines = append(lines, trunc.Render(cursorStyle.Render(entry)))
		} else {
			lines = append(lines, trunc.Render(entry))
		}
		// A working project's activity says more than the message it is
		// replacing, which is by definition the previous turn's.
		sub := p.LastMessage
		if p.Status == driver.StatusWorking && p.Activity != "" {
			sub = p.Activity
		}
		if !isKing {
			if s := truncateOneLine(sub, maxW-4); s != "" {
				lines = append(lines, trunc.Render(mutedStyle.Render("  "+s)))
			}
		}
	}

	if fleetCount == 0 {
		lines = append(lines, trunc.Render(mutedStyle.Render("(no projects —")))
		lines = append(lines, trunc.Render(mutedStyle.Render(" tab, then i to")))
		lines = append(lines, trunc.Render(mutedStyle.Render(" add from the inbox)")))
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
		// Start animating now rather than on the next second-tick, so the
		// spinner appears in the same frame the message does.
		return m, m.startSpin()

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

	case "i":
		// The inbox: every session on the machine, and the way to take one on.
		return m, m.openInbox()

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
