package inbox

import (
	"path/filepath"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// A constraint that comes back as a fact after a restart has been silently
// demoted: it would start being filtered, evicted early, and swept when its
// project is removed.
func TestNoteKindSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.json")
	projects := []*Project{{Name: "supervisor", Tool: "mock", Dir: t.TempDir()}}

	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(dir, "state.json")).
		WithKing("supervisor").WithNotesPath(notes)
	in.AddConstraints([]string{"neutron stays on the free model"})
	in.AddPriorities([]string{"teploy ships first"})
	in.AddNotes([]string{"an ordinary observation"})
	in.Close()

	again := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(dir, "state.json")).
		WithKing("supervisor").WithNotesPath(notes)
	t.Cleanup(again.Close)

	kinds := map[string]Kind{}
	for _, n := range again.Notes() {
		kinds[n.Text] = n.Kind
	}
	if len(kinds) != 3 {
		t.Fatalf("reloaded %d notes, want 3: %+v", len(kinds), kinds)
	}
	if kinds["neutron stays on the free model"] != KindConstraint {
		t.Errorf("constraint came back as %q", kinds["neutron stays on the free model"])
	}
	if kinds["teploy ships first"] != KindPriority {
		t.Errorf("priority came back as %q", kinds["teploy ships first"])
	}
	if kinds["an ordinary observation"] != KindFact {
		t.Errorf("fact came back as %q", kinds["an ordinary observation"])
	}
}
