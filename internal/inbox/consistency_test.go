package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/ident"
)

// F14: a deletion must survive a stale writer. B holds alpha in memory from
// before A removed it; B's next save must not resurrect alpha's state entry,
// and a re-add must clear the tombstone so the project can live again.
func TestDeletionSurvivesStaleWriter(t *testing.T) {
	env := newMultiEnv(t)
	a := env.inbox(t, "alpha", "bravo")
	b := env.inbox(t, "alpha", "bravo")

	// A removes alpha (its snapshot: alpha=1, bravo=2).
	if err := a.RemoveProject(1); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// B never refreshed: it still holds alpha and has its own reason to save.
	b.mu.Lock()
	if p, err := b.projectByName("bravo"); err != nil {
		t.Fatal(err)
	} else {
		p.LastMessage = "bravo moved on"
		p.UpdatedAt = time.Now()
	}
	b.mu.Unlock()
	b.save()

	for _, p := range readStateFileOrFatal(t, env.state) {
		if strings.EqualFold(p.Name, "alpha") {
			t.Fatalf("alpha resurrected by a stale writer's save: %+v", p)
		}
	}

	// And a refresh does not adopt it back from anywhere — A bumps the state
	// file so B's mtime check sees a foreign write.
	a.save()
	b.RefreshExternal()
	if _, err := b.projectByName("alpha"); err == nil {
		t.Fatal("alpha reappeared in a refreshed frontend")
	}

	// Re-adding clears the tombstone: the project can persist again.
	if err := b.AddProject("alpha", "mock", filepath.Join(env.base, "alpha")); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	b.save()
	found := false
	for _, p := range readStateFileOrFatal(t, env.state) {
		if strings.EqualFold(p.Name, "alpha") {
			found = true
		}
	}
	if !found {
		t.Fatal("re-added alpha still suppressed by its tombstone")
	}
}

// F16: removing a grouped project must leave a config the next startup can
// validate. Leaving the name in the group's member list bricked startup.
func TestRemoveProjectKeepsConfigValid(t *testing.T) {
	env := newMultiEnv(t)
	cfg := filepath.Join(env.base, "config.json")
	settings := &config.Settings{
		Projects: []config.Project{{Name: "alpha", Tool: "claude", Dir: filepath.Join(env.base, "alpha")}},
		Groups:   []config.Group{{Name: "core", Projects: []string{"alpha"}}},
	}
	if err := config.Save(cfg, settings); err != nil {
		t.Fatal(err)
	}

	in := env.inbox(t, "alpha")
	in.configPath = cfg
	if err := in.RemoveProject(1); err != nil {
		t.Fatalf("remove: %v", err)
	}

	reloaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := config.Validate(reloaded); err != nil {
		t.Fatalf("config invalid after removal — next startup would refuse it: %v", err)
	}
	for _, g := range reloaded.Groups {
		for _, m := range g.Projects {
			if strings.EqualFold(m, "alpha") {
				t.Fatalf("group %q still lists removed alpha", g.Name)
			}
		}
	}
}

// F18: a notes mutation that cannot take the store lock must not proceed
// unlocked — the fallback saved a stale in-memory snapshot over whatever the
// lock holder was writing, which is the lost update the lock exists to stop.
func TestNotesNeverWriteUnlocked(t *testing.T) {
	env := newMultiEnv(t)
	notes := filepath.Join(env.base, "notes.json")

	holder := New(nil, map[string]driver.Driver{}, env.state)
	holder.WithNotesPath(notes)
	holder.AddNotes([]string{"the holder's fact"})

	contender := New(nil, map[string]driver.Driver{}, env.state)
	contender.WithNotesPath(notes)

	// Break the lock path: a directory where the lock file belongs makes the
	// lock unwinnable, deterministically, without waiting out lockWait. The
	// holder's earlier turn left the lock file behind; it must go first or
	// MkdirAll hits a file.
	os.Remove(notes + ".lock")
	if err := os.MkdirAll(notes+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	contender.AddNotes([]string{"the contender's fact"})

	b, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "contender") {
		t.Fatalf("unlocked write reached the store: %s", b)
	}
	var kept []map[string]any
	if json.Unmarshal(b, &kept) != nil || len(kept) != 1 {
		t.Fatalf("holder's store was disturbed: %s", b)
	}

	// Repair: mutations work again, and the earlier one simply never happened.
	os.RemoveAll(notes + ".lock")
	contender.AddNotes([]string{"the contender's fact"})
	if got := contender.Notes(); len(got) != 2 {
		t.Fatalf("after repair: %d notes, want 2", len(got))
	}
}

// F15: a project added with a relative dir from one working directory must
// still address the same repository when driven from another.
func TestRelativeDirResolvedAtCreation(t *testing.T) {
	env := newMultiEnv(t)
	repo := filepath.Join(env.base, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	elswhere := filepath.Join(env.base, "elsewhere")
	if err := os.MkdirAll(elswhere, 0o700); err != nil {
		t.Fatal(err)
	}

	d := &recDriver{seedWith: "s1"}
	in := New(nil, map[string]driver.Driver{"rec": d}, env.state)
	t.Cleanup(in.Close)

	t.Chdir(repo)
	if err := in.AddProject("alpha", "rec", "."); err != nil {
		t.Fatalf("add: %v", err)
	}

	t.Chdir(elswhere)
	if out, ok := in.SendAndWait("alpha", "work", 15*time.Second); !ok || out.Err != nil {
		t.Fatalf("send: ok=%v err=%v", ok, out.Err)
	}
	dirs := d.receivedDirs()
	want := ident.Dir(repo)
	if len(dirs) != 1 || dirs[0] != want {
		t.Fatalf("driver received dir %v, want [%s] — identity moved with the caller's cwd", dirs, want)
	}
}

// F06: an interactive attach holds the project's claim for its lifetime, so
// a managed send from another process cannot start against the same session.
// Releasing the lease reopens the project.
func TestAttachLeaseBlocksForeignSends(t *testing.T) {
	env := newMultiEnv(t)
	a := env.inbox(t, "alpha")
	b := env.inbox(t, "alpha")

	a.projectSetSession(t, "alpha", "s1")
	_, _, lease, err := a.BeginAttach(1)
	if err != nil {
		t.Fatalf("begin attach: %v", err)
	}

	b.projectSetSession(t, "alpha", "s1")
	if out, ok := b.SendAndWait("alpha", "hello", 3*time.Second); ok || out.Err == nil {
		t.Fatalf("foreign send proceeded during an attach: ok=%v err=%v", ok, out.Err)
	}

	lease.Release()
	if out, ok := b.SendAndWait("alpha", "hello", 15*time.Second); !ok || out.Err != nil {
		t.Fatalf("send after lease release: ok=%v err=%v", ok, out.Err)
	}
}
