package inbox

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// groupFixture is two supervisors over four projects, with one project
// deliberately left out of every group.
func groupFixture(t *testing.T) *Inbox {
	t.Helper()
	projects := []*Project{
		{Name: "sup-infra", Tool: "mock", Dir: "/data/infra", Status: driver.StatusIdle},
		{Name: "sup-product", Tool: "mock", Dir: "/data/product", Status: driver.StatusIdle},
		{Name: "teploy", Tool: "mock", Dir: "/repo/teploy", Status: driver.StatusIdle},
		{Name: "infra", Tool: "mock", Dir: "/repo/infra", Status: driver.StatusIdle},
		{Name: "neutron", Tool: "mock", Dir: "/repo/neutron", Status: driver.StatusIdle},
		{Name: "stray", Tool: "mock", Dir: "/repo/stray", Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(in.Close)
	return in.WithGroups([]Group{
		{Name: "infra", King: "sup-infra", Projects: []string{"teploy", "infra"}},
		{Name: "product", King: "sup-product", Projects: []string{"neutron"}},
	})
}

func TestFleetIsScopedToItsGroup(t *testing.T) {
	in := groupFixture(t)

	// The first group also absorbs "stray", which no group claimed.
	got := in.FleetNamesOf(0)
	want := []string{"teploy", "infra", "stray"}
	if !slices.Equal(got, want) {
		t.Errorf("FleetNamesOf(0) = %v, want %v", got, want)
	}
	if got := in.FleetNamesOf(1); !slices.Equal(got, []string{"neutron"}) {
		t.Errorf("FleetNamesOf(1) = %v, want [neutron]", got)
	}
}

// A supervisor is never in anybody's fleet — not its own, where it would wait
// on itself, and not another's, where the partition would leak.
func TestNoSupervisorAppearsInAnyFleet(t *testing.T) {
	in := groupFixture(t)
	for g := range in.GroupCount() {
		for _, n := range in.FleetNamesOf(g) {
			if in.IsKing(n) {
				t.Errorf("group %d fleet contains supervisor %q", g, n)
			}
		}
	}
}

func TestKingIndexIsPerGroup(t *testing.T) {
	in := groupFixture(t)
	if got := in.KingIndexOf(0); got != 1 {
		t.Errorf("KingIndexOf(0) = %d, want 1", got)
	}
	if got := in.KingIndexOf(1); got != 2 {
		t.Errorf("KingIndexOf(1) = %d, want 2", got)
	}
	// Out of range is "no supervisor", not a panic and not group zero's.
	if got := in.KingIndexOf(7); got != 0 {
		t.Errorf("KingIndexOf(7) = %d, want 0", got)
	}
}

// IsKing does not depend on which tab is open: rendering asks it of every row.
func TestIsKingSpansGroups(t *testing.T) {
	in := groupFixture(t)
	for _, name := range []string{"sup-infra", "SUP-PRODUCT"} {
		if !in.IsKing(name) {
			t.Errorf("IsKing(%q) = false", name)
		}
	}
	if in.IsKing("neutron") {
		t.Error("a fleet project reported as a supervisor")
	}
}

// Every supervisor's name is reserved, not just the one whose tab is open.
func TestEverySupervisorNameIsReserved(t *testing.T) {
	in := groupFixture(t)
	if err := in.AddProject("sup-product", "mock", "/repo/new"); err == nil {
		t.Error("a project took another group's supervisor name")
	}
}

func TestNoSupervisorCanBeRemoved(t *testing.T) {
	in := groupFixture(t)
	for g := range in.GroupCount() {
		if err := in.RemoveProject(in.KingIndexOf(g)); err == nil {
			t.Errorf("group %d supervisor was removed", g)
		}
	}
	if len(in.Snapshot()) != 6 {
		t.Errorf("project count changed: %d", len(in.Snapshot()))
	}
}

// A project added at runtime belongs to somebody. Landing in the first group
// is arbitrary, but a project in no group would sit in the fleet unreachable
// and unasked, which is worse.
func TestAnAddedProjectJoinsTheFirstGroup(t *testing.T) {
	in := groupFixture(t)
	if err := in.AddProject("fresh", "mock", "/repo/fresh"); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	if !slices.Contains(in.FleetNamesOf(0), "fresh") {
		t.Errorf("FleetNamesOf(0) = %v, want it to contain fresh", in.FleetNamesOf(0))
	}
	if slices.Contains(in.FleetNamesOf(1), "fresh") {
		t.Error("a new project appeared in a group that did not claim it")
	}
}

func TestGroupOfProject(t *testing.T) {
	in := groupFixture(t)
	cases := map[string]int{
		"sup-infra":   0,
		"teploy":      0,
		"stray":       0, // unclaimed, and FleetNamesOf puts it in group 0 too
		"sup-product": 1,
		"neutron":     1,
	}
	for name, want := range cases {
		if got := in.GroupOfProject(name); got != want {
			t.Errorf("GroupOfProject(%q) = %d, want %d", name, got, want)
		}
	}
}

// An unconfigured fleet is one anonymous group over everything, which is what
// this program was before groups existed.
func TestNoGroupsMeansOneGroupOverEverything(t *testing.T) {
	in := groupFixture(t).WithGroups(nil)
	if got := in.GroupCount(); got != 1 {
		t.Fatalf("GroupCount() = %d, want 1", got)
	}
	// No king named, so nothing is filtered out.
	if got := in.FleetNamesOf(0); len(got) != 6 {
		t.Errorf("FleetNamesOf(0) = %v, want all six", got)
	}
}

// WithKing is the single-supervisor shorthand and has to behave exactly as it
// did before groups: one king, everything else its fleet.
func TestWithKingIsOneGroup(t *testing.T) {
	in := groupFixture(t).WithKing("sup-infra")
	if got := in.GroupCount(); got != 1 {
		t.Fatalf("GroupCount() = %d, want 1", got)
	}
	got := in.FleetNamesOf(0)
	want := []string{"sup-product", "teploy", "infra", "neutron", "stray"}
	if !slices.Equal(got, want) {
		t.Errorf("FleetNamesOf(0) = %v, want %v", got, want)
	}
}
