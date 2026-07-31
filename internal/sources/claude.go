package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentinbox/internal/feed"
)

// Claude reports your live Claude Code sessions.
//
// State comes from `claude agents --json`, which Claude Code maintains itself:
// which sessions exist right now, whether each is background or interactive,
// and whether it is busy or blocked waiting on you. That is authoritative.
// Deriving the same thing from transcripts was guesswork, and it got the
// actions wrong — a background agent cannot be resumed, only attached to.
//
// Transcripts are still read, but only to enrich an item with the branch and
// last prompt. If that read fails the item survives without them.
type Claude struct {
	// Root defaults to ~/.claude/projects, for transcript enrichment.
	Root string
	// Bin is the claude executable. Defaults to "claude" on PATH.
	Bin string
	// Timeout bounds the `claude agents --json` call.
	Timeout time.Duration

	Now func() time.Time
}

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

// agentInfo is one entry of `claude agents --json`.
type agentInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`   // "background" | "interactive"
	Name      string `json:"name"`   // the session's title
	State     string `json:"state"`  // "blocked" when it wants you
	Status    string `json:"status"` // "busy" | "idle"
	PID       int    `json:"pid"`
	StartedAt int64  `json:"startedAt"` // epoch millis
}

func (c Claude) listAgents(ctx context.Context) ([]agentInfo, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "agents", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("claude agents: %v: %s", err, truncate(detail, 200))
		}
		return nil, fmt.Errorf("claude agents: %w", err)
	}
	var agents []agentInfo
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &agents); err != nil {
		return nil, fmt.Errorf("claude agents: %w", err)
	}
	return agents, nil
}

// enrichment is the handful of things worth pulling out of a transcript that
// `claude agents --json` does not carry.
type enrichment struct {
	branch     string
	lastPrompt string
	lastAt     time.Time
}

const tailBytes = 256 * 1024

// transcriptPath locates a session's JSONL by scanning project directories.
// Claude Code names the file after the session id, but the directory is a
// slug of the working directory that is lossy to reconstruct, so this looks
// rather than computes.
func (c Claude) transcriptPath(sessionID string) string {
	root := c.root()
	if root == "" || sessionID == "" {
		return ""
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	name := sessionID + ".jsonl"
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		path := filepath.Join(root, dir.Name(), name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func (c Claude) enrich(sessionID string) enrichment {
	path := c.transcriptPath(sessionID)
	if path == "" {
		return enrichment{}
	}
	f, err := os.Open(path)
	if err != nil {
		return enrichment{}
	}
	defer f.Close()

	partial := false
	if info, err := f.Stat(); err == nil && info.Size() > tailBytes {
		if _, err := f.Seek(-tailBytes, io.SeekEnd); err == nil {
			partial = true
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return enrichment{}
	}

	var out enrichment
	lines := strings.Split(string(data), "\n")
	if partial && len(lines) > 0 {
		// The seek lands mid-line; that fragment is not valid JSON.
		lines = lines[1:]
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r struct {
			GitBranch  string `json:"gitBranch"`
			LastPrompt string `json:"lastPrompt"`
			Timestamp  string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.GitBranch != "" {
			out.branch = r.GitBranch
		}
		if r.LastPrompt != "" {
			out.lastPrompt = r.LastPrompt
		}
		if t, err := time.Parse(time.RFC3339, r.Timestamp); err == nil && t.After(out.lastAt) {
			out.lastAt = t
		}
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// actionsFor is where getting the state right pays off.
//
// A live session — background or interactive — cannot be resumed: Claude Code
// refuses with "currently running as a background agent (bg)". Attaching goes
// through the agent view, which `--cwd` filters to the right project. Forking
// is offered alongside because it always works and does not disturb the
// running session.
func actionsFor(a agentInfo) []feed.Action {
	actions := []feed.Action{
		{Label: "attach", Run: []string{"claude", "agents", "--cwd", a.Cwd}, Dir: a.Cwd},
	}
	if a.SessionID != "" {
		actions = append(actions, feed.Action{
			Label: "fork",
			Run:   []string{"claude", "--resume", a.SessionID, "--fork-session"},
			Dir:   a.Cwd,
		})
	}
	return actions
}

func (c Claude) item(a agentInfo, e enrichment) feed.Item {
	project := filepath.Base(a.Cwd)
	if project == "." || project == string(filepath.Separator) {
		project = a.Cwd
	}

	// Claude Code says "blocked" itself when a session wants you. Trust it
	// rather than re-deriving the same fact from the transcript.
	state := feed.StateRunning
	prompt := ""
	if a.State == "blocked" {
		state = feed.StateBlocked
		prompt = "Finished its turn — waiting on you."
		if a.Kind == "background" {
			prompt = "Background agent finished its turn — waiting on you."
		}
	}

	since := e.lastAt
	if since.IsZero() && a.StartedAt > 0 {
		since = time.UnixMilli(a.StartedAt)
	}
	if since.IsZero() {
		since = c.now()
	}

	title := a.Name
	if title == "" {
		title = e.lastPrompt
	}
	if title == "" {
		title = "session in " + project
	}

	ctx := map[string]string{"project": project}
	if a.Kind != "" {
		ctx["kind"] = a.Kind
	}
	if a.Status != "" {
		ctx["status"] = a.Status
	}
	if a.Cwd != "" {
		ctx["cwd"] = a.Cwd
	}
	if e.branch != "" {
		ctx["branch"] = e.branch
	}
	if e.lastPrompt != "" && e.lastPrompt != title {
		ctx["last_prompt"] = truncate(e.lastPrompt, 120)
	}

	item := feed.Item{
		Schema:    feed.Schema,
		Source:    "claude-code",
		ID:        a.SessionID,
		Kind:      "session",
		Title:     title,
		State:     state,
		Since:     since.UTC().Format(time.RFC3339),
		UpdatedAt: since.UTC().Format(time.RFC3339),
		Context:   ctx,
	}
	if item.ID == "" {
		item.ID = a.ID
	}
	if state == feed.StateBlocked {
		item.Needs = &feed.Needs{Prompt: prompt, Actions: actionsFor(a)}
	}
	return item
}

func (c Claude) Fetch(ctx context.Context) (feed.Feed, error) {
	agents, err := c.listAgents(ctx)
	if err != nil {
		return feed.Feed{}, err
	}
	f := feed.Feed{Schema: feed.Schema, Items: make([]feed.Item, 0, len(agents))}
	for _, a := range agents {
		f.Items = append(f.Items, c.item(a, c.enrich(a.SessionID)))
	}
	return f, nil
}
