package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/im-tyler/agent-inbox/internal/inbox"
)

// The supervisor's memory, on screen.
//
// This exists because of who writes it. Every note, constraint and priority in
// the store arrives as model output — shaped by project replies, which are
// themselves shaped by whatever those agents have read. That was tolerable
// while a note was an observation that ages out of a bounded store on its own.
//
// It stopped being tolerable when notes gained kinds. A constraint is injected
// into every turn regardless of subject, is given up last under eviction, and
// survives the removal of the project it names — which is exactly right for a
// decision the user made, and exactly wrong for one that arrived through a
// poisoned reply and can no longer age out.
//
// So: the memory has to be legible and it has to be deletable, without editing
// notes.json by hand.

// renderNotes draws the memory view.
func (m *Model) renderNotes() string {
	notes := m.inbox.Notes()
	width := m.width - 8
	if width < 20 {
		width = 20
	}

	rules := m.inbox.Rules()

	var b strings.Builder
	b.WriteString(headerStyle.Render("supervisor memory"))
	b.WriteString("\n\n")

	// Ratified rules first, and visibly apart. These came from config — from
	// you — and they are the only things here that bind the supervisor.
	if len(rules) > 0 {
		b.WriteString(mutedStyle.Render("  in force — from your config, edit the file to change them"))
		b.WriteString("\n")
		for _, r := range rules {
			b.WriteString("  ")
			b.WriteString(waitingStyle.Render(fmt.Sprintf("%-10s", string(r.Kind))))
			b.WriteString(" ")
			b.WriteString(truncateOneLine(r.Text, width))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(mutedStyle.Render(fmt.Sprintf(
		"  %d of %d written by the supervisor — a proposal binds nothing until you accept it",
		len(notes), inbox.MaxNotes)))
	b.WriteString("\n\n")

	if len(notes) == 0 {
		b.WriteString(mutedStyle.Render("  (nothing remembered yet)"))
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("  esc back"))
		return b.String()
	}

	for i, n := range notes {
		marker := "  "
		if i == m.notesCursor {
			marker = "▶ "
		}
		kind := string(n.Kind)
		if kind == "" {
			kind = string(inbox.KindFact)
		}
		style := mutedStyle
		if n.Proposed {
			// A proposal is styled apart because accepting one is the only
			// action in this program that changes what binds the supervisor.
			style = errorStyle
			kind += "?"
		}
		b.WriteString(marker)
		b.WriteString(style.Render(fmt.Sprintf("%-11s", kind)))
		b.WriteString(" ")
		b.WriteString(truncateOneLine(n.Text, width))
		b.WriteString("\n")
		if i == m.notesCursor {
			meta := "    " + n.CreatedAt.Format("2 Jan 15:04")
			if len(n.Projects) > 0 {
				meta += "  ·  " + strings.Join(n.Projects, ", ")
			}
			if n.Proposed {
				meta += "  ·  proposed by the supervisor; not in force"
			}
			b.WriteString(mutedStyle.Render(truncateOneLine(meta, width)))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	if hasProposal(notes) {
		b.WriteString(mutedStyle.Render("  j/k move  a accept (writes to config)  d delete  esc back"))
	} else {
		b.WriteString(mutedStyle.Render("  j/k move  d delete  esc back"))
	}
	return b.String()
}

func hasProposal(notes []inbox.Note) bool {
	for _, n := range notes {
		if n.Proposed {
			return true
		}
	}
	return false
}

func (m *Model) handleNotesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	notes := m.inbox.Notes()
	switch msg.String() {
	case "esc", "q":
		m.view = viewMain
		return m, nil

	case "j", "down":
		if m.notesCursor < len(notes)-1 {
			m.notesCursor++
		}
		return m, nil

	case "k", "up":
		if m.notesCursor > 0 {
			m.notesCursor--
		}
		return m, nil

	case "a":
		// Accepting is the only action in this program that changes what binds
		// the supervisor, so it is an explicit keypress on a specific line and
		// it writes to config — the file you own — rather than to the store the
		// supervisor dictates to.
		if m.notesCursor < 0 || m.notesCursor >= len(notes) {
			return m, nil
		}
		if !notes[m.notesCursor].Proposed {
			m.toast = "only a proposed rule can be accepted"
			m.toastAt = time.Now()
			return m, nil
		}
		if err := m.inbox.RatifyRule(notes[m.notesCursor].Text); err != nil {
			m.toast = err.Error()
		} else {
			m.toast = "in force, and written to config"
		}
		m.toastAt = time.Now()
		if m.notesCursor > 0 && m.notesCursor >= len(notes)-1 {
			m.notesCursor--
		}
		return m, nil

	case "d":
		if m.notesCursor < 0 || m.notesCursor >= len(notes) {
			return m, nil
		}
		// By text, not by index: the store can move between the render and the
		// keypress, and deleting whatever now sits at that position is how the
		// wrong rule gets removed.
		if m.inbox.DropNoteExact(notes[m.notesCursor].Text) {
			m.toast = "forgotten"
		} else {
			m.toast = "already gone"
		}
		m.toastAt = time.Now()
		if m.notesCursor > 0 && m.notesCursor >= len(notes)-1 {
			m.notesCursor--
		}
		return m, nil
	}
	return m, nil
}
