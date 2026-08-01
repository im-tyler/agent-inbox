package sources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentinbox/internal/feed"
)

// rollout writes a codex session file the way codex lays them out:
// <root>/<year>/<month>/<day>/rollout-*.jsonl, session_meta first.
func rollout(t *testing.T, root, day, id, cwd string, tail []string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(root, "2026", "07", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"session_meta","timestamp":"2026-07-` + day + `T10:00:00.000Z","payload":{"id":"` +
			id + `","session_id":"` + id + `","cwd":"` + cwd + `"}}`,
	}
	lines = append(lines, tail...)
	path := filepath.Join(dir, "rollout-2026-07-"+day+"T10-00-00-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func event(kind, message string) string {
	if message == "" {
		return `{"type":"event_msg","timestamp":"2026-07-28T10:00:00.000Z","payload":{"type":"` + kind + `"}}`
	}
	return `{"type":"event_msg","timestamp":"2026-07-28T10:00:00.000Z","payload":{"type":"` + kind +
		`","message":"` + message + `"}}`
}

func codexSrc(root string) Codex {
	return Codex{Root: root, AnyDirectory: true, MaxAge: 30 * 24 * time.Hour,
		Now: func() time.Time { return fixedNow }}
}

func codexFetch(t *testing.T, c Codex) []feed.Item {
	t.Helper()
	f, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f.Items
}

func TestTaskCompleteMeansTheTurnEndedAndYouAreNext(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "28", "sess-done", "/repos/a",
		[]string{event("agent_message", "Which database should I use?"), event("task_complete", "")},
		fixedNow.Add(-time.Hour))

	item := codexFetch(t, codexSrc(root))[0]
	if item.State != feed.StateBlocked {
		t.Fatalf("task_complete means waiting on you, got %q", item.State)
	}
	if item.Context["last_event"] != "task_complete" {
		t.Fatalf("got %+v", item.Context)
	}
}

func TestASessionStillStreamingIsRunning(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "28", "sess-live", "/repos/a",
		[]string{event("agent_message", "working on it"), event("token_count", "")},
		fixedNow.Add(-time.Minute))

	item := codexFetch(t, codexSrc(root))[0]
	if item.State == feed.StateBlocked {
		t.Fatalf("no task_complete means it is still running, got blocked")
	}
	if item.Needs != nil {
		t.Fatal("a running session asks nothing of you")
	}
}

func TestTheAskComesFromTheLastAgentMessage(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "28", "sess-ask", "/repos/a", []string{
		event("agent_message", "Ran the tests. All green."),
		event("agent_message", "Deployed to staging. Promote to production now?"),
		event("task_complete", ""),
	}, fixedNow.Add(-time.Hour))

	item := codexFetch(t, codexSrc(root))[0]
	if item.Needs.Prompt != "Deployed to staging. Promote to production now?" {
		t.Fatalf("got %q", item.Needs.Prompt)
	}
	// The row stays scannable; the paragraph lives in the prompt.
	if item.Title != "session in a" {
		t.Fatalf("title should stay short, got %q", item.Title)
	}
}

func TestABlockedSessionWithNoRecoverableAskStillSaysSomething(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "28", "sess-quiet", "/repos/a", []string{event("task_complete", "")},
		fixedNow.Add(-time.Hour))

	if got := codexFetch(t, codexSrc(root))[0].Needs.Prompt; got != "Finished its turn — waiting on you." {
		t.Fatalf("got %q", got)
	}
}

func TestALongSessionMetaLineIsStillParsed(t *testing.T) {
	// session_meta embeds the full base instructions and ran to ~19KB on a
	// real machine. A fixed 8KB read buffer truncated it, the id came back
	// empty, and every codex session was silently skipped.
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "28")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", 40000)
	lines := []string{
		`{"type":"session_meta","payload":{"id":"sess-big","cwd":"/repos/a","base_instructions":"` + huge + `"}}`,
		event("task_complete", ""),
	}
	path := filepath.Join(dir, "rollout-big.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, fixedNow.Add(-time.Hour), fixedNow.Add(-time.Hour))

	items := codexFetch(t, codexSrc(root))
	if len(items) != 1 || items[0].ID != "sess-big" {
		t.Fatalf("a long header must not drop the session, got %+v", items)
	}
}

func TestRolloutsOlderThanMaxAgeAreSkipped(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "28", "recent", "/repos/a", []string{event("task_complete", "")}, fixedNow.Add(-time.Hour))
	rollout(t, root, "01", "ancient", "/repos/b", []string{event("task_complete", "")}, fixedNow.Add(-100*24*time.Hour))

	c := codexSrc(root)
	c.MaxAge = 7 * 24 * time.Hour
	items := codexFetch(t, c)
	if len(items) != 1 || items[0].ID != "recent" {
		t.Fatalf("got %+v", items)
	}
}

func TestOnlyTheNewestRolloutPerDirectoryIsShown(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "27", "older", "/repos/a", []string{event("task_complete", "")}, fixedNow.Add(-5*time.Hour))
	rollout(t, root, "28", "newer", "/repos/a", []string{event("task_complete", "")}, fixedNow.Add(-time.Hour))

	items := codexFetch(t, codexSrc(root))
	if len(items) != 1 || items[0].ID != "newer" {
		t.Fatalf("got %+v", items)
	}
}

func TestReplyResumesTheExistingSessionRatherThanStartingANewOne(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "28", "sess-1", "/repos/a", []string{event("task_complete", "")}, fixedNow.Add(-time.Hour))

	actions := codexFetch(t, codexSrc(root))[0].Needs.Actions
	if len(actions) != 2 || actions[0].Label != "reply" {
		t.Fatalf("got %+v", actions)
	}
	if strings.Join(actions[0].Run, " ") != "codex exec resume sess-1 {message}" {
		t.Fatalf("unexpected reply argv: %v", actions[0].Run)
	}
	if actions[0].Dir != "/repos/a" {
		t.Fatalf("reply should run in the session's directory, got %q", actions[0].Dir)
	}
}

func TestAMalformedOrHeaderlessRolloutIsSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "28")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "rollout-junk.jsonl")
	os.WriteFile(path, []byte("not json\n{\"type\":\"event_msg\"}\n"), 0o644)
	os.Chtimes(path, fixedNow.Add(-time.Hour), fixedNow.Add(-time.Hour))
	rollout(t, root, "28", "good", "/repos/a", []string{event("task_complete", "")}, fixedNow.Add(-time.Hour))

	items := codexFetch(t, codexSrc(root))
	if len(items) != 1 || items[0].ID != "good" {
		t.Fatalf("one bad file should not cost the rest, got %+v", items)
	}
}

func TestAMissingSessionsRootIsEmptyNotAnError(t *testing.T) {
	if got := len(codexFetch(t, codexSrc(filepath.Join(t.TempDir(), "nope")))); got != 0 {
		t.Fatalf("got %d items", got)
	}
}
