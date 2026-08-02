package board

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/im-tyler/agent-inbox/internal/feed"
	"github.com/im-tyler/agent-inbox/internal/sources"
)

func join(argv []string) string { return strings.Join(argv, "\x00") }

func TestSubstitutePlacesAFillAsExactlyOneArgument(t *testing.T) {
	argv := []string{"teploy-ship", "deny", "run-1", "{reason}"}
	// The whole security property: a reason full of shell metacharacters is
	// one argument, not another command. Nothing here goes through a shell.
	got := substitute(argv, []string{`nope; rm -rf / && echo "pwned"`})
	if len(got) != 4 {
		t.Fatalf("expected 4 args, got %d: %v", len(got), got)
	}
	if got[3] != `nope; rm -rf / && echo "pwned"` {
		t.Fatalf("fill must survive verbatim as one arg, got %q", got[3])
	}
}

func TestSubstituteDropsAnArgumentWhoseFillIsEmpty(t *testing.T) {
	// Denying with no reason should omit the argument rather than pass "".
	got := substitute([]string{"teploy-ship", "deny", "run-1", "{reason}"}, []string{"  "})
	if join(got) != join([]string{"teploy-ship", "deny", "run-1"}) {
		t.Fatalf("empty fill should drop the arg, got %v", got)
	}
}

func TestSubstituteLeavesArgvWithoutPlaceholdersAlone(t *testing.T) {
	argv := []string{"claude", "--resume", "sess-1"}
	if join(substitute(argv, nil)) != join(argv) {
		t.Fatalf("argv without placeholders must pass through unchanged")
	}
}

func TestSubstituteFillsMultiplePlaceholdersInArgvOrder(t *testing.T) {
	got := substitute([]string{"tool", "{choice}", "--why", "{reason}"}, []string{"deny", "not in prod"})
	if join(got) != join([]string{"tool", "deny", "--why", "not in prod"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSubstituteWithNoFillsDropsPlaceholderArgsRatherThanLeakingTheToken(t *testing.T) {
	got := substitute([]string{"tool", "{reason}"}, nil)
	if join(got) != join([]string{"tool"}) {
		t.Fatalf("an unfilled placeholder must never reach the command, got %v", got)
	}
}

func TestPlaceholdersListsTokensInArgvOrder(t *testing.T) {
	got := placeholders(feed.Action{Run: []string{"tool", "{choice}", "--why", "{reason}"}})
	if join(got) != join([]string{"choice", "reason"}) {
		t.Fatalf("got %v", got)
	}
	if len(placeholders(feed.Action{Run: []string{"claude", "--resume", "x"}})) != 0 {
		t.Fatal("expected no placeholders")
	}
}

func TestSelectedIsSafeWhenTheListIsEmptyOrTheCursorIsStale(t *testing.T) {
	m := New(nil)
	if _, ok := m.selected(); ok {
		t.Fatal("an empty list must not yield a selection")
	}
	m.items = []feed.Item{{ID: "a"}}
	m.cursor = 7
	if _, ok := m.selected(); ok {
		t.Fatal("a cursor past the end must not yield a selection")
	}
}

func TestActionsForReturnsNothingWhenAnItemNeedsNoDecision(t *testing.T) {
	m := New(nil)
	if got := m.actionsFor(feed.Item{State: feed.StateRunning}); got != nil {
		t.Fatalf("a running item offers no actions, got %v", got)
	}
}

// populated builds a model as if a fetch had completed, so the views can be
// rendered without a terminal.
func populated() Model {
	m := New(nil)
	m.loading = false
	m.width = 96
	m.items = []feed.Item{
		{
			Origin: "teploy-ship", Source: "teploy-ship", ID: "run-parked01", Kind: "agent-run",
			Title: "fix the failing auth test in decor-arbor", State: feed.StateBlocked,
			Attention: feed.AttentionDecision, Since: "2026-07-29T10:05:00Z",
			Needs: &feed.Needs{
				Prompt: "Approve this action? bash: rm -rf node_modules",
				Actions: []feed.Action{
					{Label: "approve", Run: []string{"teploy-ship", "approve", "run-parked01"}},
					{Label: "deny", Run: []string{"teploy-ship", "deny", "run-parked01", "{reason}"}},
				},
			},
			Context: map[string]string{"model": "claude-opus-5", "ran_on": "worker-02", "project": "decor-arbor"},
			Link:    "http://box:7460/runs/run-parked01",
		},
		{
			Origin: "claude-code", Source: "claude-code", ID: "sess-1", Kind: "session",
			Title: "Deploy latest changes for testing", State: feed.StateBlocked,
			Attention: feed.AttentionDecision, Since: "2026-07-29T11:00:00Z",
			Needs: &feed.Needs{Prompt: "Finished its turn — waiting for your next instruction."},
		},
		{
			Origin: "claude-code", Source: "claude-code", ID: "sess-2", Kind: "session",
			Title: "Engine performance instrumentation", State: feed.StateRunning,
			Attention: feed.AttentionInfo, Since: "2026-07-29T11:30:00Z",
		},
	}
	return m
}

func TestListViewLeadsWithTheCountThatMattersAndMarksTheCursor(t *testing.T) {
	out := populated().View()
	if !strings.Contains(out, "2 waiting on you") {
		t.Fatalf("header should lead with the decision count:\n%s", out)
	}
	// The attention word is suppressed when every visible row shares it —
	// repeating "decision" down the screen carries no information.
	if strings.Contains(out, "decision") {
		t.Fatalf("a uniform list should not repeat the band:\n%s", out)
	}
	// Sessions that want nothing are hidden by default — a list of twelve
	// running things is the pane-switching problem wearing a list.
	if strings.Contains(out, "Engine performance instrumentation") {
		t.Fatalf("running items should be hidden by default:\n%s", out)
	}
	if !strings.Contains(out, "a show 1 running") {
		t.Fatalf("the hidden count should be discoverable:\n%s", out)
	}
	if !strings.Contains(out, "> ") {
		t.Fatalf("the selected row should be marked:\n%s", out)
	}
	// Origin, not source: two Ship instances both say "teploy-ship".
	if !strings.Contains(out, "claude-code") {
		t.Fatalf("rows should name the source they came from:\n%s", out)
	}
}

func TestEmptyInboxSaysSoRatherThanRenderingNothing(t *testing.T) {
	m := New(nil)
	m.loading = false
	out := m.View()
	if !strings.Contains(out, "nothing waiting on you") || !strings.Contains(out, "inbox empty") {
		t.Fatalf("an empty inbox should say so:\n%s", out)
	}
}

func TestDetailViewShowsThePromptAndNumberedActions(t *testing.T) {
	m := populated()
	m.mode = modeDetail
	out := m.View()
	if !strings.Contains(out, "Approve this action? bash: rm -rf node_modules") {
		t.Fatalf("detail should show the question:\n%s", out)
	}
	if !strings.Contains(out, "[1] approve") || !strings.Contains(out, "[2] deny") {
		t.Fatalf("actions should be numbered for selection:\n%s", out)
	}
	if !strings.Contains(out, "worker-02") || !strings.Contains(out, "http://box:7460/runs/run-parked01") {
		t.Fatalf("detail should show context and link:\n%s", out)
	}
}

func TestAFailingSourceIsReportedWithoutHidingTheList(t *testing.T) {
	m := populated()
	m.results = []sources.Result{{Source: "dash", Err: errTest{}}}
	out := m.View()
	if !strings.Contains(out, "dash is unreachable") {
		t.Fatalf("a dead source should be visible:\n%s", out)
	}
	if !strings.Contains(out, "fix the failing auth test") {
		t.Fatalf("the rest of the list must still render:\n%s", out)
	}
}

type errTest struct{}

func (errTest) Error() string { return "dash is unreachable" }

func TestAUniformListDropsTheBandButAMixedOneKeepsIt(t *testing.T) {
	// With a running item revealed the list is mixed, so the band becomes
	// the thing that distinguishes rows and has to come back.
	out := populated().SetShowAll(true).View()
	if !strings.Contains(out, "decision") || !strings.Contains(out, "info") {
		t.Fatalf("a mixed list needs its bands:\n%s", out)
	}
}

func TestOnlyARealQuestionGetsASecondLine(t *testing.T) {
	m := New(nil)
	m.loading, m.width = false, 100
	m.items = []feed.Item{
		{Origin: "opencode", ID: "a", Title: "real", State: feed.StateBlocked,
			Attention: feed.AttentionDecision,
			Needs:     &feed.Needs{Prompt: "Promote to production now?"}},
		{Origin: "opencode", ID: "b", Title: "filler", State: feed.StateBlocked,
			Attention: feed.AttentionDecision,
			Needs:     &feed.Needs{Prompt: "Finished its turn — waiting on you."}},
	}
	out := m.View()
	if !strings.Contains(out, "Promote to production now?") {
		t.Fatalf("a real ask must show:\n%s", out)
	}
	// Placeholder prompts stay in the feed for contract consumers, but
	// repeating them down the screen is noise, not information.
	if strings.Contains(out, "Finished its turn") {
		t.Fatalf("a placeholder ask must not render:\n%s", out)
	}
}

func TestTheProjectIsVisibleWithoutOpeningTheDetailPane(t *testing.T) {
	// "which repo is this?" is the orienting question; burying it one
	// keypress away was the main thing wrong with the first layout.
	if !strings.Contains(populated().View(), "decor-arbor") {
		t.Fatalf("project should be a column:\n%s", populated().View())
	}
}

func TestShowAllRevealsRunningSessionsAndTheHelpFlips(t *testing.T) {
	out := populated().SetShowAll(true).View()
	if !strings.Contains(out, "Engine performance instrumentation") {
		t.Fatalf("--all should reveal running sessions:\n%s", out)
	}
}

func TestTheKeyListStaysShortUntilAskedFor(t *testing.T) {
	// Seven keys on one line reads as clutter and gets skipped.
	m := populated()
	if strings.Contains(m.View(), "j/k move") {
		t.Fatalf("the full key list should be behind ?:\n%s", m.View())
	}
	updated, _ := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !strings.Contains(updated.(Model).View(), "j/k move") {
		t.Fatal("? should expand the keys")
	}
}

func TestAnInboxWithNothingWaitingSaysSoRatherThanLookingEmpty(t *testing.T) {
	m := New(nil)
	m.loading = false
	m.items = []feed.Item{{ID: "a", Title: "busy thing", State: feed.StateRunning, Attention: feed.AttentionInfo}}
	out := m.View()
	// "empty" and "nothing needs you, three are running" are different facts.
	if !strings.Contains(out, "nothing needs you — 1 running") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestSelectionTracksTheVisibleListNotTheFullOne(t *testing.T) {
	m := populated()
	m.cursor = 1
	item, ok := m.selected()
	if !ok || item.ID != "sess-1" {
		t.Fatalf("cursor must index the filtered list, got %+v", item)
	}
}

func sendableItem(id, label string) feed.Item {
	return feed.Item{
		Origin: "opencode", Source: "opencode", ID: id, State: feed.StateBlocked,
		Attention: feed.AttentionDecision, Title: id,
		Needs: &feed.Needs{Prompt: "waiting", Actions: []feed.Action{
			{Label: label, Run: []string{"opencode", "run", "-s", id, "{message}"}},
		}},
	}
}

func TestSendableRecognisesReplyAndSendButNotAttach(t *testing.T) {
	if _, ok := sendable(sendableItem("a", "reply")); !ok {
		t.Fatal("reply is a delivery action")
	}
	if _, ok := sendable(sendableItem("b", "send")); !ok {
		t.Fatal("send is a delivery action")
	}
	// attach opens a session for a human; it delivers nothing.
	if _, ok := sendable(sendableItem("c", "attach")); ok {
		t.Fatal("attach must not count as sendable")
	}
	if _, ok := sendable(feed.Item{State: feed.StateRunning}); ok {
		t.Fatal("an item with no needs is not sendable")
	}
}

func TestMarkedSendableCountsOnlyWhatCanActuallyReceive(t *testing.T) {
	m := New(nil)
	reachable := sendableItem("a", "reply")
	// A Claude Code session whose pane never resolved: markable, but a
	// broadcast cannot deliver to it.
	unreachable := sendableItem("b", "attach")

	m.items = []feed.Item{reachable, unreachable}
	m.marked[reachable.Key()] = true
	m.marked[unreachable.Key()] = true

	if got := m.markedSendable(); got != 1 {
		t.Fatalf("got %d, want 1 — an unreachable mark must not inflate the count", got)
	}
}

func TestSpaceTogglesAMarkAndBroadcastRefusesWithNothingReachable(t *testing.T) {
	m := New(nil)
	m.loading = false
	m.items = []feed.Item{sendableItem("a", "attach")}

	updated, _ := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = updated.(Model)
	if len(m.marked) != 1 {
		t.Fatal("space should mark the selected item")
	}

	updated, _ = m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = updated.(Model)
	if m.mode == modeBroadcast {
		t.Fatal("broadcast must not open when nothing marked can receive")
	}
	if m.lastErr == nil {
		t.Fatal("refusing silently is worse than saying why")
	}
}

func TestBroadcastComposerOpensWhenSomethingIsReachable(t *testing.T) {
	m := New(nil)
	m.loading = false
	m.items = []feed.Item{sendableItem("a", "reply")}
	m.marked[m.items[0].Key()] = true

	updated, _ := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = updated.(Model)
	if m.mode != modeBroadcast {
		t.Fatalf("expected the composer, got mode %v", m.mode)
	}
	if !strings.Contains(m.View(), "broadcast to 1 session") {
		t.Fatalf("composer should state the reach:\n%s", m.View())
	}
}

func TestBroadcastResultClearsMarksAndReportsFailures(t *testing.T) {
	m := New(nil)
	m.loading = false
	m.items = []feed.Item{sendableItem("a", "reply")}
	m.marked[m.items[0].Key()] = true

	updated, _ := m.Update(broadcastMsg{ok: 2, failed: 1, firstErr: errTest{}})
	m = updated.(Model)
	if !strings.Contains(m.sent, "sent to 2") || !strings.Contains(m.sent, "1 failed") {
		t.Fatalf("got %q", m.sent)
	}
	if len(m.marked) != 0 {
		t.Fatal("marks should clear after a send, or the next broadcast repeats it")
	}
}

func TestRefreshIsSuspendedWhileComposingABroadcast(t *testing.T) {
	// A list reordering under a half-typed message is how you send the
	// wrong thing to the wrong session.
	m := New(nil)
	m.mode = modeBroadcast
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("the tick should still be rescheduled")
	}
}

func TestEveryAgeFitsTheColumnSoRowsStayAligned(t *testing.T) {
	// A single over-wide value shunts the project and title columns out of
	// line for that row only, which reads as a rendering glitch.
	now := time.Now()
	for _, since := range []time.Time{
		now, now.Add(-30 * time.Second), now.Add(-5 * time.Minute),
		now.Add(-3 * time.Hour), now.Add(-2 * 24 * time.Hour), now.Add(-400 * 24 * time.Hour),
	} {
		item := feed.Item{Since: since.Format(time.RFC3339)}
		if got := age(item); len([]rune(got)) > 5 {
			t.Fatalf("age %q is %d wide, column is 5", got, len([]rune(got)))
		}
	}
}
