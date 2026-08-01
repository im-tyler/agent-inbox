package mux

import "testing"

func TestCleanTitleStripsEverySpinnerFrameNotJustKnownOnes(t *testing.T) {
	// The spinner cycles through the whole braille block. Enumerating the
	// frames that happened to be on screen left U+2802 attached, and the
	// title then matched no session at all.
	for _, glyph := range []string{"⠂", "⠐", "⠈", "⣀", "✳", "⏺", "*"} {
		if got := CleanTitle(glyph + " Fix the router"); got != "Fix the router" {
			t.Fatalf("glyph %q: got %q", glyph, got)
		}
	}
}

func TestCleanTitleDropsATrailingEllipsisFromATruncatedTitle(t *testing.T) {
	// Pane titles are cut to the pane width; the ellipsis left behind would
	// otherwise defeat the prefix comparison in FindByTitle.
	if got := CleanTitle("⠂ Nucleus performance optimiz…"); got != "Nucleus performance optimiz" {
		t.Fatalf("got %q", got)
	}
	if got := CleanTitle("Nucleus performance optimiz..."); got != "Nucleus performance optimiz" {
		t.Fatalf("got %q", got)
	}
}

func TestCleanTitleLeavesAnOrdinaryTitleAlone(t *testing.T) {
	if got := CleanTitle("  Review security findings  "); got != "Review security findings" {
		t.Fatalf("got %q", got)
	}
}

func panes() []Pane {
	return []Pane{
		{ID: "terminal_0", Title: "✳ Fix dropdown menu positioning and drag-drop"},
		{ID: "terminal_1", Title: "⠂ Design Fleet Vision supervisor loop architecture"},
		{ID: "terminal_2", Title: "✳ Claude Code"},
		{ID: "terminal_3", Title: "✳ Claude Code"},
		{ID: "terminal_4", Title: ""},
		{ID: "terminal_5", Title: "⠂ Nucleus performance optimiz…", Path: "/repos/neutron"},
	}
}

func TestFindByTitleMatchesExactlyAndThroughTruncation(t *testing.T) {
	p, ok := FindByTitle(panes(), "Design Fleet Vision supervisor loop architecture")
	if !ok || p.ID != "terminal_1" {
		t.Fatalf("exact match failed: %+v %v", p, ok)
	}
	// The pane title is the truncated one; the session knows the full text.
	p, ok = FindByTitle(panes(), "Nucleus performance optimization handoff")
	if !ok || p.ID != "terminal_5" {
		t.Fatalf("truncated match failed: %+v %v", p, ok)
	}
}

func TestFindByTitleRefusesAnAmbiguousMatch(t *testing.T) {
	// Two panes are both just "Claude Code". Guessing would send your
	// message to a different agent, so no pane is returned at all.
	if _, ok := FindByTitle(panes(), "Claude Code"); ok {
		t.Fatal("an ambiguous title must not resolve to a pane")
	}
}

func TestFindByTitleIgnoresUntitledPanesAndEmptyQueries(t *testing.T) {
	if _, ok := FindByTitle(panes(), ""); ok {
		t.Fatal("an empty session title must not match the untitled pane")
	}
	if _, ok := FindByTitle(panes(), "something nobody is running"); ok {
		t.Fatal("unexpected match")
	}
}

func TestFindByPathIsExactAndOnlyWorksWhereAPathIsReported(t *testing.T) {
	p, ok := FindByPath(panes(), "/repos/neutron")
	if !ok || p.ID != "terminal_5" {
		t.Fatalf("got %+v %v", p, ok)
	}
	// zellij reports no path, so those panes can never match this way.
	if _, ok := FindByPath(panes(), ""); ok {
		t.Fatal("an empty path must not match a pathless pane")
	}
	if _, ok := FindByPath(panes(), "/repos/other"); ok {
		t.Fatal("unexpected match")
	}
}

func TestFindByPathRefusesAmbiguity(t *testing.T) {
	two := []Pane{
		{ID: "a", Path: "/repos/x"},
		{ID: "b", Path: "/repos/x"},
	}
	if _, ok := FindByPath(two, "/repos/x"); ok {
		t.Fatal("two panes in one directory must not resolve")
	}
}
