package inbox

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

func autonomyFixture(t *testing.T) *Inbox {
	t.Helper()
	projects := []*Project{
		{Name: "sup-a", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "sup-b", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "alpha", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "beta", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json")).
		WithGroups([]Group{
			{Name: "a", King: "sup-a", Projects: []string{"alpha"}},
			{Name: "b", King: "sup-b", Projects: []string{"beta"}},
		})
	t.Cleanup(in.Close)
	return in
}

// Off is the default, and the default is the feature. Anything else spends
// money the first time a hook fires.
func TestAutonomyIsOffByDefault(t *testing.T) {
	in := autonomyFixture(t)
	if o := in.WithAutonomy(false, 0, nil); o != nil {
		t.Fatal("autonomy was created while disabled")
	}
	if in.oversight() != nil {
		t.Error("a disabled overseer was installed on the inbox")
	}
	// Every call has to tolerate the nil, so no caller has to branch.
	in.oversight().Notice("alpha", "done")
	in.oversight().Stop()
}

func TestWakeBudgetIsClamped(t *testing.T) {
	in := autonomyFixture(t)
	if got := in.WithAutonomy(true, 0, nil).perHour; got != DefaultWakesPerHour {
		t.Errorf("perHour = %d with none configured, want the default", got)
	}
	if got := in.WithAutonomy(true, 9999, nil).perHour; got != maxWakesPerHour {
		t.Errorf("perHour = %d, want it clamped to %d", got, maxWakesPerHour)
	}
}

// A fleet finishing together is one wake, not one per project. Three turns
// that each see a slice would dispatch against a fleet still in motion.
func TestTriggersAreCoalescedIntoOneWake(t *testing.T) {
	in := autonomyFixture(t)
	o := in.WithAutonomy(true, 10, nil)
	o.coalesce = 20 * time.Millisecond

	o.Notice("alpha", "done")
	o.Notice("beta", "done")
	o.Notice("alpha", "done")

	waitFor(t, "sup-a to wake", func() bool { return len(kingHistory(in, "sup-a")) > 0 })
	waitFor(t, "sup-b to wake", func() bool { return len(kingHistory(in, "sup-b")) > 0 })

	// One wake each: two groups, one project apiece.
	if got := countWakeLines(in, "sup-a"); got != 1 {
		t.Errorf("sup-a woke %d times, want 1", got)
	}
	if got := countWakeLines(in, "sup-b"); got != 1 {
		t.Errorf("sup-b woke %d times, want 1", got)
	}
}

// A trigger only reaches the supervisor that owns that project. The partition
// is the point, and an autonomous turn is the easiest place to leak it.
func TestAWakeGoesOnlyToTheOwningGroup(t *testing.T) {
	in := autonomyFixture(t)
	o := in.WithAutonomy(true, 10, nil)
	o.coalesce = 20 * time.Millisecond

	o.Notice("beta", "done")
	waitFor(t, "sup-b to wake", func() bool { return len(kingHistory(in, "sup-b")) > 0 })

	if got := countWakeLines(in, "sup-a"); got != 0 {
		t.Errorf("sup-a woke on another group's project (%d times)", got)
	}
}

// The user being mid-message outranks everything: a turn starting under them
// changes the fleet their message was about.
func TestNoWakeWhileTheUserIsTyping(t *testing.T) {
	in := autonomyFixture(t)
	in.SetDraft(true)
	o := in.WithAutonomy(true, 10, in.HasDraft)
	o.coalesce = 10 * time.Millisecond

	o.Notice("alpha", "done")
	time.Sleep(120 * time.Millisecond)
	if got := countWakeLines(in, "sup-a"); got != 0 {
		t.Errorf("woke %d times while a draft was unsent", got)
	}
}

// The budget is a rolling hour, not a counter that never resets.
func TestWakeBudgetIsARollingHour(t *testing.T) {
	in := autonomyFixture(t)
	o := in.WithAutonomy(true, 2, nil)
	now := time.Now()
	o.nowFn = func() time.Time { return now }

	if !o.spendWake() || !o.spendWake() {
		t.Fatal("the first two wakes were refused")
	}
	if o.spendWake() {
		t.Error("a third wake was allowed on a budget of two")
	}
	// An hour on, the first two have aged out.
	now = now.Add(time.Hour + time.Minute)
	if !o.spendWake() {
		t.Error("the budget did not roll over")
	}
}

// Spending the budget stops the wake and says so. Silence would read as the
// supervisor deciding nothing needed doing.
//
// The budget is pre-spent rather than exhausted by running a real turn: doing
// it the other way entangles the assertion with a turn's lifecycle, and the
// supervisor's status racing between Working and Waiting decides the outcome
// instead of the budget.
func TestBudgetExhaustionIsRecordedNotSilent(t *testing.T) {
	in := autonomyFixture(t)
	o := in.WithAutonomy(true, 1, nil)
	if !o.spendWake() {
		t.Fatal("setup: the budget was already spent")
	}

	o.wakeGroup(0, map[string]string{"alpha": "done"})

	hist := kingHistory(in, "sup-a")
	if !strings.Contains(hist, "wake budget is spent") {
		t.Errorf("the refusal was not recorded:\n%s", hist)
	}
	if countWakeLines(in, "sup-a") != 0 {
		t.Error("a turn was started despite the budget being spent")
	}
}

// A wake that fires after shutdown would start a turn into an inbox that is
// closing.
func TestStopCancelsAScheduledWake(t *testing.T) {
	in := autonomyFixture(t)
	o := in.WithAutonomy(true, 10, nil)
	o.coalesce = 200 * time.Millisecond

	o.Notice("alpha", "done")
	o.Stop()
	time.Sleep(300 * time.Millisecond)
	if got := countWakeLines(in, "sup-a"); got != 0 {
		t.Errorf("a cancelled wake still fired (%d times)", got)
	}
}

// The prompt has to bound what an unattended turn may do. An open-ended one
// produces prose nobody reads and actions nobody sanctioned.
func TestAutonomousPromptIsAClosedVocabulary(t *testing.T) {
	p := autonomousPrompt(map[string]string{"alpha": "permission", "beta": "done"})
	for _, want := range []string{"UNBLOCK", "REPROMPT", "PARK", "ESCALATE", "Default to PARK"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// The triggers themselves, so the turn knows why it exists.
	for _, want := range []string{"alpha: permission", "beta: done"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt does not state the trigger %q:\n%s", want, p)
		}
	}
	// Deterministic ordering, so two identical wakes read identically.
	if strings.Index(p, "alpha") > strings.Index(p, "beta") {
		t.Error("triggers are not in a stable order")
	}
}

func kingHistory(in *Inbox, name string) string {
	var b strings.Builder
	for _, p := range in.Snapshot() {
		if p.Name != name {
			continue
		}
		for _, m := range p.History {
			b.WriteString(m.Content + "\n")
		}
	}
	return b.String()
}

func countWakeLines(in *Inbox, name string) int {
	n := 0
	for _, ln := range strings.Split(kingHistory(in, name), "\n") {
		if strings.HasPrefix(ln, "woke: ") {
			n++
		}
	}
	return n
}
