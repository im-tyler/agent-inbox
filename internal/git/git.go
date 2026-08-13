// Package git reports what a project's working tree looks like, and answers
// the cheap questions about it.
//
// It exists because of what a question costs here. Everything else this program
// knows about a project it learns by sending a prompt to an agent session in
// that project's directory — a model invocation, thirty seconds, and real
// money. "What branch is neutron on, and has anything changed" is not worth
// that, and a supervisor that has to spend a turn to find out will either spend
// it or guess. A subprocess answers in milliseconds.
//
// Everything here is read-only. Projects are already separate directories and
// separate repositories, so there is nothing to isolate them from, and a
// program that supervises agents has no business rewriting the trees they work
// in behind their backs.
package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrNotARepo means the directory is not inside a git repository. It is not a
// failure: a project may legitimately not be one, and the difference between
// "not a repo" and "git could not be run" is the difference between showing
// nothing and reporting a broken install.
var ErrNotARepo = errors.New("not a git repository")

// State is a working tree at a moment in time.
//
// It is a snapshot and nothing more. The tree changes while this program is not
// looking — and while it is not running at all — so a State is never persisted;
// it is re-read.
type State struct {
	// Branch is the checked-out branch, or "" on a detached head.
	Branch string
	// Head is the abbreviated commit the tree is on.
	Head string
	// Upstream is the tracking branch, empty when the branch has none.
	Upstream string
	// Ahead and Behind count commits against Upstream. Both zero when there is
	// no upstream to compare against, which is not the same as being in sync —
	// Upstream tells them apart.
	Ahead, Behind int
	// Dirty reports uncommitted changes, tracked or untracked.
	Dirty bool
	// Err is why this state is empty. ErrNotARepo is the ordinary case.
	Err error
}

// Known reports whether the state describes a readable repository.
func (s State) Known() bool { return s.Err == nil }

// Summary is the one-line form: branch, divergence, dirtiness. Empty when
// there is nothing to say, so a caller can render it or not without having to
// decide what "nothing" looks like.
func (s State) Summary() string {
	if !s.Known() {
		return ""
	}
	branch := s.Branch
	if branch == "" {
		branch = "detached@" + s.Head
	}
	var extra []string
	if s.Ahead > 0 {
		extra = append(extra, fmt.Sprintf("+%d", s.Ahead))
	}
	if s.Behind > 0 {
		extra = append(extra, fmt.Sprintf("-%d", s.Behind))
	}
	if s.Dirty {
		extra = append(extra, "dirty")
	}
	if len(extra) == 0 {
		return branch
	}
	return branch + " " + strings.Join(extra, " ")
}

const (
	// timeout bounds one invocation. git is local and fast; a repository on a
	// stalled network mount is neither, and the fleet view refreshes on a timer
	// that must not accumulate blocked calls.
	timeout = 5 * time.Second

	// maxInspect and maxQuery cap what git may hand back. A status in a
	// generated-file tree runs to megabytes, and this output is destined for a
	// sidebar row or a model's context window — neither of which has anywhere
	// to put it.
	maxInspect = 64 << 10
	maxQuery   = 64 << 10
)

// Inspect reads dir's working tree.
//
// One invocation: `status --porcelain=v2 --branch` reports the branch, the
// upstream, the ahead/behind counts and every modified path together. Asking
// for them separately would be four processes per project per refresh, which
// on a fleet of a dozen is a fork storm in service of a sidebar line.
func Inspect(ctx context.Context, dir string) State {
	out, err := run(ctx, dir, maxInspect, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return State{Err: err}
	}
	return parseStatus(out)
}

func parseStatus(out string) State {
	var s State
	sc := bufio.NewScanner(strings.NewReader(out))
	// A single path can exceed bufio's default 64KB line limit on a
	// pathologically long filename, and a scanner that stops there would report
	// a dirty tree as clean.
	sc.Buffer(make([]byte, 0, 64<<10), maxInspect)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "# ") {
			// Anything that is not a header is a changed, unmerged, untracked
			// or ignored path. Ignored paths are not requested, so any of them
			// means the tree is dirty.
			if strings.TrimSpace(line) != "" {
				s.Dirty = true
			}
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "# "), " ")
		if !ok {
			continue
		}
		switch key {
		case "branch.oid":
			// "(initial)" on a repository with no commits yet.
			if value != "(initial)" {
				s.Head = abbrev(value)
			}
		case "branch.head":
			// "(detached)" is git saying there is no branch, not a branch
			// called that.
			if value != "(detached)" {
				s.Branch = value
			}
		case "branch.upstream":
			s.Upstream = value
		case "branch.ab":
			s.Ahead, s.Behind = parseAheadBehind(value)
		}
	}
	return s
}

// parseAheadBehind reads git's "+2 -1" divergence field.
func parseAheadBehind(v string) (ahead, behind int) {
	for _, f := range strings.Fields(v) {
		if len(f) < 2 {
			continue
		}
		n, err := strconv.Atoi(f[1:])
		if err != nil {
			continue
		}
		switch f[0] {
		case '+':
			ahead = n
		case '-':
			behind = n
		}
	}
	return ahead, behind
}

func abbrev(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// Kind names a read-only question about a tree. It is a closed set because the
// supervisor picks from it, and the supervisor's output is shaped by replies
// from agents that have read repositories, issues and web pages. A subcommand
// assembled from that text is a command someone else wrote.
type Kind string

const (
	KindStatus Kind = "status"
	KindDiff   Kind = "diff"
	KindLog    Kind = "log"
)

// Kinds is every question that can be asked, for error messages and help.
var Kinds = []Kind{KindStatus, KindDiff, KindLog}

// ParseKind resolves a name to a Kind.
func ParseKind(s string) (Kind, bool) {
	k := Kind(strings.ToLower(strings.TrimSpace(s)))
	switch k {
	case KindStatus, KindDiff, KindLog:
		return k, true
	}
	return "", false
}

// args are the fixed argv for each kind. Fixed is the point: nothing from a
// caller reaches git except the working directory.
func (k Kind) args() []string {
	switch k {
	case KindDiff:
		return []string{"diff", "--stat", "HEAD"}
	case KindLog:
		return []string{"log", "--oneline", "--no-decorate", "-n", "20"}
	default:
		return []string{"status", "--short", "--branch"}
	}
}

// Query answers one of the fixed questions about dir.
//
// The empty string with a nil error means the question has no answer right now
// — a clean tree, no commits in range. That is a real answer and the caller
// should say so rather than treating it as a failure.
func Query(ctx context.Context, dir string, k Kind) (string, error) {
	if _, ok := ParseKind(string(k)); !ok {
		return "", fmt.Errorf("unknown git query %q", k)
	}
	out, err := run(ctx, dir, maxQuery, k.args()...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// run executes git in dir and returns its stdout, capped.
//
// argv, never a shell. The arguments are fixed by this package and the only
// caller-supplied value is the directory, which is passed as a parameter rather
// than interpolated anywhere.
func run(ctx context.Context, dir string, cap int, args ...string) (string, error) {
	if dir == "" {
		return "", ErrNotARepo
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		// git does not give this a distinct exit code, so the message is the
		// only signal. Getting it wrong in the safe direction — reporting a
		// real failure as "not a repo" — hides a broken install, so match
		// narrowly and let everything else through as an error.
		low := strings.ToLower(msg)
		switch {
		case strings.Contains(low, "not a git repository"),
			strings.Contains(low, "does not exist"),
			strings.Contains(low, "no such file or directory"):
			return "", ErrNotARepo
		}
		var ee *exec.Error
		if errors.As(err, &ee) {
			// git is not installed, or not on PATH. Every project will report
			// this, so it has to be legible the first time.
			return "", fmt.Errorf("git could not be run: %w", ee.Err)
		}
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return truncate(stdout.String(), cap), nil
}

// truncate caps s at n bytes on a rune boundary, saying that it did.
// Silent truncation of a diff reads as a smaller diff.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "\n[truncated]"
	cut := n - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}
