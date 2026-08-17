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
	p := provisionOne(t, dd, &config.Settings{})

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
	provisionOne(t, dd, &config.Settings{})
	path := filepath.Join(dd, "supervisor", "AGENTS.md")
	if err := os.WriteFile(path, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	provisionOne(t, dd, &config.Settings{})
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
	provisionOne(t, dd, &config.Settings{})
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

	p := provisionOne(t, dd, &cfg)
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
	got := withSupervisors([]*inbox.Project{king}, []*inbox.Project{{Name: "omni"}, {Name: "akiroo"}})
	if len(got) != 3 || got[0].Name != "supervisor" {
		t.Fatalf("order = %v", names(got))
	}
}

// The supervisor's name is reserved, and config validation is where that is
// enforced.
//
// This used to be read the other way round: a project carrying the
// supervisor's name was treated as the user electing it as their supervisor.
// But config has no way to say that, and the accidental case is far more
// likely than the deliberate one — naming a repository "supervisor" silently
// suppressed the isolated supervisor, excluded that repo from its own fleet,
// and ran supervision prompts inside a working tree.
func TestAProjectMayNotClaimTheSupervisorsName(t *testing.T) {
	for _, name := range []string{"supervisor", "Supervisor", "SUPERVISOR"} {
		cfg := &config.Settings{Projects: []config.Project{{Name: name, Tool: "claude", Dir: "/mine"}}}
		if err := config.Validate(cfg); err == nil {
			t.Errorf("%q was accepted; the supervisor's name is reserved", name)
		}
	}
}

// A custom king.name is reserved on the same terms.
func TestACustomSupervisorNameIsAlsoReserved(t *testing.T) {
	cfg := &config.Settings{Projects: []config.Project{{Name: "boss", Tool: "claude", Dir: "/mine"}}}
	cfg.King.Name = "boss"
	if err := config.Validate(cfg); err == nil {
		t.Error("a project matching a custom king.name was accepted")
	}
}

// And the supervisor is always its own entry, never one of the configured
// projects wearing the name.
func TestWithSupervisorAlwaysPrependsItsOwn(t *testing.T) {
	king := &inbox.Project{Name: "supervisor", Dir: "/default"}
	got := withSupervisors([]*inbox.Project{king}, []*inbox.Project{{Name: "omni"}})
	if len(got) != 2 || got[0].Dir != "/default" {
		t.Fatalf("supervisor should lead with its own dir, got %v", names(got))
	}
}

func names(ps []*inbox.Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// provisionOne is the single-supervisor path, which is what most of these
// tests are about.
func provisionOne(t *testing.T, dataDir string, cfg *config.Settings) *inbox.Project {
	t.Helper()
	kings, _, err := supervisors(dataDir, cfg)
	if err != nil {
		t.Fatalf("provisioning failed: %v", err)
	}
	if len(kings) != 1 {
		t.Fatalf("provisioned %d supervisors, want 1", len(kings))
	}
	return kings[0]
}

// Each group gets a supervisor of its own, named and housed after the group.
// Two supervisors sharing a folder would share a session's working directory,
// which is the one thing the dedicated folder exists to prevent.
func TestGroupsProvisionASupervisorEach(t *testing.T) {
	dd := t.TempDir()
	cfg := &config.Settings{
		Projects: []config.Project{
			{Name: "teploy", Tool: "claude", Dir: "/a"},
			{Name: "neutron", Tool: "claude", Dir: "/b"},
		},
		Groups: []config.Group{
			{Name: "infra", Projects: []string{"teploy"}},
			{Name: "product", Projects: []string{"neutron"}},
		},
	}
	kings, groups, err := supervisors(dd, cfg)
	if err != nil {
		t.Fatalf("provisioning failed: %v", err)
	}
	if len(kings) != 2 || len(groups) != 2 {
		t.Fatalf("got %d kings and %d groups, want 2 and 2", len(kings), len(groups))
	}
	if kings[0].Name != "supervisor-infra" || kings[1].Name != "supervisor-product" {
		t.Errorf("names = %v, want supervisor-infra and supervisor-product", names(kings))
	}
	if kings[0].Dir == kings[1].Dir {
		t.Fatalf("both supervisors share a directory: %s", kings[0].Dir)
	}
	for _, k := range kings {
		if fi, err := os.Stat(k.Dir); err != nil || !fi.IsDir() {
			t.Errorf("%s: dir not created: %v", k.Name, err)
		}
	}
	if groups[0].King != "supervisor-infra" || groups[0].Projects[0] != "teploy" {
		t.Errorf("group 0 = %+v", groups[0])
	}
}

// With groups configured the singular supervisor is not provisioned, so
// "supervisor" is an ordinary name a project may take.
func TestGroupedFleetDoesNotReserveTheDefaultName(t *testing.T) {
	cfg := &config.Settings{
		Projects: []config.Project{{Name: "supervisor", Tool: "claude", Dir: "/a"}},
		Groups:   []config.Group{{Name: "infra", Projects: []string{"supervisor"}}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Errorf("rejected a project named supervisor in a grouped fleet: %v", err)
	}
}

// A project in two groups has two supervisors, which is the one thing the
// partition is supposed to rule out.
func TestAProjectBelongsToOneGroup(t *testing.T) {
	cfg := &config.Settings{
		Projects: []config.Project{{Name: "teploy", Tool: "claude", Dir: "/a"}},
		Groups: []config.Group{
			{Name: "infra", Projects: []string{"teploy"}},
			{Name: "product", Projects: []string{"teploy"}},
		},
	}
	if err := config.Validate(cfg); err == nil {
		t.Error("a project claimed by two groups was accepted")
	}
}

// A group naming a project that does not exist is a typo, and silently
// producing a supervisor with an empty fleet would hide it.
func TestGroupCannotNameAnUnknownProject(t *testing.T) {
	cfg := &config.Settings{
		Projects: []config.Project{{Name: "teploy", Tool: "claude", Dir: "/a"}},
		Groups:   []config.Group{{Name: "infra", Projects: []string{"telpoy"}}},
	}
	if err := config.Validate(cfg); err == nil {
		t.Error("a group naming an unknown project was accepted")
	}
}

func TestDuplicateGroupNamesRejected(t *testing.T) {
	cfg := &config.Settings{
		Groups: []config.Group{{Name: "infra"}, {Name: "Infra"}},
	}
	if err := config.Validate(cfg); err == nil {
		t.Error("two groups with the same name were accepted")
	}
}
