// Package ident is the one place that decides when two names or two paths
// refer to the same thing.
//
// It exists because the answer used to differ by caller. Adding a project
// compared directory strings byte for byte; the Stop hook resolved symlinks;
// tmux matching wanted an exact string; state restore keyed on the name as
// typed while lookup compared case-insensitively. So the same repository could
// be added twice under two spellings, and a hook fired from a path spelled a
// third way matched nothing at all.
package ident

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Dir returns a canonical key for a filesystem path: absolute, cleaned, and
// with symlinks resolved when the path exists.
//
// A path that cannot be made absolute or does not exist still gets a stable
// answer — cleaned, and absolute where possible. Identity has to work for
// directories that have not been created yet.
func Dir(path string) string {
	if path == "" {
		return ""
	}
	p := path
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	return p
}

// SameDir reports whether two paths denote the same directory.
//
// os.SameFile is the strongest test available — it compares inode identity, so
// it sees through symlinks, bind mounts and case-insensitive filesystems that
// string comparison cannot. It only works when both paths exist, so canonical
// string comparison is the fallback rather than the primary.
func SameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, cb := Dir(a), Dir(b)
	if ca == cb {
		return true
	}
	fa, ea := os.Stat(ca)
	fb, eb := os.Stat(cb)
	return ea == nil && eb == nil && os.SameFile(fa, fb)
}

// Name canonicalises a project name for uniqueness and lookup. Lookup was
// already case-insensitive; only the uniqueness check was not, which let "API"
// and "api" coexist as separate projects that every directive addressed
// ambiguously.
func Name(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// SameName reports whether two project names refer to the same project.
func SameName(a, b string) bool {
	return Name(a) == Name(b)
}

// maxNameLen bounds a project name so it cannot overflow a sidebar row or a
// prompt budget on its own.
const maxNameLen = 64

// ValidateName rejects names that cannot survive the round trip through the
// supervisor's directive syntax.
//
// Directives are written as [send to NAME: message], so a name containing ':'
// or ']' truncates the parse and addresses a project nobody named. Leading and
// trailing space is rejected rather than trimmed, because a name that displays
// identically to another one is its own kind of ambiguity.
func ValidateName(s string) error {
	if s == "" {
		return fmt.Errorf("name is required")
	}
	if s != strings.TrimSpace(s) {
		return fmt.Errorf("name %q has leading or trailing whitespace", s)
	}
	if len(s) > maxNameLen {
		return fmt.Errorf("name is %d characters; the limit is %d", len(s), maxNameLen)
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
		case r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("name %q contains %q; use letters, digits, dot, underscore or hyphen", s, r)
		}
	}
	return nil
}
