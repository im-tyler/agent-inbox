package inbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

func memoryFixture(t *testing.T) *Inbox {
	t.Helper()
	projects := []*Project{
		{Name: "supervisor", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "neutron", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "teploy", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json")).
		WithKing("supervisor")
	t.Cleanup(in.Close)
	return in
}

// The whole point of the kind. A constraint naming neutron must reach a turn
// that is only about teploy — a rule that applies when its subject happens to
// be present is not a rule.
func TestAConstraintIsNotFilteredOutByProject(t *testing.T) {
	in := memoryFixture(t)
	in.AddConstraints([]string{"neutron stays on the free model"})
	in.AddNotes([]string{"neutron rewrote its parser"})

	ctx := in.formatKingState([]string{"teploy"})
	if !strings.Contains(ctx, "stays on the free model") {
		t.Errorf("the constraint was filtered out of a turn about another project:\n%s", ctx)
	}
	// The observation about neutron is correctly absent — that is what
	// filtering is for.
	if strings.Contains(ctx, "rewrote its parser") {
		t.Errorf("an unrelated observation was injected:\n%s", ctx)
	}
}

func TestPrioritiesAreAlsoStanding(t *testing.T) {
	in := memoryFixture(t)
	in.AddPriorities([]string{"teploy ships before anything else"})
	ctx := in.formatKingState([]string{"neutron"})
	if !strings.Contains(ctx, "ships before anything else") {
		t.Errorf("a priority was filtered out:\n%s", ctx)
	}
}

// A rule and an observation read identically in a bulleted list, and only one
// of them may override what the turn decides to do.
func TestStandingRulesAreRenderedSeparately(t *testing.T) {
	in := memoryFixture(t)
	in.AddConstraints([]string{"neutron stays on the free model"})
	in.AddNotes([]string{"teploy depends on the db layer"})

	ctx := in.formatKingState([]string{"neutron", "teploy"})
	rules := strings.Index(ctx, "Standing rules")
	noted := strings.Index(ctx, "What you have noted")
	if rules < 0 || noted < 0 {
		t.Fatalf("both blocks should be present:\n%s", ctx)
	}
	if rules > noted {
		t.Error("observations were placed above standing rules")
	}
	if !strings.Contains(ctx, "constraint: neutron stays") {
		t.Errorf("the rule is not labelled by kind:\n%s", ctx)
	}
}

func TestParsingTheStandingDirectives(t *testing.T) {
	resp := "thinking\n[constraint: neutron stays free]\n[priority: teploy first]\n[note: an ordinary fact]\n"
	if got := ParseKingConstraints(resp); len(got) != 1 || got[0] != "neutron stays free" {
		t.Errorf("constraints = %v", got)
	}
	if got := ParseKingPriorities(resp); len(got) != 1 || got[0] != "teploy first" {
		t.Errorf("priorities = %v", got)
	}
	if got := ParseKingNotes(resp); len(got) != 1 || got[0] != "an ordinary fact" {
		t.Errorf("notes = %v", got)
	}
}

// One retraction path for all three kinds — the supervisor should not have to
// remember which kind it filed something as in order to take it back.
func TestDropRetractsAnyKind(t *testing.T) {
	in := memoryFixture(t)
	in.AddConstraints([]string{"neutron stays on the free model"})
	in.AddPriorities([]string{"teploy ships first"})
	in.DropNotes([]string{"free model", "ships first"})
	if got := in.Notes(); len(got) != 0 {
		t.Errorf("notes remain after retraction: %+v", got)
	}
}

// Giving up a rule to make room for an observation is the wrong trade: the
// observation is re-derived from the next status line, the rule is not.
func TestEvictionGivesUpFactsBeforeRules(t *testing.T) {
	in := memoryFixture(t)
	in.AddConstraints([]string{"the one rule that must survive"})
	facts := make([]string, 0, maxNotes+10)
	for i := range maxNotes + 10 {
		facts = append(facts, "ordinary observation number "+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('0'+i%10))+"-"+itoa(i))
	}
	in.AddNotes(facts)

	found := false
	for _, n := range in.Notes() {
		if strings.Contains(n.Text, "the one rule that must survive") {
			found = true
		}
	}
	if !found {
		t.Errorf("the constraint was evicted to make room for observations (%d notes kept)", len(in.Notes()))
	}
	if len(in.Notes()) > maxNotes {
		t.Errorf("the store is over its cap: %d", len(in.Notes()))
	}
}

// A standing rule outlives the project it names — dropping it because a
// project was renamed silently repeals it.
func TestAStandingRuleSurvivesItsProjectDisappearing(t *testing.T) {
	in := memoryFixture(t)
	in.AddConstraints([]string{"neutron stays on the free model"})
	in.AddNotes([]string{"neutron rewrote its parser"})

	if err := in.RemoveProject(2); err != nil { // neutron
		t.Fatalf("RemoveProject: %v", err)
	}
	var texts []string
	for _, n := range in.Notes() {
		texts = append(texts, n.Text)
	}
	joined := strings.Join(texts, " | ")
	if !strings.Contains(joined, "free model") {
		t.Errorf("the constraint went with the project: %s", joined)
	}
}

// Notes written before kinds existed keep behaving exactly as they did.
func TestAKindlessNoteIsAFact(t *testing.T) {
	var n Note
	if n.Kind.Standing() {
		t.Error("a note with no kind reported as standing")
	}
	if !n.mentions(map[string]bool{"anything": true}) {
		t.Error("an untagged note stopped being general")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
