package inbox

import (
	"strings"
	"testing"
)

// A king with no fleet is still a supervisor having a conversation, and the
// facts that come out of it are exactly the cross-cutting ones worth keeping.
// Before this, memory switched itself off when the last project disconnected.
func TestSoloKingTakesNotes(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{"Noted.\n[note: teploy depends on Neutron's DB layer]"}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "teploy uses neutron's db", nil); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the note", func() bool { return len(in.Notes()) == 1 })
	if got := in.Notes()[0].Text; !strings.Contains(got, "Neutron's DB layer") {
		t.Errorf("note = %q", got)
	}
}

// The syntax has to be in the prompt or the king never learns it exists, and
// [note: ...] stays a thing the user types rather than a thing it can do.
func TestSoloKingIsToldTheNoteSyntax(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{"ok"}

	in := kingFixture(t, d)
	in.AddNotes([]string{"the OVH box runs the marketing sites"})
	if err := in.KingSend(1, "hello", nil); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the king's turn", func() bool { return len(d.promptsFor("/king")) == 1 })

	prompt := d.promptsFor("/king")[0]
	for _, want := range []string{"[note:", "[note drop:", "the OVH box runs the marketing sites"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("solo prompt missing %q:\n%s", want, prompt)
		}
	}
	// No fleet means no fleet block and no dispatch syntax to misuse.
	for _, unwanted := range []string{"Your fleet:", "[send to "} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("solo prompt contains %q:\n%s", unwanted, prompt)
		}
	}
}

// A note tagged to a project is not context for a conversation that is not
// about it. Only the cross-cutting ones follow the king into a solo turn.
func TestSoloKingSeesOnlyUntaggedNotes(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{"ok"}

	in := kingFixture(t, d)
	in.AddNotes([]string{"omni needs a new provider key", "postgres is the default everywhere"})
	if err := in.KingSend(1, "hello", nil); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the king's turn", func() bool { return len(d.promptsFor("/king")) == 1 })

	prompt := d.promptsFor("/king")[0]
	if !strings.Contains(prompt, "postgres is the default everywhere") {
		t.Errorf("cross-cutting note missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "omni needs a new provider key") {
		t.Errorf("note about an unconnected project was injected:\n%s", prompt)
	}
}

// Nothing is connected, so a directive names nobody the user put in front of
// this king. Harvest the notes; do not go and send to a project on the
// strength of a syntax the king was never offered.
func TestSoloKingDoesNotDispatch(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{"[send to omni: do the thing]\n[note: kept anyway]"}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "go", nil); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the note", func() bool { return len(in.Notes()) == 1 })
	if got := d.promptsFor("/omni"); len(got) != 0 {
		t.Errorf("solo king dispatched to omni: %v", got)
	}
}
