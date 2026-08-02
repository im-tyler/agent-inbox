package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/inbox"
)

// First run gives the supervisor a folder of its own and a brief to start
// from. The folder is the point: an agent session is anchored to a working
// directory, and the old default anchored it inside a project it supervised.
func TestSupervisorProvisionsItsOwnFolder(t *testing.T) {
	dd := t.TempDir()
	p := supervisorProject(dd, &config.Settings{})

	if p.Name != "supervisor" || p.Tool != "claude" {
		t.Errorf("supervisor = %+v, want name supervisor on claude", p)
	}
	want := filepath.Join(dd, "supervisor")
	if p.Dir != want {
		t.Errorf("dir = %q, want %q", p.Dir, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("supervisor dir not created: %v", err)
	}
	brief, err := os.ReadFile(filepath.Join(want, "AGENTS.md"))
	if err != nil {
		t.Fatalf("brief not written: %v", err)
	}
	if !strings.Contains(string(brief), "You cannot read their files") {
		t.Errorf("brief does not state the constraint:\n%s", brief)
	}
}

// The brief is the user's once written. Rewriting it on every start would
// silently discard whatever they told their supervisor to be.
func TestSupervisorBriefIsNotOverwritten(t *testing.T) {
	dd := t.TempDir()
	supervisorProject(dd, &config.Settings{})
	path := filepath.Join(dd, "supervisor", "AGENTS.md")
	if err := os.WriteFile(path, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	supervisorProject(dd, &config.Settings{})
	got, _ := os.ReadFile(path)
	if string(got) != "mine" {
		t.Errorf("brief was overwritten: %q", got)
	}
}

// A CLAUDE.md counts as a brief too — writing AGENTS.md beside it would leave
// two files claiming to be the supervisor's instructions.
func TestSupervisorRespectsAnExistingClaudeMd(t *testing.T) {
	dd := t.TempDir()
	dir := filepath.Join(dd, "supervisor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	supervisorProject(dd, &config.Settings{})
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil {
		t.Error("a second brief was written beside CLAUDE.md")
	}
}

func TestSupervisorHonoursConfigOverrides(t *testing.T) {
	dd := t.TempDir()
	var cfg config.Settings
	cfg.King.Name = "boss"
	cfg.King.Tool = "codex"
	cfg.King.Dir = filepath.Join(dd, "elsewhere")

	p := supervisorProject(dd, &cfg)
	if p.Name != "boss" || p.Tool != "codex" || p.Dir != cfg.King.Dir {
		t.Errorf("overrides ignored: %+v", p)
	}
	if _, err := os.Stat(cfg.King.Dir); err != nil {
		t.Errorf("custom dir not created: %v", err)
	}
}

// The supervisor leads the list so it is never buried among the projects.
func TestWithSupervisorPrepends(t *testing.T) {
	king := &inbox.Project{Name: "supervisor"}
	got := withSupervisor(king, []*inbox.Project{{Name: "omni"}, {Name: "akiroo"}})
	if len(got) != 3 || got[0].Name != "supervisor" {
		t.Fatalf("order = %v", names(got))
	}
}

// A user who has configured a project under the supervisor's name has said
// where their supervisor lives. Prepending a second one would give them two.
func TestWithSupervisorDefersToAConfiguredOne(t *testing.T) {
	king := &inbox.Project{Name: "supervisor", Dir: "/default"}
	configured := []*inbox.Project{{Name: "supervisor", Dir: "/mine"}, {Name: "omni"}}
	got := withSupervisor(king, configured)
	if len(got) != 2 {
		t.Fatalf("a duplicate supervisor was added: %v", names(got))
	}
	if got[0].Dir != "/mine" {
		t.Errorf("dir = %q, want the configured one", got[0].Dir)
	}
}

func names(ps []*inbox.Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}
