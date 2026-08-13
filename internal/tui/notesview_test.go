package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Everything in the store was written by model output, and a standing rule
// never ages out on its own, so there has to be somewhere to look at it.
func TestMemoryViewListsEveryKind(t *testing.T) {
	m := tabsFixture(t)
	m.inbox.AddNotes([]string{"teploy depends on the db layer"})
	m.inbox.AddConstraints([]string{"neutron stays on the free model"})
	m.inbox.AddPriorities([]string{"teploy ships first"})
	m.view = viewNotes

	out := m.renderNotes()
	for _, want := range []string{"fact", "constraint", "priority",
		"depends on the db layer", "free model", "ships first"} {
		if !strings.Contains(out, want) {
			t.Errorf("memory view missing %q:\n%s", want, out)
		}
	}
}

// A rule that arrived through a poisoned reply has to be removable without
// editing notes.json by hand.
func TestMemoryViewDeletesTheSelectedNote(t *testing.T) {
	m := tabsFixture(t)
	m.inbox.AddConstraints([]string{"a rule that should not be here"})
	m.view = viewNotes
	m.notesCursor = 0

	next, _ := m.handleNotesKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	_ = next
	if got := m.inbox.Notes(); len(got) != 0 {
		t.Errorf("the note survived deletion: %+v", got)
	}
}

// Deleting acts on what is on screen, not on whatever now sits at that index.
func TestDeleteIsByTextNotIndex(t *testing.T) {
	m := tabsFixture(t)
	m.inbox.AddNotes([]string{"first", "second", "third"})
	m.view = viewNotes
	m.notesCursor = 1

	target := m.inbox.Notes()[1].Text
	m.handleNotesKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	for _, n := range m.inbox.Notes() {
		if n.Text == target {
			t.Fatalf("the wrong note was deleted; %q survived", target)
		}
	}
	if len(m.inbox.Notes()) != 2 {
		t.Errorf("deleted %d notes, want 1", 3-len(m.inbox.Notes()))
	}
}

func TestMemoryViewHandlesAnEmptyStore(t *testing.T) {
	m := tabsFixture(t)
	m.view = viewNotes
	if out := m.renderNotes(); !strings.Contains(out, "nothing remembered yet") {
		t.Errorf("empty store rendered as:\n%s", out)
	}
	// Deleting with nothing there must not panic or wander off the slice.
	m.handleNotesKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m.handleNotesKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m.handleNotesKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
}
