package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

func TestParseKingNotes(t *testing.T) {
	in := "I'll look into it.\n[note: teploy depends on Neutron's DB layer]\n[NOTE: omni runs on glm-5.2]\nAnything else?"
	got := ParseKingNotes(in)
	if len(got) != 2 {
		t.Fatalf("got %d notes, want 2: %q", len(got), got)
	}
	if got[0] != "teploy depends on Neutron's DB layer" || got[1] != "omni runs on glm-5.2" {
		t.Errorf("got %q", got)
	}
}

// A directive is a whole line, so prose mentioning the syntax is not one.
func TestParseKingNotesIgnoresProse(t *testing.T) {
	for _, s := range []string{
		"you can write [note: something] to remember it",
		"[note: ]",
		"[note: unterminated",
		"note: not bracketed",
	} {
		if got := ParseKingNotes(s); len(got) != 0 {
			t.Errorf("%q produced %q", s, got)
		}
	}
}

func TestAddNotesDedupesAndCaps(t *testing.T) {
	in := New(nil, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json"))

	in.AddNotes([]string{"a fact", "a fact", "A FACT"})
	if got := in.Notes(); len(got) != 1 {
		t.Fatalf("got %d notes, want 1 after dedupe: %+v", len(got), got)
	}

	many := make([]string, maxNotes+20)
	for i := range many {
		many[i] = "fact number " + string(rune('a'+i%26)) + strings.Repeat("x", i)
	}
	in.AddNotes(many)
	if got := in.Notes(); len(got) != maxNotes {
		t.Errorf("got %d notes, want the cap of %d", len(got), maxNotes)
	}
}

func TestAddNotesTruncatesLongOnes(t *testing.T) {
	in := New(nil, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json"))
	in.AddNotes([]string{strings.Repeat("very long fact. ", 100)})
	got := in.Notes()
	if len(got) != 1 {
		t.Fatalf("got %d notes", len(got))
	}
	if n := len([]rune(got[0].Text)); n > maxNoteLen {
		t.Errorf("note is %d runes, want <= %d", n, maxNoteLen)
	}
}

// Notes have to outlive the process or they are just a longer context window.
func TestNotesPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	notesPath := filepath.Join(dir, "notes.json")

	first := New(nil, map[string]driver.Driver{}, filepath.Join(dir, "s.json")).WithNotesPath(notesPath)
	first.AddNotes([]string{"teploy depends on Neutron"})

	if _, err := os.Stat(notesPath); err != nil {
		t.Fatalf("notes file not written: %v", err)
	}

	second := New(nil, map[string]driver.Driver{}, filepath.Join(dir, "s.json")).WithNotesPath(notesPath)
	got := second.Notes()
	if len(got) != 1 || got[0].Text != "teploy depends on Neutron" {
		t.Errorf("notes did not survive the restart: %+v", got)
	}
}

func TestClearNotes(t *testing.T) {
	dir := t.TempDir()
	in := New(nil, map[string]driver.Driver{}, filepath.Join(dir, "s.json")).WithNotesPath(filepath.Join(dir, "notes.json"))
	in.AddNotes([]string{"a wrong fact"})
	in.ClearNotes()
	if got := in.Notes(); len(got) != 0 {
		t.Errorf("notes survived a clear: %+v", got)
	}
	reloaded := New(nil, map[string]driver.Driver{}, filepath.Join(dir, "s.json")).WithNotesPath(filepath.Join(dir, "notes.json"))
	if got := reloaded.Notes(); len(got) != 0 {
		t.Errorf("cleared notes came back from disk: %+v", got)
	}
}

// Notes are injected before the fleet listing: a fact already established
// should not have to be re-derived from a status line.
func TestNotesLeadTheFleetBlock(t *testing.T) {
	d := newScriptDriver()
	in := kingFixture(t, d)
	in.AddNotes([]string{"omni runs on glm-5.2"})

	got := in.formatKingState([]string{"omni"})
	noteAt := strings.Index(got, "omni runs on glm-5.2")
	fleetAt := strings.Index(got, "Your fleet:")
	if noteAt < 0 {
		t.Fatalf("note missing from fleet state:\n%s", got)
	}
	if noteAt > fleetAt {
		t.Errorf("notes came after the fleet listing:\n%s", got)
	}
	if !strings.Contains(got, "[note:") {
		t.Errorf("the note directive is never taught:\n%s", got)
	}
}

// The king takes a note mid-round and it survives to the next turn's prompt.
func TestKingNotesSurviveARound(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{
		"[send to omni: what model?]",
		"omni is on glm-5.2.\n[note: omni runs on glm-5.2]",
	}
	d.replies["/omni"] = []string{"glm-5.2"}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "what model is omni on", []string{"omni"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the note to be recorded", func() bool {
		return len(in.Notes()) == 1
	})
	if got := in.Notes()[0].Text; got != "omni runs on glm-5.2" {
		t.Errorf("note = %q", got)
	}
	// And it is in front of the king on the next turn.
	if !strings.Contains(in.formatKingState([]string{"omni"}), "omni runs on glm-5.2") {
		t.Error("the note was recorded but never injected")
	}
}

func TestParseKingNoteDrops(t *testing.T) {
	in := "That changed.\n[note drop: teploy depends on Neutron]\n[note: teploy has its own store now]"
	drops := ParseKingNoteDrops(in)
	if len(drops) != 1 || drops[0] != "teploy depends on Neutron" {
		t.Fatalf("drops = %q", drops)
	}
	// "[note:" must not swallow "[note drop:" — a retraction is not a new fact.
	adds := ParseKingNotes(in)
	if len(adds) != 1 || adds[0] != "teploy has its own store now" {
		t.Errorf("adds = %q", adds)
	}
}

func TestDropNotesBySubstring(t *testing.T) {
	in := New(nil, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json"))
	in.AddNotes([]string{"teploy depends on Neutron's DB layer", "omni runs on glm-5.2"})
	if n := in.DropNotes([]string{"depends on Neutron"}); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	got := in.Notes()
	if len(got) != 1 || got[0].Text != "omni runs on glm-5.2" {
		t.Errorf("remaining = %+v", got)
	}
}

// Retract and restate in one turn is how a correction is phrased, so drops
// have to run before adds or the new fact is removed with the old one.
func TestCorrectionInOneTurn(t *testing.T) {
	in := New(nil, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json"))
	in.AddNotes([]string{"omni runs on glm-4"})
	in.applyNoteDirectives("[note drop: omni runs on]\n[note: omni runs on glm-5.2]")
	got := in.Notes()
	if len(got) != 1 || got[0].Text != "omni runs on glm-5.2" {
		t.Errorf("got %+v, want only the corrected fact", got)
	}
}

func TestNotesAreTaggedWithProjectsTheyName(t *testing.T) {
	d := newScriptDriver()
	in := kingFixture(t, d)
	in.AddNotes([]string{"omni runs on glm-5.2", "the whole fleet uses Tailscale"})
	got := in.Notes()
	if len(got) != 2 {
		t.Fatalf("got %d notes", len(got))
	}
	if len(got[0].Projects) != 1 || got[0].Projects[0] != "omni" {
		t.Errorf("first note tags = %v, want [omni]", got[0].Projects)
	}
	// A cross-cutting fact names nobody and belongs to everybody.
	if len(got[1].Projects) != 0 {
		t.Errorf("second note tags = %v, want none", got[1].Projects)
	}
}

// A note about a deleted project can never be relevant again, and would go on
// costing context in every king turn forever.
func TestRemovingAProjectForgetsItsNotes(t *testing.T) {
	d := newScriptDriver()
	in := kingFixture(t, d)
	in.AddNotes([]string{"omni runs on glm-5.2", "the fleet uses Tailscale"})

	if err := in.RemoveProject(2); err != nil { // omni
		t.Fatalf("RemoveProject: %v", err)
	}
	got := in.Notes()
	if len(got) != 1 || got[0].Text != "the fleet uses Tailscale" {
		t.Errorf("got %+v, want only the cross-cutting note", got)
	}
}

// Only notes about the fleet in front of you are worth the context.
func TestOnlyRelevantNotesAreInjected(t *testing.T) {
	d := newScriptDriver()
	in := kingFixture(t, d)
	in.AddNotes([]string{"omni runs on glm-5.2", "akiroo is a monorepo", "the fleet uses Tailscale"})

	got := in.formatKingState([]string{"omni"})
	if !strings.Contains(got, "omni runs on glm-5.2") {
		t.Errorf("the note about omni was withheld:\n%s", got)
	}
	if !strings.Contains(got, "uses Tailscale") {
		t.Errorf("a cross-cutting note was withheld:\n%s", got)
	}
	if strings.Contains(got, "akiroo is a monorepo") {
		t.Errorf("a note about a project not in this fleet was injected:\n%s", got)
	}
}

// Eviction is not FIFO. The oldest note is the one that has survived longest
// without being contradicted; the newest is most likely passing status.
func TestEvictionDropsDeadAndNarrowNotesFirst(t *testing.T) {
	live := map[string]bool{"omni": true}

	var notes []Note
	notes = append(notes, Note{Text: "oldest cross-cutting fact"})
	for i := 0; i < maxNotes; i++ {
		notes = append(notes, Note{Text: "dead " + string(rune('a'+i%26)), Projects: []string{"deleted-project"}})
	}
	got := evict(notes, live)
	if len(got) > maxNotes {
		t.Fatalf("still %d notes, over the cap", len(got))
	}
	// The oldest note survives precisely because it is untagged.
	if got[0].Text != "oldest cross-cutting fact" {
		t.Errorf("the durable fact was evicted first: %+v", got[0])
	}
	for _, n := range got {
		if len(n.Projects) > 0 && !n.mentions(live) {
			t.Errorf("a note about a deleted project survived: %+v", n)
		}
	}
}

// Notes written before tagging existed have no Projects field and must keep
// being injected rather than silently vanishing.
func TestUntaggedLegacyNotesStayVisible(t *testing.T) {
	dir := t.TempDir()
	notesPath := filepath.Join(dir, "notes.json")
	if err := os.WriteFile(notesPath, []byte(`[{"text":"a fact from before tagging","created_at":"2026-07-01T00:00:00Z"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	in := New(nil, map[string]driver.Driver{}, filepath.Join(dir, "s.json")).WithNotesPath(notesPath)
	got := in.NotesFor(map[string]bool{"anything": true})
	if len(got) != 1 {
		t.Errorf("legacy note was dropped: %+v", got)
	}
}
