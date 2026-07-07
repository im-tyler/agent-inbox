package tui

import "github.com/charmbracelet/lipgloss"

// Styling palette.
//
// Kept intentionally minimal: one accent per status, one muted gray for
// secondary text, and a subtle highlight for the selected row. Designed to
// render cleanly on both dark and light terminals.

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")) // bright white

	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")) // medium gray

	workingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")). // blue
			Bold(true)

	waitingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("221")). // yellow
			Bold(true)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // bright red
			Bold(true)

	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")). // dark gray
			Foreground(lipgloss.Color("15"))   // bright white
)
