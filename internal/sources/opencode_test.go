package sources

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"agentinbox/internal/feed"
)

// ocFixture builds a throwaway opencode.db with the real schema and given
// rows. This exercises the actual `sqlite3 -json` read path rather than a Go
// mock, since the query shape is the part most likely to drift on an opencode
// upgrade.
func ocFixture(t *testing.T, sessions, messages []string) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	path := filepath.Join(t.TempDir(), "opencode.db")
	run := func(sql string) {
		if out, err := exec.Command("sqlite3", path, sql).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3: %v: %s", err, out)
		}
	}
	run(`CREATE TABLE session (
	  id text PRIMARY KEY, title text, directory text,
	  time_created integer, time_updated integer, time_archived integer);
	CREATE TABLE message (
	  id text PRIMARY KEY, session_id text, time_created integer, data text);`)
	for _, s := range sessions {
		run(s)
	}
	for _, m := range messages {
		run(m)
	}
	return path
}

// oc builds a source with the lsof step skipped (AnyDirectory), so a test
// exercises the database read and the state mapping without needing a real
// opencode process running.
func oc(dbPath string) OpenCode {
	return OpenCode{DB: dbPath, AnyDirectory: true, Now: func() time.Time { return fixedNow }}
}

func ocFetch(t *testing.T, o OpenCode) []feed.Item {
	t.Helper()
	f, err := o.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f.Items
}

func sessionRow(id, title, dir string, updated int64) string {
	return `INSERT INTO session (id, title, directory, time_created, time_updated) VALUES ('` +
		id + `', '` + title + `', '` + dir + `', ` + itoa(updated) + `, ` + itoa(updated) + `);`
}

func messageRow(id, sessionID, role, finish string, at int64) string {
	data := `{"role":"` + role + `"`
	if finish != "" {
		data += `,"finish":"` + finish + `"`
	}
	data += `}`
	return `INSERT INTO message (id, session_id, time_created, data) VALUES ('` +
		id + `', '` + sessionID + `', ` + itoa(at) + `, '` + data + `');`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestAnAssistantMessageThatStoppedIsBlockedAndOneStillCallingToolsIsRunning(t *testing.T) {
	db := ocFixture(t,
		[]string{
			sessionRow("s-stop", "Which auth provider?", "/repos/a", 1000),
			sessionRow("s-work", "Refactoring the router", "/repos/b", 1000),
		},
		[]string{
			messageRow("m1", "s-stop", "assistant", "stop", 1000),
			messageRow("m2", "s-work", "assistant", "tool-calls", 1000),
		})

	byID := map[string]feed.Item{}
	for _, i := range ocFetch(t, oc(db)) {
		byID[i.ID] = i
	}
	if byID["s-stop"].State != feed.StateBlocked || byID["s-stop"].Needs == nil {
		t.Fatalf("finish:stop should block, got %+v", byID["s-stop"])
	}
	if byID["s-work"].State == feed.StateBlocked {
		t.Fatalf("finish:tool-calls is still working, got blocked: %+v", byID["s-work"])
	}
}

func TestASessionWhoseLastMessageIsFromTheUserIsNotWaitingOnYou(t *testing.T) {
	// The last word is yours; the agent has not answered yet, so this is
	// working, not blocked.
	db := ocFixture(t,
		[]string{sessionRow("s1", "mid-turn", "/repos/a", 1000)},
		[]string{
			messageRow("m1", "s1", "assistant", "stop", 900),
			messageRow("m2", "s1", "user", "", 1000),
		})
	item := ocFetch(t, oc(db))[0]
	if item.State == feed.StateBlocked {
		t.Fatalf("a session waiting on the assistant is not blocked, got %+v", item)
	}
}

func TestASessionWithNoMessagesIsRunningNotBlocked(t *testing.T) {
	// An empty session opened by launching opencode in a directory has
	// nothing to be blocked on.
	db := ocFixture(t, []string{sessionRow("s1", "New session - x", "/repos/a", 1000)}, nil)
	item := ocFetch(t, oc(db))[0]
	if item.State != feed.StateRunning || item.Needs != nil {
		t.Fatalf("got %+v", item)
	}
}

func TestOpenCodeOffersNoInventedAsk(t *testing.T) {
	// Unlike Claude Code's job state, opencode has no one-line "needs".
	// Saying so plainly beats fabricating a summary.
	db := ocFixture(t,
		[]string{sessionRow("s1", "x", "/repos/a", 1000)},
		[]string{messageRow("m1", "s1", "assistant", "stop", 1000)})
	item := ocFetch(t, oc(db))[0]
	if item.Needs.Prompt != "Finished its turn — waiting on you." {
		t.Fatalf("got %q", item.Needs.Prompt)
	}
}

func TestReplyDispatchesIntoTheExistingSessionRatherThanStartingANewOne(t *testing.T) {
	// opencode has no equivalent of Claude Code's --resume refusal: this is
	// the one dispatch path that actually works.
	db := ocFixture(t,
		[]string{sessionRow("ses_abc", "x", "/repos/a", 1000)},
		[]string{messageRow("m1", "ses_abc", "assistant", "stop", 1000)})
	actions := ocFetch(t, oc(db))[0].Needs.Actions
	if len(actions) != 2 || actions[0].Label != "reply" {
		t.Fatalf("got %+v", actions)
	}
	if actions[0].Run[0] != "opencode" || actions[0].Run[1] != "run" || actions[0].Run[2] != "-s" ||
		actions[0].Run[3] != "ses_abc" || actions[0].Run[4] != "{message}" {
		t.Fatalf("unexpected reply argv: %v", actions[0].Run)
	}
	if actions[0].Dir != "/repos/a" {
		t.Fatalf("reply should run in the session's directory, got %q", actions[0].Dir)
	}
}

func TestArchivedSessionsAreExcluded(t *testing.T) {
	db := ocFixture(t, nil, nil)
	exec.Command("sqlite3", db, `INSERT INTO session (id, title, directory, time_updated, time_archived)
	  VALUES ('s1','x','/repos/a',1000,1000);`).Run()
	if got := len(ocFetch(t, oc(db))); got != 0 {
		t.Fatalf("an archived session should not appear, got %d", got)
	}
}

func TestOnlyTheNewestSessionPerDirectoryIsShown(t *testing.T) {
	db := ocFixture(t, []string{
		sessionRow("old", "old work", "/repos/a", 1000),
		sessionRow("new", "new work", "/repos/a", 2000),
	}, nil)
	items := ocFetch(t, oc(db))
	if len(items) != 1 || items[0].ID != "new" {
		t.Fatalf("got %+v", items)
	}
}

func TestAnUnprompedSessionTitleFallsBackToTheProject(t *testing.T) {
	db := ocFixture(t, []string{
		sessionRow("s1", "New session - 2026-07-31T00:00:00.000Z", "/repos/akiroo", 1000),
	}, nil)
	if got := ocFetch(t, oc(db))[0].Title; got != "session in akiroo" {
		t.Fatalf("got %q", got)
	}
}

func TestAMissingDatabaseIsAnEmptyFeedNotAnError(t *testing.T) {
	o := OpenCode{DB: filepath.Join(t.TempDir(), "nope.db"), AnyDirectory: true}
	items, err := o.Fetch(context.Background())
	if err != nil {
		t.Fatalf("a missing db should degrade quietly, got error: %v", err)
	}
	if len(items.Items) != 0 {
		t.Fatalf("got %d items", len(items.Items))
	}
}

func TestLiveNearMatchesExactAndOneLevelButNotAShallowAncestor(t *testing.T) {
	live := map[string]bool{"/repos/Clank": true}

	if !liveNear(live, "/repos/Clank") {
		t.Fatal("exact match should count")
	}
	// The real case seen: the process cwd is the repo root, the session is
	// recorded one level down.
	if !liveNear(live, "/repos/Clank/lab") {
		t.Fatal("one level below the live cwd should count")
	}
	if !liveNear(map[string]bool{"/repos/Clank/lab": true}, "/repos/Clank") {
		t.Fatal("one level above the live cwd should count")
	}
	// The bug this guards: a session recorded at a shallow root like $HOME
	// is an ancestor of every project and must not match everything live.
	if liveNear(map[string]bool{"/repos/Clank/deep/nested/dir": true}, "/repos") {
		t.Fatal("a shallow ancestor several levels up must not match")
	}
	if liveNear(live, "/repos/other-project") {
		t.Fatal("an unrelated sibling directory must not match")
	}
}
