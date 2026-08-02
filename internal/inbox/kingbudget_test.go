package inbox

import (
	"strings"
	"testing"
)

func kingSystemLine(in *Inbox, substr string) bool {
	for _, m := range in.Snapshot()[0].History {
		if m.Role == "system" && strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

// The default is one dispatch round: the king asks, reads the replies, and
// answers. A directive in the summary is not dispatched — that is the guard
// against a supervisor answering its own summary with more work, forever.
func TestDefaultBudgetStopsAfterOneRound(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{
		"[send to omni: what are you on?]",
		"omni is on ingest.\n[send to akiroo: and you?]",
	}
	d.replies["/omni"] = []string{"ingest work"}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "status", []string{"omni", "akiroo"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}

	// Silence would be indistinguishable from the king choosing not to follow
	// up. The note is the only thing that says the budget stopped it.
	waitFor(t, "the spent-budget note", func() bool {
		return kingSystemLine(in, "round budget is spent")
	})
	if got := d.promptsFor("/akiroo"); len(got) != 0 {
		t.Errorf("akiroo was dispatched past the budget: %v", got)
	}
}

// With budget to spend, the king can act on what a reply revealed instead of
// telling the user to ask again — the case where omni's answer is what makes
// akiroo worth asking.
func TestBudgetAllowsAFollowUpRound(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{
		"[send to omni: what are you on?]",
		"omni depends on akiroo.\n[send to akiroo: are you blocking omni?]",
		"akiroo is not blocking. All clear.",
	}
	d.replies["/omni"] = []string{"ingest work, depends on akiroo"}
	d.replies["/akiroo"] = []string{"not blocking anyone"}

	in := kingFixture(t, d)
	in.WithKingRounds(2)
	if err := in.KingSend(1, "status", []string{"omni", "akiroo"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}

	waitFor(t, "the second round's dispatch", func() bool {
		return len(d.promptsFor("/akiroo")) == 1
	})
	waitFor(t, "the second summary", func() bool {
		return len(d.promptsFor("/king")) == 3
	})
	if got := d.promptsFor("/akiroo")[0]; !strings.Contains(got, "blocking omni") {
		t.Errorf("akiroo got %q, want the follow-up question", got)
	}
	// The round is visible in the thread, not just in the outcome.
	if !kingSystemLine(in, "follow-up round") {
		t.Error("no receipt for the follow-up round")
	}
	// And the last summary must be terminal, or the budget did not decrement.
	if !strings.Contains(d.promptsFor("/king")[2], "will not be dispatched") {
		t.Errorf("final summary still invites directives:\n%s", d.promptsFor("/king")[2])
	}
}

// A king that asks the same project the same thing twice running is looping,
// not working. The budget alone would let it burn every remaining round.
func TestRepeatedDispatchStopsTheRound(t *testing.T) {
	d := newScriptDriver()
	same := "[send to omni: what are you on?]"
	d.replies["/king"] = []string{same, "still unclear.\n" + same}
	d.replies["/omni"] = []string{"ingest work"}

	in := kingFixture(t, d)
	in.WithKingRounds(4)
	if err := in.KingSend(1, "status", []string{"omni"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}

	waitFor(t, "the repeat to be caught", func() bool {
		return kingSystemLine(in, "repeated its previous dispatch")
	})
	if got := d.promptsFor("/omni"); len(got) != 1 {
		t.Errorf("omni was asked %d times, want 1: %v", len(got), got)
	}
}

// A budget above the cap is clamped rather than honoured. A budget large
// enough to be indistinguishable from no budget would not be one.
func TestKingRoundsClamped(t *testing.T) {
	in := kingFixture(t, newScriptDriver())
	if got := in.kingRounds(); got != defaultKingRounds {
		t.Errorf("default rounds = %d, want %d", got, defaultKingRounds)
	}
	if got := in.WithKingRounds(99).kingRounds(); got != maxKingRounds {
		t.Errorf("rounds = %d, want clamped to %d", got, maxKingRounds)
	}
	if got := in.WithKingRounds(-1).kingRounds(); got != defaultKingRounds {
		t.Errorf("rounds = %d, want the default for a nonsense value", got)
	}
}
