package tui

import (
	"path/filepath"
	"strings"

	"agentinbox/internal/feed"
)

// Adoption turns a row in the inbox into a project. The inbox already knows
// every folder you have an agent session in, so it is the project picker —
// there is no second list to keep in step with it.

// candidate is one folder the supervisor could take on, derived from a
// discovered session.
type candidate struct {
	Tool      string // driver name: claude | opencode | codex
	Dir       string
	SessionID string // empty when the supervisor must start its own session
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

// resumableSessionID returns the session id worth adopting, or "" when the
// supervisor has to start its own session in that folder instead.
//
// Claude Code refuses to resume a session that is running right now
// ("currently running as a background agent"), and the claude source reports
// nothing but live agents — `claude agents --json` lists processes, each with
// a pid. So every claude row is unresumable by construction, and adopting its
// id would produce a project whose first send always fails. opencode and codex
// resume from disk and are unaffected.
func resumableSessionID(source, id string) string {
	if toolFor(source) == "claude" {
		return ""
	}
	return id
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
	return candidate{
		Tool:      tool,
		Dir:       dir,
		SessionID: resumableSessionID(item.Source, item.ID),
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
// preview: directives out, markdown markers out, newlines collapsed. The
// same cleanup the threads get, since a preview is a quote from one.
func previewText(content string) string {
	var parts []string
	for _, raw := range strings.Split(stripDirectives(content), "\n") {
		if t, _ := demarkdown(raw); strings.TrimSpace(t) != "" {
			parts = append(parts, strings.TrimSpace(t))
		}
	}
	return strings.Join(parts, " ")
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
