package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/board"
	"agentinbox/internal/sources"
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
	m.board = board.New(built).SetEmbedded(true)
	m.view = viewInbox
	return m.board.Init()
}

// handleInboxKey intercepts the keys the host owns — leaving, and adopting a
// row — and hands everything else to the board.
func (m Model) handleInboxKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		// The board uses esc to close its own detail pane; only leave the
		// view when there is nothing left for esc to close.
		if msg.String() == "esc" && m.board.InDetail() {
			break
		}
		m.view = viewMain
		return m, nil

	case "n":
		return m.adoptSelected()
	}

	updated, cmd := m.board.Update(msg)
	m.board = updated.(board.Model)
	return m, cmd
}

// adoptSelected registers the highlighted row as a project.
func (m Model) adoptSelected() (tea.Model, tea.Cmd) {
	item, ok := m.board.Selected()
	if !ok {
		return m, nil
	}
	c, ok := candidateFrom(item)
	if !ok {
		m.toast = "no agent session to adopt on this row"
		m.toastAt = time.Now()
		return m, nil
	}
	name := c.Name()
	if err := m.inbox.AdoptProject(name, c.Tool, c.Dir, c.SessionID); err != nil {
		m.toast = err.Error()
		m.toastAt = time.Now()
		return m, nil
	}
	m.selected = len(m.inbox.Snapshot())
	if c.SessionID != "" {
		m.toast = fmt.Sprintf("added %s (%s, resumes its session)", name, c.Tool)
	} else {
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
