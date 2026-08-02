package inbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentinbox/internal/driver"
)

// scriptDriver answers with a queued reply per directory and records every
// prompt it was given, so a test can assert what the king was actually told.
type scriptDriver struct {
	mu      sync.Mutex
	replies map[string][]string
	prompts map[string][]string
	block   map[string]chan struct{}
}

func newScriptDriver() *scriptDriver {
	return &scriptDriver{
		replies: map[string][]string{},
		prompts: map[string][]string{},
		block:   map[string]chan struct{}{},
	}
}

func (s *scriptDriver) Name() string { return "script" }

func (s *scriptDriver) Send(ctx context.Context, dir, sessionID, prompt string) driver.Result {
	s.mu.Lock()
	s.prompts[dir] = append(s.prompts[dir], prompt)
	gate := s.block[dir]
	var reply string
	if q := s.replies[dir]; len(q) > 0 {
		reply = q[0]
		s.replies[dir] = q[1:]
	} else {
		reply = "(nothing scripted)"
	}
	s.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return driver.Result{SessionID: sessionID, Status: driver.StatusError, Err: ctx.Err()}
		}
	}
	return driver.Result{SessionID: "s", Final: reply, Status: driver.StatusWaiting}
}

func (s *scriptDriver) AttachArgs(dir, sessionID string) []string { return []string{"true"} }

func (s *scriptDriver) promptsFor(dir string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts[dir]...)
}

// waitFor polls until cond holds, so a test never depends on a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func kingFixture(t *testing.T, d *scriptDriver) *Inbox {
	t.Helper()
	projects := []*Project{
		{Name: "king", Tool: "script", Dir: "/king", Status: driver.StatusIdle},
		{Name: "omni", Tool: "script", Dir: "/omni", Status: driver.StatusIdle},
		{Name: "akiroo", Tool: "script", Dir: "/akiroo", Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"script": d}, filepath.Join(t.TempDir(), "state.json"))
	// Set before any turn starts: watchers read this and outlive the test.
	in.pollEvery = time.Millisecond
	// Without this the watchers keep writing state after the temp dir is
	// removed, which fails cleanup rather than the assertion.
	t.Cleanup(in.Close)
	return in
}

func historyRoles(p Project) []string {
	out := make([]string, 0, len(p.History))
	for _, m := range p.History {
		out = append(out, m.Role)
	}
	return out
}

// The whole loop: one instruction fans out to two projects, both replies come
// back, and the king gets them in full so it can report.
func TestKingRoundClosesTheLoop(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{
		"On it.\n[send to omni: check for bugs]\n[send to akiroo: check for bugs]",
		"omni found a nil deref; akiroo is clean.",
	}
	d.replies["/omni"] = []string{"found a nil deref in ingest.go line 42"}
	d.replies["/akiroo"] = []string{"no issues found"}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "check omni and akiroo for bugs", []string{"omni", "akiroo"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}

	waitFor(t, "the king's summary turn", func() bool {
		return len(d.promptsFor("/king")) == 2
	})

	// The summary prompt must carry the full replies, not the truncated
	// receipts — recovering the findings is the entire point of the round.
	summary := d.promptsFor("/king")[1]
	for _, want := range []string{"found a nil deref in ingest.go line 42", "no issues found", "omni", "akiroo"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary prompt missing %q:\n%s", want, summary)
		}
	}

	waitFor(t, "the summary to land in history", func() bool {
		return strings.Contains(in.Snapshot()[0].LastMessage, "akiroo is clean")
	})

	king := in.Snapshot()[0]
	roles := historyRoles(king)
	// One user turn only: the summary pass is not something the user typed.
	users := 0
	for _, r := range roles {
		if r == "user" {
			users++
		}
	}
	if users != 1 {
		t.Errorf("history has %d user turns, want 1: %v", users, roles)
	}
	// Each project filed a receipt under its own name.
	for _, name := range []string{"omni", "akiroo"} {
		found := false
		for _, r := range roles {
			if r == name {
				found = true
			}
		}
		if !found {
			t.Errorf("no receipt from %s: %v", name, roles)
		}
	}
}

// A receipt is a one-line pointer to the project's own thread, not a copy of
// the reply. Duplicating full replies makes the supervisor's conversation the
// transcript of every other one.
func TestKingReceiptsAreCompact(t *testing.T) {
	long := strings.Repeat("this is a very long finding. ", 40)
	d := newScriptDriver()
	d.replies["/king"] = []string{"[send to omni: look]", "done"}
	d.replies["/omni"] = []string{long}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "look", []string{"omni"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the receipt", func() bool {
		for _, m := range in.Snapshot()[0].History {
			if m.Role == "omni" {
				return true
			}
		}
		return false
	})

	for _, m := range in.Snapshot()[0].History {
		if m.Role != "omni" {
			continue
		}
		if len([]rune(m.Content)) > receiptWidth {
			t.Errorf("receipt is %d runes, want <= %d", len([]rune(m.Content)), receiptWidth)
		}
		if strings.Contains(m.Content, "\n") {
			t.Error("receipt spans multiple lines")
		}
	}
	// The full text is still where it belongs: the project's own thread.
	if got := in.Snapshot()[1].LastMessage; got != long {
		t.Error("the project's own thread lost the full reply")
	}
}

// Dispatching to a busy project must not schedule a watcher — otherwise it
// waits out the work already in flight and files that unrelated answer as the
// reply to the question just asked.
func TestKingDoesNotWatchAFailedDispatch(t *testing.T) {
	d := newScriptDriver()
	gate := make(chan struct{})
	d.block["/omni"] = gate
	d.replies["/omni"] = []string{"answer to the EARLIER question"}
	d.replies["/king"] = []string{"[send to omni: the new question]"}

	in := kingFixture(t, d)

	// omni is already mid-turn on something else.
	if err := in.Send(2, "an earlier question"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "omni to be working", func() bool {
		return in.Snapshot()[1].Status == driver.StatusWorking
	})

	if err := in.KingSend(1, "ask omni", []string{"omni"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the refusal note", func() bool {
		for _, m := range in.Snapshot()[0].History {
			if m.Role == "system" && strings.Contains(m.Content, "omni") {
				return true
			}
		}
		return false
	})

	// Let the earlier turn finish and confirm its answer never gets filed as
	// a reply to the king.
	close(gate)
	waitFor(t, "omni to finish", func() bool {
		return in.Snapshot()[1].Status != driver.StatusWorking
	})
	time.Sleep(50 * time.Millisecond)

	for _, m := range in.Snapshot()[0].History {
		if m.Role == "omni" {
			t.Errorf("a stale reply was filed as omni's answer: %q", m.Content)
		}
	}
	// And no summary pass ran off the back of it.
	if n := len(d.promptsFor("/king")); n != 1 {
		t.Errorf("king got %d prompts, want 1 (no summary for a round that never happened)", n)
	}
}

func TestKingNotesAnUnknownTarget(t *testing.T) {
	d := newScriptDriver()
	d.replies["/king"] = []string{"[send to nonexistent: do a thing]"}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "go", []string{"omni"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "the unknown-target note", func() bool {
		for _, m := range in.Snapshot()[0].History {
			if m.Role == "system" && strings.Contains(m.Content, "nonexistent") {
				return true
			}
		}
		return false
	})
}

func TestSummaryPromptForbidsFurtherDirectives(t *testing.T) {
	got := summaryPrompt([]fleetReply{{name: "omni", content: "all good"}}, 0)
	if !strings.Contains(got, "all good") || !strings.Contains(got, "omni") {
		t.Errorf("reply missing from prompt:\n%s", got)
	}
	// The summary turn is sent outside the dispatch path, so a directive in
	// it would be silently dropped. Say so rather than let the model try.
	if !strings.Contains(got, "[send to ...]") {
		t.Errorf("prompt does not warn against directives:\n%s", got)
	}
	if !strings.Contains(got, "will not be dispatched") {
		t.Errorf("prompt does not say the directives are inert:\n%s", got)
	}
}

// With rounds left the summary invites a follow-up instead of forbidding one,
// and says how many are left — a model that is not told its limit either stops
// early or keeps asking after the answer stopped being dispatched.
func TestSummaryPromptOffersRemainingRounds(t *testing.T) {
	got := summaryPrompt([]fleetReply{{name: "omni", content: "all good"}}, 2)
	if strings.Contains(got, "will not be dispatched") {
		t.Errorf("prompt forbids directives despite budget:\n%s", got)
	}
	if !strings.Contains(got, "2 further round(s)") {
		t.Errorf("prompt does not state the remaining budget:\n%s", got)
	}
}

func TestTruncateForKingIsRuneSafe(t *testing.T) {
	got := truncateForKing("日本語のテキストです", 5)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q", got)
	}
	if len([]rune(got)) != 5 {
		t.Errorf("got %d runes, want 5: %q", len([]rune(got)), got)
	}
	// Collapsing newlines keeps a receipt to one line.
	if got := truncateForKing("one\ntwo", 100); got != "one two" {
		t.Errorf("got %q, want %q", got, "one two")
	}
}

// Telling the king the directive format is not enough. Asked about another
// project it reaches for the filesystem, and every one of those calls is
// rejected because the folder is outside its working directory. The prompt
// has to rule that out, not just offer an alternative.
func TestKingStateForbidsReachingIntoOtherProjects(t *testing.T) {
	d := newScriptDriver()
	in := kingFixture(t, d)
	got := in.formatKingState([]string{"omni", "akiroo"})

	for _, want := range []string{"cannot read", "outside your working directory", "[send to omni:"} {
		if !strings.Contains(got, want) {
			t.Errorf("fleet state missing %q:\n%s", want, got)
		}
	}
	// Both projects have to appear, or the king cannot address them.
	for _, name := range []string{"omni", "akiroo"} {
		if !strings.Contains(got, "- "+name+" (script)") {
			t.Errorf("fleet state missing project %s:\n%s", name, got)
		}
	}
}

// With nothing connected the king is a plain chat session; injecting fleet
// rules would have it refusing to read its own files.
func TestKingStateOmitsFleetRulesWithoutFleet(t *testing.T) {
	d := newScriptDriver()
	in := kingFixture(t, d)
	if got := in.formatKingState(nil); strings.Contains(got, "cannot read") {
		t.Errorf("fleet rules injected with no fleet:\n%s", got)
	}
}

// Watchers live for minutes and RemoveProject splices the slice, shifting
// every index after it down by one. A watcher holding an index would file
// akiroo's reply under omni's name, or send the summary to the wrong session
// entirely. Identity is by name for exactly this reason.
func TestRoundSurvivesAProjectRemovedMidFlight(t *testing.T) {
	d := newScriptDriver()
	gate := make(chan struct{})
	d.block["/akiroo"] = gate
	d.replies["/king"] = []string{"[send to akiroo: check]", "akiroo reported in."}
	d.replies["/akiroo"] = []string{"akiroo's own findings"}

	in := kingFixture(t, d)
	if err := in.KingSend(1, "ask akiroo", []string{"akiroo"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	waitFor(t, "akiroo to start working", func() bool {
		for _, p := range in.Snapshot() {
			if p.Name == "akiroo" && p.Status == driver.StatusWorking {
				return true
			}
		}
		return false
	})

	// Remove omni (index 2), which sits between the king and akiroo. Every
	// index after it now points somewhere else.
	if err := in.RemoveProject(2); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	close(gate)

	waitFor(t, "the round to finish", func() bool {
		return strings.Contains(in.Snapshot()[0].LastMessage, "akiroo reported in")
	})

	king := in.Snapshot()[0]
	if king.Name != "king" {
		t.Fatalf("the summary landed on %q, not the king", king.Name)
	}
	var receipts []string
	for _, m := range king.History {
		if m.Role == "akiroo" || m.Role == "omni" {
			receipts = append(receipts, m.Role+": "+m.Content)
		}
	}
	if len(receipts) != 1 || !strings.HasPrefix(receipts[0], "akiroo: ") {
		t.Errorf("receipts filed under the wrong project: %v", receipts)
	}
	if !strings.Contains(receipts[0], "akiroo's own findings") {
		t.Errorf("receipt content came from the wrong project: %v", receipts)
	}
}

// A non-streaming driver's reply reached the screen and then vanished on the
// next restart: blockingSend appended to history but never persisted it, and
// only the streaming path (Claude) saved as it went. opencode, codex and mock
// all take the blocking path.
func TestBlockingReplySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	d := newScriptDriver()
	d.replies["/omni"] = []string{"the reply that used to disappear"}
	projects := []*Project{{Name: "omni", Tool: "script", Dir: "/omni", Status: driver.StatusIdle}}
	in := New(projects, map[string]driver.Driver{"script": d}, statePath)
	in.pollEvery = time.Millisecond
	t.Cleanup(in.Close)

	if err := in.Send(1, "ask something"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "the turn to finish", func() bool {
		return in.Snapshot()[0].Status != driver.StatusWorking
	})
	// The save happens just after the status flips; give it its moment.
	waitFor(t, "the reply to reach disk", func() bool {
		b, err := os.ReadFile(statePath)
		return err == nil && strings.Contains(string(b), "the reply that used to disappear")
	})

	// Reopen exactly as main.go does.
	reopened := []*Project{{Name: "omni", Tool: "script", Dir: "/omni"}}
	LoadState(statePath, reopened)
	if reopened[0].LastMessage != "the reply that used to disappear" {
		t.Errorf("last message lost on restart: %q", reopened[0].LastMessage)
	}
	var sawAssistant bool
	for _, m := range reopened[0].History {
		if m.Role == "assistant" && strings.Contains(m.Content, "used to disappear") {
			sawAssistant = true
		}
	}
	if !sawAssistant {
		t.Errorf("the assistant turn is missing from restored history: %+v", reopened[0].History)
	}
}

// Close must drain, not just signal. A watcher already past its stop check
// still has a write to make, and a caller that deletes the state directory
// the moment Close returns would race it.
func TestCloseWaitsForBackgroundWork(t *testing.T) {
	dir := t.TempDir()
	d := newScriptDriver()
	d.replies["/king"] = []string{"[send to omni: go]", "done"}
	d.replies["/omni"] = []string{"reply"}
	projects := []*Project{
		{Name: "king", Tool: "script", Dir: "/king", Status: driver.StatusIdle},
		{Name: "omni", Tool: "script", Dir: "/omni", Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"script": d}, filepath.Join(dir, "state.json"))
	in.pollEvery = time.Millisecond

	if err := in.KingSend(1, "go", []string{"omni"}); err != nil {
		t.Fatalf("KingSend: %v", err)
	}
	in.Close()

	// Everything has stopped: the directory can be emptied and stays empty.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(80 * time.Millisecond)
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("something wrote after Close returned: %v", left)
	}
}

// Close is safe twice, and safe when nothing ever ran.
func TestCloseIsIdempotent(t *testing.T) {
	in := New(nil, map[string]driver.Driver{}, filepath.Join(t.TempDir(), "s.json"))
	in.Close()
	in.Close()
}
