package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/driver"
)

// Attaching to a session the inbox is mid-turn on puts two writers on one
// conversation. Delete and tool-change already refused this; attach did not,
// and it is the one that hands the user a live prompt into the session.
func TestAttachIsRefusedWhileTheProjectIsWorking(t *testing.T) {
	in := New([]*Project{{
		Name: "api", Tool: "mock", Dir: t.TempDir(),
		SessionID: "s1", Status: driver.StatusWorking,
	}}, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "s.json"))
	t.Cleanup(in.Close)

	if _, _, err := in.AttachArgs(1); err == nil {
		t.Fatal("attach was allowed during a turn")
	}
}

// A pending fork source belongs to somebody else's live agent. Attaching to it
// would drop the user into that agent's session, not this project's.
func TestAttachIsRefusedBeforeAnAdoptedSessionHasForked(t *testing.T) {
	in := New([]*Project{{
		Name: "api", Tool: "mock", Dir: t.TempDir(),
		SessionID: "borrowed", ForkFrom: "borrowed", Status: driver.StatusIdle,
	}}, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "s.json"))
	t.Cleanup(in.Close)

	_, _, err := in.AttachArgs(1)
	if err == nil {
		t.Fatal("attach was allowed into an unforked adopted session")
	}
	if !strings.Contains(err.Error(), "forked") {
		t.Errorf("the error should explain why: %v", err)
	}
}

func TestAttachWorksOnceTheProjectIsIdleAndOwnsItsSession(t *testing.T) {
	in := New([]*Project{{
		Name: "api", Tool: "mock", Dir: t.TempDir(),
		SessionID: "mine", Status: driver.StatusIdle,
	}}, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "s.json"))
	t.Cleanup(in.Close)

	if _, _, err := in.AttachArgs(1); err != nil {
		t.Fatalf("attach should be allowed: %v", err)
	}
}

// Config is the durable record of which projects exist, so a project that
// cannot be written there has not really been added. It used to appear in the
// UI and vanish on the next restart.
func TestAddProjectFailsRatherThanAddingAProjectItCannotPersist(t *testing.T) {
	dir := t.TempDir()
	// A directory where the config file should be: Save cannot write through it.
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.Mkdir(cfgPath, 0o755); err != nil {
		t.Fatal(err)
	}
	in := New(nil, map[string]driver.Driver{"mock": driver.Mock{}},
		filepath.Join(dir, "state.json")).WithConfigPath(cfgPath)
	t.Cleanup(in.Close)

	if err := in.AddProject("api", "mock", t.TempDir()); err == nil {
		t.Fatal("AddProject reported success despite an unwritable config")
	}
	if got := in.Snapshot(); len(got) != 0 {
		t.Fatalf("the project was added in memory anyway: %+v", got)
	}
}

// Selecting the tool a project already uses is not a change, and the
// unconditional reset destroyed the session it was mid-conversation with.
func TestSelectingTheCurrentToolIsANoOp(t *testing.T) {
	in := New([]*Project{{
		Name: "api", Tool: "mock", Dir: t.TempDir(),
		SessionID: "keep-me", Status: driver.StatusIdle,
	}}, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "s.json"))
	t.Cleanup(in.Close)

	if err := in.SetProjectTool(1, "mock"); err != nil {
		t.Fatal(err)
	}
	got, _ := in.Detail(1)
	if got.SessionID != "keep-me" {
		t.Fatalf("a no-op tool change cleared the session: %q", got.SessionID)
	}
}

// A saved entry only applies when its whole identity matches. Matching on the
// name alone handed a repointed project the old one's session and history.
func TestStateIsNotRestoredAcrossAChangeOfToolOrDirectory(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	oldDir := t.TempDir()

	saved := []*Project{{
		Name: "api", Tool: "claude", Dir: oldDir,
		SessionID: "old-session", LastMessage: "from the old repo",
		Status: driver.StatusWaiting,
	}}
	in := New(saved, map[string]driver.Driver{}, statePath)
	in.save()
	in.Close()

	t.Run("different tool", func(t *testing.T) {
		p := &Project{Name: "api", Tool: "codex", Dir: oldDir}
		LoadState(statePath, []*Project{p})
		if p.SessionID != "" {
			t.Errorf("a claude session was restored into a codex project: %q", p.SessionID)
		}
	})

	t.Run("different directory", func(t *testing.T) {
		p := &Project{Name: "api", Tool: "claude", Dir: t.TempDir()}
		LoadState(statePath, []*Project{p})
		if p.SessionID != "" {
			t.Errorf("another repository's session was restored: %q", p.SessionID)
		}
	})

	t.Run("same identity", func(t *testing.T) {
		p := &Project{Name: "api", Tool: "claude", Dir: oldDir}
		LoadState(statePath, []*Project{p})
		if p.SessionID != "old-session" {
			t.Errorf("a matching project should restore, got %q", p.SessionID)
		}
	})
}

// History was bounded by message count alone, so one megabyte-sized reply
// could make state.json enormous and every save and startup pay for it.
func TestHistoryIsBoundedByBytesAsWellAsCount(t *testing.T) {
	p := &Project{Name: "api"}
	huge := strings.Repeat("x", maxMessageBytes*2)
	for range 30 {
		p.appendHistory(Message{Role: "assistant", Content: huge})
	}
	total := 0
	for _, m := range p.History {
		if len(m.Content) > maxMessageBytes {
			t.Fatalf("a single message of %d bytes was stored whole", len(m.Content))
		}
		total += len(m.Content)
	}
	if total > maxHistoryBytes {
		t.Fatalf("history is %d bytes, over the %d budget", total, maxHistoryBytes)
	}
	if len(p.History) == 0 {
		t.Fatal("the newest message should always survive")
	}
}

// A short project name must not be tagged onto a note that merely contains it
// inside another word: wrong tags decide injection, eviction and deletion.
func TestNotesAreTaggedOnWordBoundaries(t *testing.T) {
	in := New([]*Project{{Name: "app"}, {Name: "omni"}}, nil, filepath.Join(t.TempDir(), "s.json"))
	t.Cleanup(in.Close)

	if got := in.projectsNamedIn("nothing happened while mapping"); len(got) != 0 {
		t.Errorf("app was tagged from inside another word: %v", got)
	}
	if got := in.projectsNamedIn("the app depends on omni"); len(got) != 2 {
		t.Errorf("real mentions should tag: %v", got)
	}
}

// Deleting A from a note tagged [A, B] must untag it. Dropping only notes
// whose Projects was exactly [this] left the note tagged forever: deleting B
// afterwards still saw a two-element list.
func TestDeletingAProjectUntagsItsSharedNotes(t *testing.T) {
	in := New([]*Project{{Name: "alpha"}, {Name: "beta"}}, nil, filepath.Join(t.TempDir(), "s.json"))
	t.Cleanup(in.Close)
	in.notes = []Note{
		{Text: "alpha and beta share a queue", Projects: []string{"alpha", "beta"}},
		{Text: "alpha alone", Projects: []string{"alpha"}},
		{Text: "a cross-cutting fact"},
	}

	in.forgetProject("alpha")
	if len(in.notes) != 2 {
		t.Fatalf("expected the alpha-only note to go: %+v", in.notes)
	}
	for _, n := range in.notes {
		for _, p := range n.Projects {
			if p == "alpha" {
				t.Errorf("alpha survived as a tag on %q", n.Text)
			}
		}
	}

	in.forgetProject("beta")
	if len(in.notes) != 1 || in.notes[0].Text != "a cross-cutting fact" {
		t.Fatalf("the shared note should go once both projects have: %+v", in.notes)
	}
}
