package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/driver"
)

// A turn can take minutes. Without a moving frame there is no difference on
// screen between "thinking" and "hung", so the spinner runs whenever anything
// in the fleet is working — and stops the moment nothing is, rather than
// repainting an idle terminal ten times a second forever.

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerEvery = 90 * time.Millisecond

type spinMsg time.Time

func spinTick() tea.Cmd {
	return tea.Tick(spinnerEvery, func(t time.Time) tea.Msg { return spinMsg(t) })
}

// frame is the current spinner glyph.
func (m Model) frame() string {
	return spinnerFrames[m.spin%len(spinnerFrames)]
}

// anyWorking reports whether a turn is in flight anywhere in the fleet.
func (m Model) anyWorking() bool {
	for _, p := range m.inbox.Snapshot() {
		if p.Status == driver.StatusWorking {
			return true
		}
	}
	return false
}

// startSpin returns the tick that animates the spinner, or nil when one is
// already running. Callers batch it alongside whatever else they were
// returning.
func (m *Model) startSpin() tea.Cmd {
	if m.spinning || !m.anyWorking() {
		return nil
	}
	m.spinning = true
	return spinTick()
}
