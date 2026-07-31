package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentinbox/internal/feed"
)

// Claude reads Claude Code's own session transcripts and emits them as inbox
// items.
//
// This is the piece that makes the rebuilt inbox a reader rather than a
// driver. The original agent-inbox parsed the live terminal — spinners,
// redraws, per-vendor quirks — and broke whenever anything changed. Claude
// Code already writes a structured JSONL transcript per session, so the state
// is there to be read rather than scraped.
//
// The signal that matters is the last non-sidechain assistant record's
// stop_reason: "end_turn" means the assistant finished and the human is next,
// "tool_use" means it is still working.
type Claude struct {
	// Root defaults to ~/.claude/projects.
	Root string
	// MaxAge drops sessions untouched for longer than this. Without it the
	// list is a museum: most of the transcripts on disk are months old.
	MaxAge time.Duration
	// StallAfter is how long a session may claim to be mid-tool-call before
	// it is reported as stalled instead of running.
	StallAfter time.Duration
	// AllPerProject shows every recent session rather than the newest one
	// per project. Off by default — you care whether a repo needs you, not
	// about five old sessions in it.
	AllPerProject bool

	Now func() time.Time
}

const (
	defaultMaxAge     = 24 * time.Hour
	defaultStallAfter = 5 * time.Minute
	// Enough of the tail to hold a full turn plus the periodic title and
	// prompt records, without reading megabytes for every session.
	tailBytes = 512 * 1024
)

func (c Claude) Name() string { return "claude-code" }

func (c Claude) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Claude) root() string {
	if c.Root != "" {
		return c.Root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

type record struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	SessionID   string `json:"sessionId"`
	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	AITitle     string `json:"aiTitle"`
	LastPrompt  string `json:"lastPrompt"`
	IsSidechain bool   `json:"isSidechain"`
	Message     *struct {
		StopReason string `json:"stop_reason"`
	} `json:"message"`
}

type session struct {
	id         string
	path       string
	cwd        string
	branch     string
	title      string
	lastPrompt string
	stopReason string
	last       time.Time
}

// scan reads the tail of a transcript and keeps only the last of each thing it
// needs. Transcripts run to thousands of records; reading whole files for 100+
// sessions on every refresh would make the list feel slow for no gain.
func scan(path string, modTime time.Time) (session, error) {
	f, err := os.Open(path)
	if err != nil {
		return session{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return session{}, err
	}
	partial := false
	if info.Size() > tailBytes {
		if _, err := f.Seek(-tailBytes, io.SeekEnd); err != nil {
			return session{}, err
		}
		partial = true
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return session{}, err
	}

	s := session{path: path, last: modTime}
	lines := strings.Split(string(data), "\n")
	if partial && len(lines) > 0 {
		// The seek almost certainly landed mid-line; that fragment is not
		// valid JSON and would only produce a parse error.
		lines = lines[1:]
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.SessionID != "" {
			s.id = r.SessionID
		}
		if r.Cwd != "" {
			s.cwd = r.Cwd
		}
		if r.GitBranch != "" {
			s.branch = r.GitBranch
		}
		if r.AITitle != "" {
			s.title = r.AITitle
		}
		if r.LastPrompt != "" {
			s.lastPrompt = r.LastPrompt
		}
		// A sidechain is a subagent's turn. Letting it set stop_reason would
		// report the parent session as finished while it is still working.
		if r.Type == "assistant" && !r.IsSidechain && r.Message != nil && r.Message.StopReason != "" {
			s.stopReason = r.Message.StopReason
		}
		if t, err := time.Parse(time.RFC3339, r.Timestamp); err == nil && t.After(s.last) {
			s.last = t
		}
	}
	if s.id == "" {
		s.id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return s, nil
}

// newest picks one transcript per project directory unless AllPerProject.
func (c Claude) collect() ([]session, error) {
	root := c.root()
	if root == "" {
		return nil, nil
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	maxAge := c.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}
	cutoff := c.now().Add(-maxAge)

	var out []session
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, dir.Name()))
		if err != nil {
			continue
		}
		var best session
		var found bool
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
			path := filepath.Join(root, dir.Name(), entry.Name())
			s, err := scan(path, info.ModTime())
			if err != nil {
				continue
			}
			if c.AllPerProject {
				out = append(out, s)
				continue
			}
			if !found || s.last.After(best.last) {
				best, found = s, true
			}
		}
		if found && !c.AllPerProject {
			out = append(out, best)
		}
	}
	return out, nil
}

func projectName(s session, path string) string {
	if s.cwd != "" {
		return filepath.Base(s.cwd)
	}
	return filepath.Base(filepath.Dir(path))
}

func (c Claude) item(s session) feed.Item {
	stallAfter := c.StallAfter
	if stallAfter <= 0 {
		stallAfter = defaultStallAfter
	}
	project := projectName(s, s.path)

	state := feed.StateRunning
	prompt := ""
	switch {
	case s.stopReason == "end_turn":
		state = feed.StateBlocked
		prompt = "Finished its turn — waiting for your next instruction."
	case c.now().Sub(s.last) > stallAfter:
		// It claims to be mid-tool-call but nothing has been written for a
		// while: the process is gone, or it is wedged. Either way a human
		// should look, so this is a failure rather than quiet "running".
		state = feed.StateFailed
	}

	title := s.title
	if title == "" {
		title = s.lastPrompt
	}
	if title == "" {
		title = "session in " + project
	}

	item := feed.Item{
		Schema:    feed.Schema,
		Source:    "claude-code",
		ID:        s.id,
		Kind:      "session",
		Title:     title,
		State:     state,
		Since:     s.last.UTC().Format(time.RFC3339),
		UpdatedAt: s.last.UTC().Format(time.RFC3339),
		Context:   map[string]string{"project": project},
	}
	if s.branch != "" {
		item.Context["branch"] = s.branch
	}
	if s.cwd != "" {
		item.Context["cwd"] = s.cwd
	}
	if s.lastPrompt != "" && s.lastPrompt != title {
		item.Context["last_prompt"] = truncate(s.lastPrompt, 120)
	}
	if state == feed.StateFailed {
		item.Context["stalled_for"] = c.now().Sub(s.last).Truncate(time.Minute).String()
	}
	if state == feed.StateBlocked {
		item.Needs = &feed.Needs{
			Prompt: prompt,
			Actions: []feed.Action{
				{Label: "resume", Run: []string{"claude", "--resume", s.id}, Dir: s.cwd},
			},
		}
	}
	return item
}

func truncate(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func (c Claude) Fetch(context.Context) (feed.Feed, error) {
	sessions, err := c.collect()
	if err != nil {
		return feed.Feed{}, fmt.Errorf("read claude sessions: %w", err)
	}
	f := feed.Feed{Schema: feed.Schema, Items: make([]feed.Item, 0, len(sessions))}
	for _, s := range sessions {
		f.Items = append(f.Items, c.item(s))
	}
	return f, nil
}
