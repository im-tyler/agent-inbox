// Package tui is the Bubble Tea dashboard for agent-inbox.
//
// It replaces the basic REPL with a single-screen view of all federated
// projects: status, last message, age, with keyboard navigation and an
// inline send prompt. The underlying inbox state model is unchanged.
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

// viewMode controls which screen the TUI is rendering.
type viewMode int

const (
	viewList viewMode = iota
	viewDetail
)

// Model is the Bubble Tea model for the agent-inbox dashboard.
type Model struct {
	inbox     *inbox.Inbox
	eventsDir string

	view      viewMode
	selected  int  // 1-based, matches existing convention
	sendMode  bool // when true, sendInput is active for the selected project
	helpMode  bool // when true, keybindings overlay is shown
	sendInput textinput.Model

	toast   string
	toastAt time.Time

	// attachRequest, when non-nil, signals the program should exit so
	// main.go can run the interactive attach command. main.go then
	// re-launches the TUI.
	attachRequest *attachArgs

	width  int
	height int
}

// attachArgs describes a pending interactive attach request.
type attachArgs struct {
	Argv []string
	Dir  string
}

// New constructs a Model bound to the given inbox.
func New(in *inbox.Inbox, eventsDir string) Model {
	ti := textinput.New()
	ti.Placeholder = "message"
	ti.CharLimit = 0
	ti.Width = 60

	return Model{
		inbox:     in,
		eventsDir: eventsDir,
		view:      viewList,
		selected:  1,
		sendInput: ti,
	}
}

// AttachRequest returns the pending attach command, if any. main.go
// inspects this after Run() returns.
func (m Model) AttachRequest() *attachArgs {
	return m.attachRequest
}

// tickMsg is emitted once per second to drive live updates.
type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Init starts the per-second ticker.
func (m Model) Init() tea.Cmd {
	return tick()
}

// Update handles all messages: key presses, window resize, ticks.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sendInput.Width = max(60, msg.Width-30)
		return m, nil

	case tickMsg:
		if upd := m.inbox.Ingest(m.eventsDir); len(upd) > 0 {
			m.toast = fmt.Sprintf("waiting: %s", strings.Join(upd, ", "))
			m.toastAt = time.Now()
		}
		return m, tick()

	case tea.KeyMsg:
		if m.sendMode {
			return m.handleSendKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global keys (work in any view) unless we're typing into send input.
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	}

	switch m.view {
	case viewList:
		return m.handleListKey(msg)
	case viewDetail:
		return m.handleDetailKey(msg)
	}
	return m, nil
}

func (m Model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit

	case "?":
		m.helpMode = !m.helpMode

	case "up", "k":
		if m.selected > 1 {
			m.selected--
		}
	case "down", "j":
		snap := m.inbox.Snapshot()
		if m.selected < len(snap) {
			m.selected++
		}

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		var n int
		fmt.Sscanf(msg.String(), "%d", &n)
		snap := m.inbox.Snapshot()
		if n >= 1 && n <= len(snap) {
			m.selected = n
		}

	case "s":
		// Enter send mode for the selected project.
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			return m, nil
		}
		if snap[m.selected-1].Status == driver.StatusWorking {
			m.toast = fmt.Sprintf("%s is already working", snap[m.selected-1].Name)
			m.toastAt = time.Now()
			return m, nil
		}
		m.sendMode = true
		m.sendInput.Focus()
		return m, textinput.Blink

	case "v", "enter":
		// Switch to full-screen detail view.
		snap := m.inbox.Snapshot()
		if m.selected >= 1 && m.selected <= len(snap) {
			m.view = viewDetail
		}

	case "a":
		// Interactive attach: request the argv from inbox, then exit.
		args, dir, err := m.inbox.AttachArgs(m.selected)
		if err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
			return m, nil
		}
		m.attachRequest = &attachArgs{Argv: args, Dir: dir}
		return m, tea.Quit

	case "r":
		m.toast = "refreshed"
		m.toastAt = time.Now()
	}

	return m, nil
}

func (m Model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "back":
		m.view = viewList

	case "a":
		// Attach from detail view too.
		args, dir, err := m.inbox.AttachArgs(m.selected)
		if err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
			return m, nil
		}
		m.attachRequest = &attachArgs{Argv: args, Dir: dir}
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) handleSendKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		text := m.sendInput.Value()
		if text == "" {
			m.sendMode = false
			m.sendInput.Blur()
			return m, nil
		}
		idx := m.selected
		m.sendMode = false
		m.sendInput.Blur()
		m.sendInput.Reset()
		err := m.inbox.Send(idx, text)
		if err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
		} else {
			snap := m.inbox.Snapshot()
			if idx >= 1 && idx <= len(snap) {
				m.toast = "sent to " + snap[idx-1].Name
				m.toastAt = time.Now()
			}
		}
		return m, nil

	case "esc":
		m.sendMode = false
		m.sendInput.Blur()
		m.sendInput.Reset()
		return m, nil
	}

	var cmd tea.Cmd
	m.sendInput, cmd = m.sendInput.Update(msg)
	return m, cmd
}

// View renders the dashboard.
func (m Model) View() string {
	if m.width == 0 {
		return "starting..."
	}

	switch m.view {
	case viewDetail:
		return m.viewDetail()
	default:
		return m.viewList()
	}
}

func (m Model) viewList() string {
	snap := m.inbox.Snapshot()
	waiting := m.inbox.WaitingCount()

	var b strings.Builder

	header := headerStyle.Render(fmt.Sprintf(
		"agent-inbox  %d projects  %d waiting",
		len(snap), waiting,
	))
	b.WriteString(header)
	b.WriteByte('\n')
	b.WriteByte('\n')

	if len(snap) == 0 {
		b.WriteString(mutedStyle.Render("  no projects configured — edit config.json"))
		b.WriteByte('\n')
	} else {
		for i, p := range snap {
			row := renderRow(i+1, p, m.selected == i+1)
			b.WriteString(row)
			b.WriteByte('\n')
		}
	}

	if m.toast != "" && time.Since(m.toastAt) < 6*time.Second {
		b.WriteByte('\n')
		b.WriteString(wrapToast(m.toast, m.width))
		b.WriteByte('\n')
	}

	b.WriteString("\n\n")
	b.WriteString(m.footer())
	b.WriteByte('\n')

	return b.String()
}

func (m Model) viewDetail() string {
	snap := m.inbox.Snapshot()
	if m.selected < 1 || m.selected > len(snap) {
		m.view = viewList
		return m.viewList()
	}
	p := snap[m.selected-1]

	var b strings.Builder

	// Header: project name + tool.
	b.WriteString(headerStyle.Render(fmt.Sprintf("%s  (%s)", p.Name, p.Tool)))
	b.WriteString(statusStyle(p.Status, "  ["+string(p.Status)+"]"))
	b.WriteByte('\n')
	b.WriteByte('\n')

	// Metadata block.
	rows := [][2]string{
		{"dir", p.Dir},
		{"session", shortSession(p.SessionID)},
		{"updated", fmt.Sprintf("%s (%s ago)", p.UpdatedAt.Format(time.RFC3339), ageHuman(time.Since(p.UpdatedAt)))},
		{"turns", fmt.Sprintf("%d", len(p.History))},
	}
	for _, r := range rows {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  %-10s", r[0])))
		b.WriteString(r[1])
		b.WriteByte('\n')
	}

	// History (most-recent-last; visually reads top→bottom like a chat).
	b.WriteByte('\n')
	if len(p.History) == 0 {
		b.WriteString(mutedStyle.Render("  (no messages yet — press 's' to send one)"))
		b.WriteByte('\n')
	} else {
		b.WriteString(workingStyle.Render("  history:"))
		b.WriteByte('\n')
		// Show the most recent ~8 turns to fit a typical viewport. Future
		// enhancement: scrollable viewport. For now we trim from the front.
		start := 0
		const show = 8
		if len(p.History) > show {
			start = len(p.History) - show
			b.WriteString(mutedStyle.Render(
				fmt.Sprintf("    …(%d earlier turns not shown)…\n", start),
			))
		}
		for _, msg := range p.History[start:] {
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
			}
			ts := msg.Timestamp.Format(time.Kitchen)
			b.WriteString(style.Render(fmt.Sprintf("  [%s %s]", label, ts)))
			b.WriteByte('\n')
			b.WriteString(indent(msg.Content, "    "))
			b.WriteByte('\n')
		}
	}

	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render("  esc back  s send  a attach  q quit"))
	b.WriteByte('\n')

	return b.String()
}

// footer renders the bottom-of-screen prompt area: send input or keybindings.
func (m Model) footer() string {
	if m.sendMode {
		snap := m.inbox.Snapshot()
		name := ""
		if m.selected >= 1 && m.selected <= len(snap) {
			name = snap[m.selected-1].Name
		}
		return fmt.Sprintf("send to %s: %s\n%s",
			name,
			m.sendInput.View(),
			mutedStyle.Render("enter to send  esc to cancel"),
		)
	}
	if m.helpMode {
		return helpText()
	}
	return mutedStyle.Render(footerText)
}

func renderRow(idx int, p inbox.Project, selected bool) string {
	idxStr := fmt.Sprintf("[%d]", idx)
	statusStr := string(p.Status)
	ageStr := ageHuman(time.Since(p.UpdatedAt))
	msgStr := p.LastMessage
	if msgStr == "" && p.LastErr != "" {
		msgStr = "error: " + p.LastErr
	}
	msgStr = truncateOneLine(msgStr, 60)

	statusStyled := statusStyle(p.Status, statusStr)
	row := fmt.Sprintf(
		"%s %-20s %-10s %6s  %s",
		idxStr, p.Name, p.Tool, ageStr, statusStyled,
	)
	if msgStr != "" {
		row += mutedStyle.Render("  " + msgStr)
	}
	if selected {
		row = selectedStyle.Render(row)
	}
	return row
}

func statusStyle(s driver.Status, text string) string {
	switch s {
	case driver.StatusIdle:
		return mutedStyle.Render(text)
	case driver.StatusWorking:
		return workingStyle.Render(text)
	case driver.StatusWaiting:
		return waitingStyle.Render(text)
	case driver.StatusError:
		return errorStyle.Render(text)
	}
	return text
}

func ageHuman(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func truncateOneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func shortSession(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	if id == "" {
		return "(none — send a message first)"
	}
	return id
}

// indent prepends prefix to every line of s.
func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

func wrapToast(s string, width int) string {
	if width <= 0 {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		b.WriteRune(r)
		if r == '\n' {
			col = 0
			continue
		}
		col++
		if col >= width-2 {
			b.WriteRune('\n')
			col = 0
		}
	}
	return b.String()
}

func helpText() string {
	lines := []string{
		"  keybindings:",
		"    j/k or ↑↓     navigate",
		"    1-9           select by index",
		"    s             send message to selected",
		"    v or enter    open detail view (full message + metadata)",
		"    a             attach to live session (interactive)",
		"    r             refresh toast",
		"    ?             toggle this help",
		"    q or ctrl+c   quit",
	}
	return strings.Join(lines, "\n")
}

const footerText = "j/k move  s send  v detail  a attach  ? help  q quit"

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
