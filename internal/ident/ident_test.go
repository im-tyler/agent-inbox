package ident

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSameDirSeesThroughASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "repo")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !SameDir(real, link) {
		t.Error("a symlink to a directory is that directory")
	}
}

func TestSameDirNormalisesRelativeAndRedundantPaths(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	// The same directory spelled with a redundant hop through a sibling.
	round := filepath.Join(root, "repo", "..", "repo")
	if !SameDir(repo, round) {
		t.Error("a path with .. in it still names the same directory")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	defer func() { _ = os.Chdir(cwd) }()
	if !SameDir("repo", repo) {
		t.Error("a relative path should resolve against the working directory")
	}
}

func TestSameDirIsFalseForDifferentDirectories(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if SameDir(a, b) {
		t.Error("distinct directories must not compare equal")
	}
	if SameDir("", a) || SameDir(a, "") {
		t.Error("an empty path names nothing and matches nothing")
	}
}

func TestNameIsCaseAndSpaceInsensitive(t *testing.T) {
	if !SameName("API", "api") {
		t.Error("project names are matched case-insensitively")
	}
	if !SameName(" api ", "api") {
		t.Error("surrounding space is not part of a name")
	}
	if SameName("api", "apiv2") {
		t.Error("distinct names must not match")
	}
}

// Names are embedded in [send to NAME: message], so a name carrying the
// syntax's own punctuation would truncate the parse and address a project
// nobody named.
func TestValidateNameRejectsWhatTheDirectiveSyntaxCannotCarry(t *testing.T) {
	for _, bad := range []string{"", " api", "api ", "foo:bar", "foo]bar", "a b"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, good := range []string{"api", "omni-analyst", "teploy_cli", "v2.site", "A1"} {
		if err := ValidateName(good); err != nil {
			t.Errorf("%q should be accepted: %v", good, err)
		}
	}
}

func TestValidateNameBoundsLength(t *testing.T) {
	long := make([]byte, maxNameLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateName(string(long)); err == nil {
		t.Error("an unbounded name should be rejected")
	}
}
