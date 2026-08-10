package tui

import (
	"path/filepath"
	"strings"

	"github.com/im-tyler/agent-inbox/internal/feed"
)

// Adoption turns a row in the inbox into a project. The inbox already knows
// every folder you have an agent session in, so it is the project picker —
// there is no second list to keep in step with it.

// candidate is one folder the supervisor could take on, derived from a
// discovered session.
type candidate struct {
	Tool      string // driver name: claude | opencode | codex
	Dir       string
	SessionID string // a session this project can resume as its own
	ForkFrom  string // a live session to seed from; the fork becomes ours
}

// Name is the project name a candidate defaults to.
func (c candidate) Name() string {
	base := filepath.Base(c.Dir)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return c.Dir
	}
	return base
}

// toolFor maps a source's wire name to the driver that can talk to it. An
// unrecognized source is not a candidate: the supervisor has no way to send
// to something it has no driver for.
func toolFor(source string) string {
	switch source {
	case "claude-code", "claude":
		return "claude"
	case "opencode":
		return "opencode"
	case "codex":
		return "codex"
	}
	return ""
}

// adoptSession decides how the supervisor picks a discovered session up:
// resume it, fork it, or start clean.
//
// opencode and codex sessions are read back from disk and are nobody else's
// while we use them, so their id is adopted directly.
//
// Every claude row is a live process — `claude agents --json` lists agents
// with pids, and the source filters out the dead ones — so resuming in place
// would put two writers on one transcript. Forking is the answer instead of
// starting blank: `--fork-session` seeds a new session from the original's
// history, works while the original is mid-turn, and leaves it untouched. The
// supervisor gets the context, which was the entire point of adopting rather
// than adding a folder.
func adoptSession(source, id string) (sessionID, forkFrom string) {
	if toolFor(source) == "claude" {
		return "", id
	}
	return id, ""
}

// candidateFrom derives an adoptable project from an inbox row, reporting
// false for rows the supervisor could never drive: a deploy, a CI run, or any
// session that never said which folder it is in.
func candidateFrom(item feed.Item) (candidate, bool) {
	tool := toolFor(item.Source)
	if tool == "" {
		return candidate{}, false
	}
	dir := item.Context["cwd"]
	if dir == "" {
		return candidate{}, false
	}
	sessionID, forkFrom := adoptSession(item.Source, item.ID)
	return candidate{
		Tool:      tool,
		Dir:       dir,
		SessionID: sessionID,
		ForkFrom:  forkFrom,
	}, true
}

// stripDirectives removes the king's machine syntax from what a human reads.
// [send to X: Y] and [note: ...] are instructions to this program, not speech
// — the dispatch shows up as receipts and the note as remembered fact, so
// printing the raw line too is the same information wearing a worse shape.
//
// Only whole lines are dropped, matching how the parsers read them: prose
// that merely mentions the syntax stays.
// previewText flattens a message to one line for a sidebar row or a list
// preview: markdown markers out, newlines collapsed. isKing selects whether
// supervisor directives are also removed — see displayContent.
func previewText(content string, isKing bool) string {
	var parts []string
	for _, raw := range strings.Split(displayContent(isKing, content), "\n") {
		if t, _ := demarkdown(raw); strings.TrimSpace(t) != "" {
			parts = append(parts, strings.TrimSpace(t))
		}
	}
	return strings.Join(parts, " ")
}

// displayContent is what a human should see of one message.
//
// Directive stripping applies only to the supervisor's own output. It was
// applied to every project's history, so a coding agent that legitimately
// wrote a line like "[note: this is the syntax]" — quoting documentation,
// explaining the feature, or echoing a file — had that line silently deleted
// from what is meant to be its full transcript. That also hides the evidence
// when the text arrived through prompt injection, which is exactly when you
// want to see it.
func displayContent(isKing bool, content string) string {
	if !isKing {
		return strings.TrimSpace(content)
	}
	return stripDirectives(content)
}

func stripDirectives(content string) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := strings.ToLower(strings.TrimSpace(ln))
		if strings.HasSuffix(t, "]") && (strings.HasPrefix(t, "[send to ") || strings.HasPrefix(t, "[note")) {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
