package inbox

import (
	"path/filepath"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

func identityFixture(t *testing.T) *Inbox {
	t.Helper()
	projects := []*Project{
		{Name: "supervisor", Tool: "mock", Dir: "/data/supervisor", Status: driver.StatusIdle},
		{Name: "omni", Tool: "mock", Dir: "/repo/omni", Status: driver.StatusIdle},
		{Name: "akiroo", Tool: "mock", Dir: "/repo/akiroo", Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(in.Close)
	return in.WithKing("supervisor")
}

// The supervisor is found by name. It used to be "whichever project is first",
// which made the supervisor an accident of config ordering.
func TestKingResolvesByName(t *testing.T) {
	in := identityFixture(t)
	if got := in.KingIndex(); got != 1 {
		t.Errorf("KingIndex() = %d, want 1", got)
	}
	if !in.IsKing("SUPERVISOR") {
		t.Error("IsKing is case sensitive")
	}
	if in.IsKing("omni") {
		t.Error("a fleet project reported as king")
	}
}

// The index has to follow the supervisor when the list moves underneath it.
func TestKingIndexFollowsRemoval(t *testing.T) {
	projects := []*Project{
		{Name: "omni", Tool: "mock", Dir: "/repo/omni", Status: driver.StatusIdle},
		{Name: "supervisor", Tool: "mock", Dir: "/data/supervisor", Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json")).
		WithKing("supervisor")
	t.Cleanup(in.Close)

	if got := in.KingIndex(); got != 2 {
		t.Fatalf("KingIndex() = %d, want 2", got)
	}
	if err := in.RemoveProject(1); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	// A stored index would still say 2 and read off the end of the slice.
	if got := in.KingIndex(); got != 1 {
		t.Errorf("KingIndex() = %d after removal, want 1", got)
	}
}

// Removing the supervisor would leave a dashboard with no conversation and no
// way to get one back.
func TestKingCannotBeRemoved(t *testing.T) {
	in := identityFixture(t)
	if err := in.RemoveProject(in.KingIndex()); err == nil {
		t.Fatal("the supervisor was removed")
	}
	if len(in.Snapshot()) != 3 {
		t.Errorf("project count changed: %d", len(in.Snapshot()))
	}
}

// The fleet is everything except the supervisor. A king that could dispatch to
// itself would wait for a reply from the session doing the waiting.
func TestFleetExcludesTheKing(t *testing.T) {
	in := identityFixture(t)
	got := in.FleetNames()
	if len(got) != 2 || got[0] != "omni" || got[1] != "akiroo" {
		t.Errorf("FleetNames() = %v, want [omni akiroo]", got)
	}
}

// With no king configured every project is dispatchable and nothing is king —
// the state a plain project list is in before a supervisor exists.
func TestNoKingConfigured(t *testing.T) {
	in := identityFixture(t).WithKing("")
	if got := in.KingIndex(); got != 0 {
		t.Errorf("KingIndex() = %d, want 0", got)
	}
	if in.IsKing("supervisor") {
		t.Error("IsKing true with no king configured")
	}
	if got := in.FleetNames(); len(got) != 3 {
		t.Errorf("FleetNames() = %v, want all three", got)
	}
}
