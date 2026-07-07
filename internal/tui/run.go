package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"agentinbox/internal/inbox"
)

// Run launches the Bubble Tea dashboard and blocks until the user quits.
func Run(in *inbox.Inbox, eventsDir string) error {
	p := tea.NewProgram(
		New(in, eventsDir),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
