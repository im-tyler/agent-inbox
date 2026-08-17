package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds a real repository in a temp dir. Parsing git's output is the
// whole job of this package, so the tests run git rather than replaying
// captured strings — a fixture cannot notice that the format moved.
func repo(t *testing.T, commit bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "test")
	if commit {
		write(t, dir, "a.txt", "one\n")
		git(t, dir, "add", "a.txt")
		git(t, dir, "commit", "-q", "-m", "first")
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectCleanRepo(t *testing.T) {
	s := Inspect(t.Context(), repo(t, true))
	if !s.Known() {
		t.Fatalf("Err = %v, want nil", s.Err)
	}
	if s.Branch != "main" {
		t.Errorf("Branch = %q, want main", s.Branch)
	}
	if s.Dirty {
		t.Error("a freshly committed tree reported dirty")
	}
	if s.Head == "" {
		t.Error("Head is empty on a repo with a commit")
	}
	if s.Summary() != "main" {
		t.Errorf("Summary() = %q, want main", s.Summary())
	}
}

// Untracked files count. A tree with a new file in it is not clean, and
// reporting it as clean is how "the agent said it was done" gets believed.
func TestInspectSeesUntrackedFiles(t *testing.T) {
	dir := repo(t, true)
	write(t, dir, "new.txt", "hello\n")
	if s := Inspect(t.Context(), dir); !s.Dirty {
		t.Errorf("untracked file did not mark the tree dirty: %+v", s)
	}
}

func TestInspectSeesModifiedFiles(t *testing.T) {
	dir := repo(t, true)
	write(t, dir, "a.txt", "two\n")
	s := Inspect(t.Context(), dir)
	if !s.Dirty {
		t.Errorf("modified file did not mark the tree dirty: %+v", s)
	}
	if !strings.Contains(s.Summary(), "dirty") {
		t.Errorf("Summary() = %q, want it to mention dirty", s.Summary())
	}
}

// A detached head has no branch. Reporting "(detached)" as the branch name
// would put git's placeholder in the sidebar as though it were one.
func TestInspectDetachedHead(t *testing.T) {
	dir := repo(t, true)
	git(t, dir, "checkout", "-q", "--detach")
	s := Inspect(t.Context(), dir)
	if s.Branch != "" {
		t.Errorf("Branch = %q, want empty on a detached head", s.Branch)
	}
	if !strings.HasPrefix(s.Summary(), "detached@") {
		t.Errorf("Summary() = %q, want it to say detached", s.Summary())
	}
}

// A repository with no commits has no oid. "(initial)" is git's placeholder,
// not a sha.
func TestInspectRepoWithNoCommits(t *testing.T) {
	s := Inspect(t.Context(), repo(t, false))
	if !s.Known() {
		t.Fatalf("Err = %v, want nil", s.Err)
	}
	if s.Head != "" {
		t.Errorf("Head = %q, want empty before the first commit", s.Head)
	}
	if s.Branch != "main" {
		t.Errorf("Branch = %q, want main", s.Branch)
	}
}

// Ahead/behind against a real upstream, via a local clone so no network is
// involved.
func TestInspectAheadOfUpstream(t *testing.T) {
	origin := repo(t, true)
	clone := t.TempDir()
	git(t, clone, "clone", "-q", origin, ".")
	git(t, clone, "config", "user.email", "test@example.invalid")
	git(t, clone, "config", "user.name", "test")
	write(t, clone, "b.txt", "two\n")
	git(t, clone, "add", "b.txt")
	git(t, clone, "commit", "-q", "-m", "second")

	s := Inspect(t.Context(), clone)
	if s.Upstream == "" {
		t.Fatalf("no upstream detected: %+v", s)
	}
	if s.Ahead != 1 || s.Behind != 0 {
		t.Errorf("ahead/behind = %d/%d, want 1/0", s.Ahead, s.Behind)
	}
	if !strings.Contains(s.Summary(), "+1") {
		t.Errorf("Summary() = %q, want it to carry +1", s.Summary())
	}
}

// A project that is not a repository is an ordinary state, not a failure, and
// the caller has to be able to tell it from a broken git.
func TestInspectNonRepoIsNotAFailure(t *testing.T) {
	s := Inspect(t.Context(), t.TempDir())
	if !errors.Is(s.Err, ErrNotARepo) {
		t.Errorf("Err = %v, want ErrNotARepo", s.Err)
	}
	if s.Summary() != "" {
		t.Errorf("Summary() = %q, want empty for a non-repo", s.Summary())
	}
}

func TestInspectMissingDirectory(t *testing.T) {
	s := Inspect(t.Context(), filepath.Join(t.TempDir(), "gone"))
	if s.Known() {
		t.Error("a missing directory reported a readable tree")
	}
}

// An empty directory string is a project with no dir configured. It must not
// become "run git wherever this process happens to be".
func TestInspectEmptyDirDoesNotUseTheProcessCwd(t *testing.T) {
	if s := Inspect(t.Context(), ""); !errors.Is(s.Err, ErrNotARepo) {
		t.Errorf("Err = %v, want ErrNotARepo", s.Err)
	}
}

func TestQueryKinds(t *testing.T) {
	dir := repo(t, true)
	write(t, dir, "a.txt", "changed\n")

	for _, k := range Kinds {
		out, err := Query(t.Context(), dir, k)
		if err != nil {
			t.Errorf("Query(%s): %v", k, err)
			continue
		}
		if strings.TrimSpace(out) == "" && k != KindDiff {
			t.Errorf("Query(%s) returned nothing", k)
		}
	}

	out, err := Query(t.Context(), dir, KindLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "first") {
		t.Errorf("log does not mention the commit:\n%s", out)
	}
}

// The subcommand set is closed. A supervisor's reply is shaped by agent output,
// which is shaped by files and web pages those agents read, so a name in that
// text is not a command.
func TestQueryRejectsUnknownKinds(t *testing.T) {
	for _, bad := range []string{"push", "", "status; rm -rf /", "--exec"} {
		if _, ok := ParseKind(bad); ok {
			t.Errorf("ParseKind(%q) accepted it", bad)
		}
		if _, err := Query(t.Context(), t.TempDir(), Kind(bad)); err == nil {
			t.Errorf("Query(%q) did not refuse", bad)
		}
	}
}

func TestParseKindIsCaseInsensitive(t *testing.T) {
	for _, s := range []string{"STATUS", " Log ", "Diff"} {
		if _, ok := ParseKind(s); !ok {
			t.Errorf("ParseKind(%q) = false", s)
		}
	}
}

// A cancelled context stops the call rather than running it to completion.
func TestQueryRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Query(ctx, repo(t, true), KindStatus); err == nil {
		t.Error("a cancelled query succeeded")
	}
}

func TestTruncateMarksWhatItCut(t *testing.T) {
	got := truncate(strings.Repeat("x", 100), 40)
	if len(got) > 40 {
		t.Errorf("truncate returned %d bytes, want at most 40", len(got))
	}
	if !strings.Contains(got, "[truncated]") {
		t.Errorf("truncation was silent: %q", got)
	}
}

func TestParseAheadBehind(t *testing.T) {
	cases := map[string][2]int{
		"+2 -1": {2, 1},
		"+0 -0": {0, 0},
		"+13 -": {13, 0},
		"":      {0, 0},
		"junk":  {0, 0},
	}
	for in, want := range cases {
		a, b := parseAheadBehind(in)
		if a != want[0] || b != want[1] {
			t.Errorf("parseAheadBehind(%q) = %d/%d, want %d/%d", in, a, b, want[0], want[1])
		}
	}
}
