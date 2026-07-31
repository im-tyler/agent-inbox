package sources

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentinbox/internal/feed"
)

var fixedNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// transcript writes a session file whose last assistant record carries
// stopReason, then backdates it so age-based rules are testable.
func transcript(t *testing.T, root, project, id, stopReason string, ago time.Duration, extra ...string) string {
	t.Helper()
	dir := filepath.Join(root, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := fixedNow.Add(-ago).Format(time.RFC3339)
	lines := []string{
		`{"type":"ai-title","aiTitle":"Polish the team UI","sessionId":"` + id + `"}`,
		`{"type":"last-prompt","lastPrompt":"keep going"}`,
		`{"type":"assistant","isSidechain":false,"sessionId":"` + id + `","cwd":"/repos/` + project + `","gitBranch":"main","timestamp":"` + ts + `","message":{"stop_reason":"` + stopReason + `"}}`,
	}
	lines = append(lines, extra...)
	path := filepath.Join(dir, id+".jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := fixedNow.Add(-ago)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func fetch(t *testing.T, c Claude) []feed.Item {
	t.Helper()
	c.Now = func() time.Time { return fixedNow }
	f, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f.Items
}

func TestEndTurnMeansTheSessionIsWaitingOnYou(t *testing.T) {
	root := t.TempDir()
	transcript(t, root, "akiroo", "sess-1", "end_turn", time.Minute)

	items := fetch(t, Claude{Root: root})
	if len(items) != 1 {
		t.Fatalf("expected one session, got %d", len(items))
	}
	if items[0].State != feed.StateBlocked {
		t.Fatalf("end_turn means the human is next, got %q", items[0].State)
	}
	if items[0].Needs == nil || len(items[0].Needs.Actions) != 1 {
		t.Fatal("a waiting session should offer a resume action")
	}
	action := items[0].Needs.Actions[0]
	if action.Run[0] != "claude" || action.Run[1] != "--resume" || action.Run[2] != "sess-1" {
		t.Fatalf("unexpected resume argv: %v", action.Run)
	}
	if action.Dir != "/repos/akiroo" {
		t.Fatalf("resume should start in the session's repo, got %q", action.Dir)
	}
}

func TestRecentToolUseIsRunningAndStaleToolUseIsStalled(t *testing.T) {
	root := t.TempDir()
	transcript(t, root, "fresh", "sess-fresh", "tool_use", 30*time.Second)
	transcript(t, root, "wedged", "sess-wedged", "tool_use", 30*time.Minute)

	byProject := map[string]feed.Item{}
	for _, item := range fetch(t, Claude{Root: root}) {
		byProject[item.Context["project"]] = item
	}
	if got := byProject["fresh"].State; got != feed.StateRunning {
		t.Fatalf("a live tool call is running, got %q", got)
	}
	// It claims to be mid-tool-call but nothing has been written in half an
	// hour: the process is gone or wedged, and a human should look.
	if got := byProject["wedged"].State; got != feed.StateFailed {
		t.Fatalf("a stalled tool call should surface as a failure, got %q", got)
	}
	if byProject["wedged"].Context["stalled_for"] == "" {
		t.Fatal("a stalled session should say how long it has been stuck")
	}
}

func TestSidechainTurnsDoNotDecideTheSessionState(t *testing.T) {
	root := t.TempDir()
	// A subagent finishing its turn must not report the parent session as
	// waiting while the parent is still working.
	transcript(t, root, "proj", "sess-1", "tool_use", time.Minute,
		`{"type":"assistant","isSidechain":true,"message":{"stop_reason":"end_turn"},"timestamp":"`+fixedNow.Add(-time.Minute).Format(time.RFC3339)+`"}`)

	items := fetch(t, Claude{Root: root})
	if items[0].State != feed.StateRunning {
		t.Fatalf("a sidechain end_turn must not mark the session waiting, got %q", items[0].State)
	}
}

func TestOnlyTheNewestSessionPerProjectIsShownByDefault(t *testing.T) {
	root := t.TempDir()
	transcript(t, root, "proj", "old", "end_turn", 4*time.Hour)
	transcript(t, root, "proj", "new", "end_turn", time.Minute)

	items := fetch(t, Claude{Root: root})
	if len(items) != 1 || items[0].ID != "new" {
		t.Fatalf("expected only the newest session, got %+v", items)
	}
	if got := len(fetch(t, Claude{Root: root, AllPerProject: true})); got != 2 {
		t.Fatalf("all_sessions should show both, got %d", got)
	}
}

func TestSessionsOlderThanMaxAgeAreDropped(t *testing.T) {
	root := t.TempDir()
	transcript(t, root, "ancient", "sess-old", "end_turn", 72*time.Hour)
	transcript(t, root, "today", "sess-new", "end_turn", time.Hour)

	items := fetch(t, Claude{Root: root})
	if len(items) != 1 || items[0].Context["project"] != "today" {
		t.Fatalf("stale sessions should drop out, got %+v", items)
	}
}

func TestTitleFallsBackFromAITitleToPromptToProject(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bare")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"s","cwd":"/repos/bare","timestamp":"` +
		fixedNow.Add(-time.Minute).Format(time.RFC3339) + `","message":{"stop_reason":"end_turn"}}` + "\n"
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := fixedNow.Add(-time.Minute)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}

	items := fetch(t, Claude{Root: root})
	if items[0].Title != "session in bare" {
		t.Fatalf("expected a project fallback title, got %q", items[0].Title)
	}
}

func TestMissingClaudeDirectoryIsEmptyNotAnError(t *testing.T) {
	items := fetch(t, Claude{Root: filepath.Join(t.TempDir(), "nope")})
	if len(items) != 0 {
		t.Fatalf("expected no items, got %d", len(items))
	}
}

func TestDecodeAcceptsEnvelopeBareArrayAndEmptyOutput(t *testing.T) {
	f, err := decode("x", []byte(`{"schema":"teploy.inbox/v1","items":[{"id":"a"}]}`))
	if err != nil || len(f.Items) != 1 {
		t.Fatalf("envelope: %v %+v", err, f)
	}
	// Producers get the envelope wrong at first; a readable list beats a
	// strict parser.
	f, err = decode("x", []byte(`[{"id":"a"},{"id":"b"}]`))
	if err != nil || len(f.Items) != 2 {
		t.Fatalf("bare array: %v %+v", err, f)
	}
	if f, err := decode("x", []byte("  \n")); err != nil || len(f.Items) != 0 {
		t.Fatalf("empty: %v %+v", err, f)
	}
}

func TestOneDeadSourceDoesNotBlankTheList(t *testing.T) {
	root := t.TempDir()
	transcript(t, root, "proj", "sess-1", "end_turn", time.Minute)

	live := Claude{Root: root, Now: func() time.Time { return fixedNow }}
	dead := Exec{Label: "dead", Command: []string{"/nonexistent/binary"}}

	items, results := FetchAll(context.Background(), []Source{live, dead})
	if len(items) != 1 {
		t.Fatalf("the working source should still render, got %d items", len(items))
	}
	if bad := Errors(results); len(bad) != 1 || bad[0].Source != "dead" {
		t.Fatalf("the dead source should report itself, got %+v", bad)
	}
}

func TestFetchAllStampsOriginSoSameIDsFromTwoSourcesCoexist(t *testing.T) {
	root := t.TempDir()
	transcript(t, root, "proj", "sess-1", "end_turn", time.Minute)

	items, _ := FetchAll(context.Background(), []Source{Claude{Root: root, Now: func() time.Time { return fixedNow }}})
	if items[0].Origin != "claude-code" {
		t.Fatalf("origin should name the configured source, got %q", items[0].Origin)
	}
}
