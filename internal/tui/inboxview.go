package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/im-tyler/agent-inbox/internal/board"
	"github.com/im-tyler/agent-inbox/internal/sources"
)

// The inbox is a view inside the supervisor, not a second program. It is the
// one list of sessions: what is waiting on you, and what you can take on as a
// project. Keeping a separate picker would have meant two lists of the same
// folders, drifting apart.

// openInbox builds the board and starts its first fetch.
func (m *Model) openInbox() tea.Cmd {
	cfg, err := sources.Load(sources.ConfigPath())
	if err != nil {
		m.toast = "sources: " + err.Error()
		m.toastAt = time.Now()
		return nil
	}
	built := cfg.Build()
	if len(built) == 0 {
		m.toast = "no usable sources configured"
		m.toastAt = time.Now()
		return nil
	}
	// Hand the board the terminal size immediately. It never receives a
	// WindowSizeMsg of its own until the next resize, so without this it lays
	// its first frame out at the 100-column fallback — on a 40-column terminal
	// every row overflows.
	m.board = board.New(built).SetEmbedded(true).SetSize(m.width, m.height)
	m.view = viewInbox
	return m.board.Init()
}

// handleInboxKey asks the board what to do with a key, and acts on the answer.
//
// The board decides rather than the host, because only it knows whether it is
// currently collecting text. Claiming keys here first meant that while filling
// in an action prompt, typing "no" adopted a project and typing "queue" left
// the view.
func (m Model) handleInboxKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	updated, cmd, intent := m.board.EmbeddedKey(msg)
	m.board = updated
	switch intent {
	case board.HostLeave:
		m.view = viewMain
		return m, nil
	case board.HostAdopt:
		return m.adoptSelected()
	}
	return m, cmd
}

// adoptSelected registers the highlighted row as a project.
func (m Model) adoptSelected() (tea.Model, tea.Cmd) {
	item, ok := m.board.Selected()
	if !ok {
		return m, nil
	}
	c, err := candidateFrom(item)
	if err != nil {
		m.toast = err.Error()
		m.toastAt = time.Now()
		return m, nil
	}
	name := c.Name()
	if err := m.inbox.AdoptProject(name, c.Tool, c.Dir, c.SessionID, c.ForkFrom); err != nil {
		m.toast = err.Error()
		m.toastAt = time.Now()
		return m, nil
	}
	m.selected = len(m.inbox.Snapshot())
	switch {
	case c.ForkFrom != "":
		m.toast = fmt.Sprintf("added %s (%s, forks that session's history)", name, c.Tool)
	case c.SessionID != "":
		m.toast = fmt.Sprintf("added %s (%s, resumes its session)", name, c.Tool)
	default:
		m.toast = fmt.Sprintf("added %s (%s, starts a new session)", name, c.Tool)
	}
	m.toastAt = time.Now()
	return m, nil
}

// forwardToBoard passes non-key messages (ticks, fetch results) to the board
// while its view is open.
func (m Model) forwardToBoard(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, cmd := m.board.Update(msg)
	m.board = updated.(board.Model)
	return m, cmd
}
