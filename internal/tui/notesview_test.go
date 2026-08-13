package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/im-tyler/agent-inbox/internal/config"
)

// Everything in the store was written by model output, and a standing rule
// never ages out on its own, so there has to be somewhere to look at it.
func TestMemoryViewListsEveryKind(t *testing.T) {
	m := tabsFixture(t)
	m.inbox.AddNotes([]string{"teploy depends on the db layer"})
	m.inbox.ProposeConstraints([]string{"neutron stays on the free model"})
	m.inbox.ProposePriorities([]string{"teploy ships first"})
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
	m.inbox.ProposeConstraints([]string{"a rule that should not be here"})
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

// A proposal has to be distinguishable from something in force, or accepting
// one is a decision made without the fact that matters being on screen.
func TestMemoryViewSeparatesProposalsFromRules(t *testing.T) {
	m := tabsFixture(t)
	m.inbox.WithRules([]string{"a rule you already set"}, nil)
	m.inbox.ProposeConstraints([]string{"a rule the supervisor wants"})
	m.view = viewNotes

	out := m.renderNotes()
	for _, want := range []string{"in force", "a rule you already set", "constraint?", "a accept"} {
		if !strings.Contains(out, want) {
			t.Errorf("memory view missing %q:\n%s", want, out)
		}
	}
}

// Accepting is the only action in this program that changes what binds the
// supervisor, and it writes to config rather than to the model's own store.
func TestAcceptRatifiesAProposal(t *testing.T) {
	m := tabsFixtureWithConfig(t)
	m.inbox.ProposeConstraints([]string{"neutron stays on the free model"})
	m.view = viewNotes
	m.notesCursor = 0

	m.handleNotesKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	rules := m.inbox.Rules()
	if len(rules) != 1 || rules[0].Text != "neutron stays on the free model" {
		t.Fatalf("the rule was not ratified: %+v (toast %q)", rules, m.toast)
	}
	if len(m.inbox.ProposedRules()) != 0 {
		t.Error("the proposal is still pending after acceptance")
	}
}

// A fact is an observation, not policy, and must not be promotable into one
// by putting the cursor on it.
func TestAcceptRefusesAPlainFact(t *testing.T) {
	m := tabsFixtureWithConfig(t)
	m.inbox.AddNotes([]string{"teploy depends on the db layer"})
	m.view = viewNotes
	m.notesCursor = 0

	m.handleNotesKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if len(m.inbox.Rules()) != 0 {
		t.Errorf("a fact became a binding rule: %+v", m.inbox.Rules())
	}
}

// tabsFixtureWithConfig is the same fleet, with a config file to ratify into.
func tabsFixtureWithConfig(t *testing.T) Model {
	t.Helper()
	m := tabsFixture(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := config.Save(cfgPath, &config.Settings{}); err != nil {
		t.Fatal(err)
	}
	m.inbox.WithConfigPath(cfgPath).WithNotesPath(filepath.Join(dir, "notes.json"))
	return m
}
