package feed

import "testing"

func item(id string, a Attention, since string) Item {
	return Item{Schema: Schema, Source: "s", ID: id, Attention: a, Since: since, Origin: "o"}
}

func ids(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSortPutsDecisionsFirstThenOldestWithinBand(t *testing.T) {
	items := []Item{
		item("info", AttentionInfo, "2026-07-01T00:00:00Z"),
		item("fail", AttentionFailure, "2026-07-02T00:00:00Z"),
		item("new-decision", AttentionDecision, "2026-07-03T00:00:00Z"),
		item("old-decision", AttentionDecision, "2026-07-01T00:00:00Z"),
	}
	Sort(items)
	// Oldest decision leads: the thing blocked longest is the thing quietly
	// costing the most.
	equal(t, ids(items), []string{"old-decision", "new-decision", "fail", "info"})
}

func TestSortIsStableForIdenticalTimestamps(t *testing.T) {
	items := []Item{
		item("b", AttentionDecision, "2026-07-01T00:00:00Z"),
		item("a", AttentionDecision, "2026-07-01T00:00:00Z"),
	}
	Sort(items)
	equal(t, ids(items), []string{"a", "b"})
}

func TestMergeDedupesByOriginSourceAndIDWithLaterWinning(t *testing.T) {
	stale := item("run-1", AttentionInfo, "2026-07-01T00:00:00Z")
	stale.Title = "stale"
	fresh := item("run-1", AttentionInfo, "2026-07-01T00:00:00Z")
	fresh.Title = "fresh"

	merged := Merge([]Feed{{Items: []Item{stale}}, {Items: []Item{fresh}}})
	if len(merged) != 1 {
		t.Fatalf("expected one item after dedupe, got %d", len(merged))
	}
	if merged[0].Title != "fresh" {
		t.Fatalf("later feed should win, got %q", merged[0].Title)
	}
}

func TestMergeKeepsSameIDFromDifferentOrigins(t *testing.T) {
	a := item("run-1", AttentionInfo, "2026-07-01T00:00:00Z")
	a.Origin = "ship-local"
	b := item("run-1", AttentionInfo, "2026-07-01T00:00:00Z")
	b.Origin = "ship-prod"

	// IDs are unique only within a source; two Ship instances legitimately
	// both have a "run-1".
	if got := len(Merge([]Feed{{Items: []Item{a, b}}})); got != 2 {
		t.Fatalf("expected both instances kept, got %d", got)
	}
}

func TestNormalizeClampsUnknownStateAndDerivesAttention(t *testing.T) {
	i := Item{State: "exploded", Title: "  many   spaces \n here "}
	i.Normalize("src")

	if i.State != StatePending {
		t.Fatalf("unknown state should clamp to pending, got %q", i.State)
	}
	if i.Attention != AttentionInfo {
		t.Fatalf("attention should derive from state, got %q", i.Attention)
	}
	if i.Title != "many spaces here" {
		t.Fatalf("title should collapse to one line, got %q", i.Title)
	}
	if i.Origin != "src" || i.Source != "src" || i.Schema != Schema {
		t.Fatalf("normalize should fill origin/source/schema, got %+v", i)
	}
}

func TestNormalizeGivesABlockedItemAPromptEvenWithoutOne(t *testing.T) {
	i := Item{State: StateBlocked}
	i.Normalize("src")

	if i.Attention != AttentionDecision {
		t.Fatalf("blocked implies a decision, got %q", i.Attention)
	}
	if i.Needs == nil || i.Needs.Prompt == "" {
		t.Fatal("a blocked item must never render an empty question")
	}
}

func TestNormalizeBackfillsMissingTimestampsFromEachOther(t *testing.T) {
	i := Item{State: StateRunning, UpdatedAt: "2026-07-01T00:00:00Z"}
	i.Normalize("src")
	if i.Since != "2026-07-01T00:00:00Z" {
		t.Fatalf("since should fall back to updated_at, got %q", i.Since)
	}
}

func TestSinceTimeFallsBackRatherThanDroppingTheItem(t *testing.T) {
	i := Item{Since: "not-a-date", UpdatedAt: "2026-07-01T00:00:00Z"}
	if i.SinceTime().IsZero() {
		t.Fatal("a malformed since should fall back to updated_at")
	}
	bad := Item{Since: "nope", UpdatedAt: "also-nope"}
	if !bad.SinceTime().IsZero() {
		t.Fatal("unparseable timestamps should sort to the top, not error")
	}
}

func TestDecisionsCountsOnlyWhatWaitsOnAHuman(t *testing.T) {
	items := []Item{
		item("a", AttentionDecision, ""),
		item("b", AttentionFailure, ""),
		item("c", AttentionInfo, ""),
		item("d", AttentionDecision, ""),
	}
	if got := Decisions(items); got != 2 {
		t.Fatalf("got %d decisions, want 2", got)
	}
}
