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

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/lipgloss"

	"github.com/im-tyler/agent-inbox/internal/board"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/inbox"
)

// viewMode controls which screen the TUI is rendering.
type viewMode int

const (
	viewDetail viewMode = iota
	viewNewProject
	viewDeleteConfirm
	viewToolPicker
	viewMain  // king-first split-pane layout (default)
	viewInbox // the session inbox, hosted rather than run as its own program
)

// Model is the Bubble Tea model for the agent-inbox dashboard.
type Model struct {
	inbox     *inbox.Inbox
	eventsDir string

	view        viewMode
	selected    int  // 1-based, matches existing convention
	sendMode    bool // when true, sendInput is active for the selected project
	helpMode    bool // when true, keybindings overlay is shown
	sendInput   textinput.Model
	np          newProjectModel // populated when view == viewNewProject
	pendingTool string          // populated when view == viewToolPicker

	// board is the session inbox, live while view == viewInbox.
	board board.Model

	// Spinner animation state; runs only while a turn is in flight.
	spin     int
	spinning bool

	// Detail view scroll: number of lines from the top of the body.
	// Set to a large number when entering detail view to pin to bottom.
	detailScroll int

	// King-first main view state.
	mainInput            textarea.Model
	mainScrollFromBottom int  // lines scrolled up from bottom (0 = at bottom)
	mainAutoScroll       bool // when true, auto-pins to bottom on each tick

	// Tab-focus state: false = chat focused (default), true = sidebar focused.
	focusSidebar  bool
	sidebarCursor int // 1-based project index currently highlighted in sidebar

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

	// A textarea, not a single line: a prompt worth sending to a fleet is
	// often several sentences, and typing it into a one-line box that scrolls
	// sideways means never seeing what you wrote.
	mi := textarea.New()
	mi.Placeholder = "type to talk to king..."
	mi.CharLimit = 0
	mi.SetWidth(80)
	mi.SetHeight(1)
	mi.ShowLineNumbers = false
	mi.Prompt = ""
	mi.FocusedStyle.CursorLine = lipgloss.NewStyle()
	mi.Focus()

	return Model{
		inbox:          in,
		eventsDir:      eventsDir,
		view:           viewMain,
		selected:       1,
		sendInput:      ti,
		mainInput:      mi,
		mainAutoScroll: true,
		sidebarCursor:  2, // first non-king project
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

// Init starts the per-second ticker and the cursor blink.
func (m Model) Init() tea.Cmd {
	return tea.Batch(tick(), textinput.Blink)
}

// Update handles all messages: key presses, window resize, ticks.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sendInput.Width = max(60, msg.Width-30)
		m.mainInput.SetWidth(max(40, msg.Width-8))
		if m.view == viewInbox {
			return m.forwardToBoard(msg)
		}
		return m, nil

	case tickMsg:
		if upd := m.inbox.Ingest(m.eventsDir); len(upd) > 0 {
			m.toast = fmt.Sprintf("waiting: %s", strings.Join(upd, ", "))
			m.toastAt = time.Now()
		}
		// Auto-scroll: pin to bottom.
		if m.mainAutoScroll {
			m.mainScrollFromBottom = 0
		}
		// Clamp scroll to prevent blank conversation.
		if m.mainScrollFromBottom > 0 {
			snap := m.inbox.Snapshot()
			if m.kingIndex() >= 1 && m.kingIndex() <= len(snap) {
				king := snap[m.kingIndex()-1]
				lineCount := 2
				for _, msg := range king.History {
					lineCount += 1 + strings.Count(msg.Content, "\n") + 1 + 1
				}
				bodyH := m.height - 7
				maxScroll := lineCount - bodyH
				if maxScroll < 0 {
					maxScroll = 0
				}
				if m.mainScrollFromBottom > maxScroll {
					m.mainScrollFromBottom = maxScroll
				}
			}
		}
		// Catch a turn that started without going through a keypress —
		// an ingested hook event, or a restart mid-send.
		return m, tea.Batch(tick(), m.startSpin())

	case spinMsg:
		m.spin++
		if !m.anyWorking() {
			m.spinning = false
			return m, nil
		}
		return m, spinTick()

	case tea.KeyMsg:
		if m.sendMode {
			return m.handleSendKey(msg)
		}
		return m.handleKey(msg)
	default:
		// The board owns message types this package cannot name — its tick,
		// its fetch results. While its view is open they belong to it.
		if m.view == viewInbox {
			return m.forwardToBoard(msg)
		}
		// Forward non-key messages (blink, etc.) to the focused input
		// so the cursor blink cycle stays alive.
		var cmd tea.Cmd
		if m.view == viewMain {
			m.mainInput, cmd = m.mainInput.Update(msg)
		}
		return m, cmd
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global keys (work in any view) unless we're typing into send input.
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	}

	switch m.view {
	case viewMain:
		return m.handleMainKey(msg)
	case viewDetail:
		return m.handleDetailKey(msg)
	case viewNewProject:
		return m.handleNewProjectKey(msg)
	case viewDeleteConfirm:
		return m.handleDeleteConfirmKey(msg)
	case viewToolPicker:
		return m.handleToolPickerKey(msg)
	case viewInbox:
		return m.handleInboxKey(msg)
	}
	return m, nil
}

func (m Model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Send mode takes priority.
	if m.sendMode {
		return m.handleSendKey(msg)
	}

	switch msg.String() {
	case "esc":
		m.view = viewMain
		m.detailScroll = 0

	case "q":
		return m, tea.Quit

	case "s":
		// Send a follow-up from the detail view.
		snap := m.inbox.Snapshot()
		if m.selected < 1 || m.selected > len(snap) {
			return m, nil
		}
		if snap[m.selected-1].Status == driver.StatusWorking {
			m.toast = "already working — press x to cancel first"
			m.toastAt = time.Now()
			return m, nil
		}
		m.sendMode = true
		m.sendInput.Focus()
		return m, textinput.Blink

	case "a":
		args, dir, err := m.inbox.AttachArgs(m.selected)
		if err != nil {
			m.toast = err.Error()
			m.toastAt = time.Now()
			return m, nil
		}
		m.attachRequest = &attachArgs{Argv: args, Dir: dir}
		return m, tea.Quit

	case "j", "down":
		m.detailScroll++
		m.clampDetailScroll()
	case "k", "up":
		if m.detailScroll > 0 {
			m.detailScroll--
		}
	case "pgdown", " ":
		m.detailScroll += 10
		m.clampDetailScroll()
	case "pgup":
		m.detailScroll -= 10
		if m.detailScroll < 0 {
			m.detailScroll = 0
		}
	case "g":
		m.detailScroll = 0
	case "G":
		m.detailScroll = m.detailMaxScroll()
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
		err := m.inbox.Send(idx, text)
		if err != nil {
			// Keep the input text and stay in send mode so the user
			// can edit and retry without retyping.
			m.toast = err.Error()
			m.toastAt = time.Now()
			return m, nil
		}
		// Success — clear and exit send mode.
		m.sendMode = false
		m.sendInput.Blur()
		m.sendInput.Reset()
		snap := m.inbox.Snapshot()
		if idx >= 1 && idx <= len(snap) {
			m.toast = "sent to " + snap[idx-1].Name
			m.toastAt = time.Now()
		}
		return m, nil

	case "esc":
		m.view = viewMain
		m.detailScroll = 0
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
	case viewMain:
		return m.renderMain()
	case viewDetail:
		return m.viewDetail()
	case viewNewProject:
		return m.renderNewProject()
	case viewDeleteConfirm:
		return m.renderDeleteConfirm()
	case viewToolPicker:
		return m.renderToolPicker()
	case viewInbox:
		return m.board.View()
	default:
		return m.renderMain()
	}
}

func (m Model) viewDetail() string {
	snap := m.inbox.Snapshot()
	if m.selected < 1 || m.selected > len(snap) {
		m.view = viewMain
		return m.renderMain()
	}
	p := snap[m.selected-1]

	// Frame borders and padding take four columns.
	detailW := m.width - 6
	if detailW < 20 {
		detailW = 20
	}

	var b strings.Builder

	// Metadata block (compact, 2 lines).
	b.WriteString(mutedStyle.Render(fmt.Sprintf("dir: %s   session: %s   turns: %d",
		shortPath(p.Dir), shortSession(p.SessionID), len(p.History))))
	b.WriteString("\n\n")

	// Full history (not truncated — scrollable).
	if len(p.History) == 0 {
		b.WriteString(mutedStyle.Render("(no messages yet)"))
		b.WriteString("\n")
	} else {
		// Same rendering as the king's thread. This view is where the full
		// replies live, so it is the last place that should hand back the
		// raw "## Heading" and "[send to ...]" the other one strips.
		for _, msg := range p.History {
			body := stripDirectives(msg.Content)
			if body == "" {
				continue
			}
			glyph, label, style := speaker(msg.Role, p.Tool)
			b.WriteString(speakerLine(glyph, label, msg.Timestamp.Format(time.Kitchen), style, detailW))
			b.WriteString("\n")
			b.WriteString(strings.Join(wrapBody(body, detailW), "\n"))
			b.WriteString("\n\n")
		}
	}

	// Live streaming text (if currently working).
	if p.Status == driver.StatusWorking {
		b.WriteString(speakerLine(m.frame(), p.Tool, workingLabel(p.Activity), workingStyle, detailW))
		b.WriteString("\n")
		if p.StreamingText != "" {
			b.WriteString(strings.Join(wrapBody(p.StreamingText, detailW), "\n"))
			b.WriteString("\n")
		}
	}

	// Build full body and apply scroll.
	body := b.String()
	bodyLines := strings.Split(body, "\n")

	// Available height for body inside the frame.
	availH := m.height - 6
	if availH < 3 {
		availH = 3
	}

	// Scroll is clamped in handleDetailKey (which can persist via return).
	// Here we just apply the current offset.
	maxScroll := len(bodyLines) - availH
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.detailScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}

	// Slice the visible window.
	start := scroll
	end := start + availH
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := strings.Join(bodyLines[start:end], "\n")

	// Scroll indicator if there's more content.
	scrollInfo := ""
	if maxScroll > 0 {
		scrollInfo = fmt.Sprintf("  (%d-%d of %d lines)", start+1, end, len(bodyLines))
	}

	title := fmt.Sprintf("%s  (%s)  %s%s", p.Name, p.Tool, statusBadge(p.Status, p.Activity, m.frame()), scrollInfo)
	var footer string
	if m.sendMode {
		footer = fmt.Sprintf("send: %s  (enter to send, esc to cancel)", m.sendInput.View())
	} else {
		footer = mutedStyle.Render("j/k scroll  s send  a attach  esc back  q quit")
	}
	return renderFrame(m.width, m.height, title, visible, footer)
}

func shortPath(dir string) string {
	if len(dir) > 40 {
		return "…" + dir[len(dir)-38:]
	}
	return dir
}

// detailBodyLineCount estimates how many lines the detail-view body will
// occupy for the currently-selected project. Used by detailMaxScroll and
// clampDetailScroll to bound the scroll offset.
func (m Model) detailBodyLineCount() int {
	snap := m.inbox.Snapshot()
	if m.selected < 1 || m.selected > len(snap) {
		return 0
	}
	p := snap[m.selected-1]
	lines := 2 // metadata (1 line) + blank
	for _, msg := range p.History {
		lines += 2 // header + blank
		lines += strings.Count(msg.Content, "\n") + 1
	}
	if p.Status == driver.StatusWorking && p.StreamingText != "" {
		lines += 2 + strings.Count(p.StreamingText, "\n")
	} else if p.Status == driver.StatusWorking {
		lines += 1
	}
	return lines
}

func (m Model) detailMaxScroll() int {
	availH := m.height - 6
	if availH < 1 {
		availH = 1
	}
	bodyLines := m.detailBodyLineCount()
	max := bodyLines - availH
	if max < 0 {
		return 0
	}
	return max
}

func (m *Model) clampDetailScroll() {
	max := m.detailMaxScroll()
	if m.detailScroll > max {
		m.detailScroll = max
	}
	if m.detailScroll < 0 {
		m.detailScroll = 0
	}
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

// helpText is the ? overlay. It describes the two focus modes of the main
// view, because that is the only view you can be in when you press ?.
//
// It used to document a flat list with index selection and a ":" actions menu.
// That view had already become unreachable, so the help was a map of a screen
// the program could not open.
func helpText() string {
	lines := []string{
		"  chat focused:",
		"    enter         send to the supervisor",
		"    alt+enter     newline",
		"    pgup/pgdn     scroll the conversation",
		"    tab           focus the fleet",
		"    ?             close this help",
		"    ctrl+c        quit",
		"",
		"  fleet focused (tab):",
		"    j/k or ↑↓     move through the fleet",
		"    enter         open the project's detail view",
		"    i             session inbox",
		"    n             new project",
		"    d             delete project",
		"    t             change tool",
		"    a             attach to the session",
		"    x             cancel a send / dismiss a badge",
		"    tab or esc    back to chat",
		"",
		"  in detail view:",
		"    j/k, pgup/pgdn, g/G   scroll",
		"    s             send a follow-up",
		"    a             attach",
		"    esc           back",
	}
	return strings.Join(lines, "\n")
}

// (max is the Go 1.21+ builtin — no local definition needed.)

// kingIndex resolves the supervisor's position in the current project list.
//
// Resolved on each use rather than stored. The old code kept two integers —
// one hardcoded to 1 for the dashboard, one set by pressing K — which could
// name different projects, and either could start naming the wrong one as
// soon as a removal shifted the list. Identity is the supervisor's name, and
// the inbox owns it.
//
// Returns 0 when there is no supervisor, which callers must treat as "no
// conversation to show" rather than as an index.
func (m Model) kingIndex() int { return m.inbox.KingIndex() }
