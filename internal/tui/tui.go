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

// Model is the Bubble Tea model for the agent-inbox dashboard.
type Model struct {
	inbox     *inbox.Inbox
	eventsDir string

	selected  int  // 1-based, matches existing convention
	sendMode  bool // when true, sendInput is active for the selected project
	helpMode  bool // when true, keybindings overlay is shown
	sendInput textinput.Model

	toast   string
	toastAt time.Time

	width  int
	height int
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
		selected:  1,
		sendInput: ti,
	}
}

// tickMsg is emitted once per second to drive live updates.
type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// sendResultMsg is emitted when a background send completes.
type sendResultMsg struct {
	err error
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
		// Ingest any new stop-hook events from disk, then keep ticking.
		if upd := m.inbox.Ingest(m.eventsDir); len(upd) > 0 {
			m.toast = fmt.Sprintf("waiting: %s", strings.Join(upd, ", "))
			m.toastAt = time.Now()
		}
		return m, tick()

	case tea.KeyMsg:
		// Send mode handles its own keys first.
		if m.sendMode {
			return m.handleSendKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
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
		// Direct-select by number.
		var n int
		fmt.Sscanf(msg.String(), "%d", &n)
		snap := m.inbox.Snapshot()
		if n >= 1 && n <= len(snap) {
			m.selected = n
		}

	case "s":
		// Enter send mode for the selected project.
		m.sendMode = true
		m.sendInput.Focus()
		return m, textinput.Blink

	case "v", "enter":
		// Toggle to detail view (toast with full message).
		snap := m.inbox.Snapshot()
		if m.selected >= 1 && m.selected <= len(snap) {
			p := snap[m.selected-1]
			if p.LastMessage != "" {
				m.toast = p.LastMessage
				m.toastAt = time.Now()
			} else if p.LastErr != "" {
				m.toast = "error: " + p.LastErr
				m.toastAt = time.Now()
			}
		}

	case "a":
		// Attach: surface the command the user should run.
		args, dir, err := m.inbox.AttachArgs(m.selected)
		if err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
		} else {
			m.toast = fmt.Sprintf("cd %s && %s", dir, strings.Join(args, " "))
			m.toastAt = time.Now()
		}

	case "r":
		m.toast = "refreshed"
		m.toastAt = time.Now()
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
		// Dispatch in a goroutine; Send returns immediately because it
		// spawns its own background goroutine for the actual work.
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

	snap := m.inbox.Snapshot()
	waiting := m.inbox.WaitingCount()

	var b strings.Builder

	// Header.
	header := headerStyle.Render(fmt.Sprintf(
		"agent-inbox  %d projects  %d waiting",
		len(snap), waiting,
	))
	b.WriteString(header)
	b.WriteByte('\n')

	// Project list.
	if len(snap) == 0 {
		b.WriteString(mutedStyle.Render("  no projects configured — edit config.json\n"))
	} else {
		for i, p := range snap {
			row := renderRow(i+1, p, m.selected == i+1)
			b.WriteString(row)
			b.WriteByte('\n')
		}
	}

	// Toast / detail line.
	if m.toast != "" && time.Since(m.toastAt) < 6*time.Second {
		b.WriteByte('\n')
		b.WriteString(wrapToast(m.toast, m.width))
		b.WriteByte('\n')
	}

	// Footer / send input / help.
	b.WriteByte('\n')
	if m.sendMode {
		name := ""
		if m.selected >= 1 && m.selected <= len(snap) {
			name = snap[m.selected-1].Name
		}
		b.WriteString(fmt.Sprintf("  send to %s: %s", name, m.sendInput.View()))
		b.WriteByte('\n')
		b.WriteString(mutedStyle.Render("  enter to send  esc to cancel"))
	} else if m.helpMode {
		b.WriteString(helpText())
	} else {
		b.WriteString(mutedStyle.Render(footerText))
	}
	b.WriteByte('\n')

	return b.String()
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

func wrapToast(s string, width int) string {
	if width <= 0 {
		return s
	}
	// Simple word-wrap. Toasts are short; this is enough.
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
		"    v or enter    view full message",
		"    a             show attach command",
		"    r             refresh toast",
		"    ?             toggle this help",
		"    q or ctrl+c   quit",
	}
	return strings.Join(lines, "\n")
}

const footerText = "j/k move  s send  v view  a attach  ? help  q quit"

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
