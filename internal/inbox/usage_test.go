package inbox

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/usage"
)

type stubUsage struct {
	snap usage.Snapshot
	err  error
	n    int
}

func (s *stubUsage) Name() string { return "stub" }
func (s *stubUsage) Read() (usage.Snapshot, error) {
	s.n++
	return s.snap, s.err
}

func usageFixture(t *testing.T) *Inbox {
	t.Helper()
	projects := []*Project{
		{Name: "supervisor", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "omni", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json")).
		WithKing("supervisor")
	t.Cleanup(in.Close)
	return in
}

func liveSnapshot() usage.Snapshot {
	return usage.Snapshot{
		Estimated: true,
		Account:   "you@example.com",
		Block:     usage.Window{Input: 1000, Output: 2000, CacheCreate: 3000, CacheRead: 400000},
		ResetsAt:  time.Now().Add(2 * time.Hour),
	}
}

// With no source configured the capacity line is absent, not zero. A zero
// reads as "nothing left", which is the opposite of "nothing known".
func TestNoUsageSourceMeansNoCapacityLine(t *testing.T) {
	in := usageFixture(t)
	if got := in.Usage(); !got.Block.Empty() {
		t.Errorf("Usage() = %+v with no source", got)
	}
	if ctx := in.formatKingState([]string{"omni"}); strings.Contains(ctx, "Capacity") {
		t.Errorf("a capacity line appeared with no source:\n%s", ctx)
	}
}

func TestCapacityReachesTheKing(t *testing.T) {
	in := usageFixture(t)
	in.WithUsage(&stubUsage{snap: liveSnapshot()})
	in.RefreshUsage()

	ctx := in.formatKingState([]string{"omni"})
	for _, want := range []string{"Capacity (estimated", "burn not remaining", "you@example.com", "5h block"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("injected context missing %q:\n%s", want, ctx)
		}
	}
	// The supervisor must never be handed a "remaining" figure — no published
	// denominator exists to compute one from.
	if strings.Contains(ctx, "remaining:") {
		t.Errorf("a remaining figure was injected:\n%s", ctx)
	}
}

// Cache reads outweigh everything else by a factor of fifty on real
// transcripts, so a single total would read as fifty times the work done.
func TestSummaryDoesNotLeadWithCacheReads(t *testing.T) {
	s := liveSnapshot()
	got := s.Summary()
	if !strings.Contains(got, "6.0k") {
		t.Errorf("Summary() = %q, want the non-cache tokens as the headline", got)
	}
	if !strings.Contains(got, "cached") {
		t.Errorf("Summary() = %q, want cache named separately", got)
	}
}

// A transient read failure must not blank a figure that was right a moment ago.
func TestAFailedReadKeepsTheLastGoodSnapshot(t *testing.T) {
	in := usageFixture(t)
	src := &stubUsage{snap: liveSnapshot()}
	in.WithUsage(src)
	in.RefreshUsage()
	before := in.Usage().Block.Total()

	src.err = errors.New("disk went away")
	in.RefreshUsage()
	if got := in.Usage().Block.Total(); got != before {
		t.Errorf("total = %d after a failed read, want the last good %d", got, before)
	}
}

// The read is on a timer, not the render path: the first scan of a long
// history takes about a second.
func TestUsageIsNotReadOnEveryQuery(t *testing.T) {
	in := usageFixture(t)
	src := &stubUsage{snap: liveSnapshot()}
	in.WithUsage(src)
	in.RefreshUsage()
	for range 50 {
		in.Usage()
	}
	if src.n != 1 {
		t.Errorf("source read %d times, want 1 — Usage() must serve the cached snapshot", src.n)
	}
}
