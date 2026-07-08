package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"agentinbox/internal/driver"
)

// Frame and layout styles.

var (
	// frameStyle wraps the entire app in a rounded border.
	frameStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("238"))

	// separatorStyle draws a thin horizontal divider.
	separatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238"))

	// titleStyle for the frame title line.
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15"))

	// Status badge styles — colored background pills.
	badgeIdle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("245")).
			Padding(0, 1)

	badgeWorking = lipgloss.NewStyle().
			Background(lipgloss.Color("17")).
			Foreground(lipgloss.Color("39")).
			Padding(0, 1)

	badgeWaiting = lipgloss.NewStyle().
			Background(lipgloss.Color("17")).
			Foreground(lipgloss.Color("221")).
			Padding(0, 1)

	badgeError = lipgloss.NewStyle().
			Background(lipgloss.Color("52")).
			Foreground(lipgloss.Color("196")).
			Padding(0, 1)
)

// statusBadge renders a status as a colored pill badge.
func statusBadge(s driver.Status, activity string) string {
	text := string(s)
	if activity != "" && s == driver.StatusWorking {
		text = string(s) + ":" + activity
	}
	switch s {
	case driver.StatusIdle:
		return badgeIdle.Render(text)
	case driver.StatusWorking:
		return badgeWorking.Render(text)
	case driver.StatusWaiting:
		return badgeWaiting.Render(text)
	case driver.StatusError:
		return badgeError.Render(text)
	}
	return text
}

// renderFrame wraps body content in a rounded border with a title line
// at the top and a footer pinned at the bottom. The body is padded with
// blank lines to fill the available terminal height so the footer always
// sits at the bottom.
//
// If the terminal is too small (width < 30 or height < 8), the frame is
// skipped and raw body+footer is returned.
func renderFrame(width, height int, title, body, footer string) string {
	if width < 30 || height < 8 {
		// Too small for a frame — just return content.
		return body + "\n\n" + footer
	}

	// Inner dimensions (account for left+right border).
	innerW := width - 2

	// Content width (account for border + 1-char padding each side).
	contentW := innerW - 2

	// Split body and footer into lines for height calculation.
	bodyLines := strings.Split(body, "\n")
	footerLines := strings.Split(footer, "\n")

	// Calculate available body height:
	//   innerH = height - 2 (top + bottom border)
	//   fixed lines = 1 (title) + 1 (blank) + 1 (blank before sep) + 1 (sep) + len(footer)
	innerH := height - 2
	fixedLines := 1 + 1 + 1 + 1 + len(footerLines)
	bodyAvailH := innerH - fixedLines
	if bodyAvailH < 1 {
		bodyAvailH = 1
	}

	// Trim body if too long.
	if len(bodyLines) > bodyAvailH {
		bodyLines = bodyLines[:bodyAvailH]
	}

	// Pad body to fill available height.
	for len(bodyLines) < bodyAvailH {
		bodyLines = append(bodyLines, "")
	}

	// Apply width to each line. lipgloss.Width handles ANSI codes correctly.
	bodyContent := strings.Join(bodyLines, "\n")

	// Title line — styled + padded to content width.
	titleStyled := lipgloss.NewStyle().Width(contentW).Bold(true).Render(title)

	// Separator — thin line spanning content width.
	sep := separatorStyle.Render(strings.Repeat("─", contentW))

	// Footer — padded to content width.
	footerContent := strings.Join(footerLines, "\n")

	// Stack: title, blank, body, blank, separator, footer
	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyled,
		"",
		bodyContent,
		"",
		sep,
		footerContent,
	)

	// Apply frame border.
	return frameStyle.Render(content)
}

// separator returns a thin horizontal line of the given width.
func separator(width int) string {
	return separatorStyle.Render(strings.Repeat("─", width))
}
