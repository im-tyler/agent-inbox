package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/inbox"
)

// Run launches the Bubble Tea dashboard and blocks until the user quits
// or requests an attach. The returned Model is inspectable via
// (*Model).AttachRequest() so the caller can run the attach command and
// then re-launch the TUI in a loop.
func Run(in *inbox.Inbox, eventsDir string) (Model, error) {
	p := tea.NewProgram(
		New(in, eventsDir),
		tea.WithAltScreen(),
	)
	m, err := p.Run()
	if err != nil {
		return Model{}, fmt.Errorf("tui: %w", err)
	}
	tm, ok := m.(Model)
	if !ok {
		return Model{}, fmt.Errorf("tui: unexpected model type %T", m)
	}
	return tm, nil
}
