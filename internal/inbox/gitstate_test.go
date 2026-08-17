package inbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/git"
)

func tempRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "first"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

func gitFixture(t *testing.T) (*Inbox, string) {
	t.Helper()
	repo := tempRepo(t)
	projects := []*Project{
		{Name: "supervisor", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
		{Name: "repo", Tool: "mock", Dir: repo, Status: driver.StatusIdle},
		{Name: "plain", Tool: "mock", Dir: t.TempDir(), Status: driver.StatusIdle},
	}
	in := New(projects, map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(t.TempDir(), "state.json")).
		WithKing("supervisor")
	t.Cleanup(in.Close)
	return in, repo
}

func TestRefreshGitFilesEachProjectsTree(t *testing.T) {
	in, _ := gitFixture(t)
	in.RefreshGit()

	if got := in.GitStateOf("repo"); got.Branch != "main" {
		t.Errorf("repo branch = %q (err %v), want main", got.Branch, got.Err)
	}
	// A project that is not a repository is an ordinary state, and has to stay
	// distinguishable from one whose git call failed.
	if got := in.GitStateOf("plain"); !errors.Is(got.Err, git.ErrNotARepo) {
		t.Errorf("plain err = %v, want ErrNotARepo", got.Err)
	}
}

func TestRefreshGitSeesTheTreeChange(t *testing.T) {
	in, repo := gitFixture(t)
	in.RefreshGit()
	if in.GitStateOf("repo").Dirty {
		t.Fatal("a clean tree reported dirty")
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in.RefreshGit()
	if !in.GitStateOf("repo").Dirty {
		t.Error("a new file did not show up on the next refresh")
	}
}

// Tree state is transient. Persisting it would restore a branch read at some
// earlier point and present it as current.
func TestGitStateIsNotPersisted(t *testing.T) {
	in, _ := gitFixture(t)
	in.RefreshGit()
	in.save()

	b, err := os.ReadFile(in.statePath)
	if err != nil {
		t.Fatalf("state not written: %v", err)
	}
	for _, leaked := range []string{`"Branch"`, `"branch"`, "main"} {
		if strings.Contains(string(b), leaked) {
			t.Errorf("%s reached state.json:\n%s", leaked, b)
		}
	}
}

func TestQueryGitAnswersFromTheProjectsDirectory(t *testing.T) {
	in, _ := gitFixture(t)
	out, err := in.QueryGit("repo", git.KindLog)
	if err != nil {
		t.Fatalf("QueryGit: %v", err)
	}
	if !strings.Contains(out, "first") {
		t.Errorf("log does not mention the commit:\n%s", out)
	}
}

func TestQueryGitRejectsAnUnknownProject(t *testing.T) {
	in, _ := gitFixture(t)
	if _, err := in.QueryGit("nope", git.KindStatus); err == nil {
		t.Error("a query against an unknown project succeeded")
	}
}

// The refresh runs on a timer for minutes. Without a stop signal it outlives
// the inbox and goes on writing to state nobody owns.
func TestGitRefreshLoopStopsOnClose(t *testing.T) {
	in, _ := gitFixture(t)
	in.WithGitRefresh(10 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { in.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; the refresh loop outlived it")
	}
}

// A refresh in flight must not file its answer against a project that has
// since been repointed at a different directory.
func TestRefreshGitIgnoresAStaleTarget(t *testing.T) {
	in, _ := gitFixture(t)
	in.RefreshGit()
	if in.GitStateOf("repo").Branch != "main" {
		t.Fatal("setup: expected a branch")
	}

	// Repoint the project at somewhere that is not a repository, then refresh:
	// the stale "main" must not survive.
	in.mu.Lock()
	p, _ := in.projectByName("repo")
	p.Dir = t.TempDir()
	in.mu.Unlock()

	in.RefreshGit()
	if got := in.GitStateOf("repo"); got.Branch != "" {
		t.Errorf("branch = %q after the project moved, want empty", got.Branch)
	}
}
