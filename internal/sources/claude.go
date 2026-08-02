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
	"syscall"
	"time"

	"github.com/im-tyler/agent-inbox/internal/feed"
	"github.com/im-tyler/agent-inbox/internal/mux"
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
	// IncludeDead keeps sessions whose process is gone. Off by default.
	IncludeDead bool

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

// enrichment is what a transcript can add that `claude agents --json` does
// not carry — most importantly `ask`, the thing the session actually wants.
//
// "Session X is blocked" is not useful on its own; you still have to open each
// one to find out what it wants. The last thing the assistant said before it
// stopped IS the question, and surfacing it is the difference between a list
// of rows and knowing what your fleet needs.
type enrichment struct {
	branch     string
	lastPrompt string
	ask        string
	// aiTitle is what Claude Code puts in the terminal's title bar, and so
	// the only handle for finding this session's pane: `claude agents
	// --json` reports a slug like "neutron-79" while the pane shows the
	// conversation topic.
	aiTitle string
	lastAt  time.Time
}

// askFrom reduces a final assistant message to the part that is actually a
// question. These messages usually end with the ask after some working-out, so
// the last prose paragraph is nearly always the right pick — and far better
// than the first 200 characters, which is preamble.
func askFrom(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Drop fenced code; a trailing code block is never the question.
	for {
		open := strings.Index(text, "```")
		if open < 0 {
			break
		}
		rest := text[open+3:]
		close := strings.Index(rest, "```")
		if close < 0 {
			text = strings.TrimSpace(text[:open])
			break
		}
		text = strings.TrimSpace(text[:open] + "\n" + rest[close+3:])
	}

	raw := strings.Split(text, "\n\n")
	paragraphs := make([]string, 0, len(raw))
	for _, p := range raw {
		// Strip headings and list scaffolding; the ask is prose.
		p = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(p), "#*->|_ \t"))
		if len(p) >= 12 {
			paragraphs = append(paragraphs, p)
		}
	}
	if len(paragraphs) == 0 {
		return truncate(text, 240)
	}
	// Prefer an actual question. These messages typically close with a
	// summary of what was done and then the ask; taking the last paragraph
	// blindly surfaces the summary and buries the thing needing an answer.
	for i := len(paragraphs) - 1; i >= 0; i-- {
		if strings.Contains(paragraphs[i], "?") {
			return truncate(paragraphs[i], 240)
		}
	}
	return truncate(paragraphs[len(paragraphs)-1], 240)
}

// assistantText pulls the text blocks out of an assistant record, tolerating
// both the string and content-array shapes.
func assistantText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

const tailBytes = 256 * 1024

// jobState is the part of ~/.claude/jobs/<id>/state.json worth reading.
//
// Background agents keep their state here rather than only in a transcript,
// and Claude Code already does the work this tool was about to redo badly:
// `needs` is a one-line statement of what the agent wants from you, `detail`
// summarises what it just did, and `suggestedReply` is sometimes a draft
// answer. Parsing the last assistant paragraph was a guess at exactly this.
type jobState struct {
	State          string `json:"state"`
	Detail         string `json:"detail"`
	Needs          string `json:"needs"`
	SuggestedReply string `json:"suggestedReply"`
	UpdatedAt      string `json:"updatedAt"`
}

func (c Claude) jobRoot() string {
	if c.Root != "" {
		// Tests point Root at a fixture tree; jobs sit beside projects.
		return filepath.Join(filepath.Dir(c.Root), "jobs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "jobs")
}

// job reads a background agent's state. shortID is the `id` from
// `claude agents --json`, which names the directory.
func (c Claude) job(shortID string) jobState {
	root := c.jobRoot()
	if root == "" || shortID == "" {
		return jobState{}
	}
	data, err := os.ReadFile(filepath.Join(root, shortID, "state.json"))
	if err != nil {
		return jobState{}
	}
	var js jobState
	if json.Unmarshal(data, &js) != nil {
		return jobState{}
	}
	return js
}

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
			Type        string `json:"type"`
			GitBranch   string `json:"gitBranch"`
			LastPrompt  string `json:"lastPrompt"`
			AITitle     string `json:"aiTitle"`
			Timestamp   string `json:"timestamp"`
			IsSidechain bool   `json:"isSidechain"`
			Message     *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
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
		if r.AITitle != "" {
			out.aiTitle = r.AITitle
		}
		// A sidechain is a subagent's turn. Its closing message is not what
		// the session is asking you.
		if r.Type == "assistant" && !r.IsSidechain && r.Message != nil {
			if ask := askFrom(assistantText(r.Message.Content)); ask != "" {
				out.ask = ask
			}
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
func actionsFor(a agentInfo, pane string) []feed.Action {
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
	// Typing into the pane is the only way to answer a live Claude Code
	// session — and only when exactly one pane matched. On an ambiguous
	// match the action is withheld rather than guessed at: several panes
	// are titled just "Claude Code", and a wrong guess sends your message
	// to a different agent.
	if pane != "" {
		actions = append(actions, feed.Action{
			Label: "send",
			Run:   []string{"{message}"},
			Pane:  pane,
			Dir:   a.Cwd,
		})
	}
	return actions
}

func (c Claude) item(a agentInfo, e enrichment, js jobState, pane string) feed.Item {
	project := filepath.Base(a.Cwd)
	if project == "." || project == string(filepath.Separator) {
		project = a.Cwd
	}

	// Claude Code says "blocked" itself when a session wants you. Trust it
	// rather than re-deriving the same fact from the transcript.
	state := feed.StateRunning
	prompt := ""
	// `status` is live; `state` is a checkpoint that outlives it. An agent
	// can report state:"blocked" while status says busy — it recorded a
	// question, you never answered, and it carried on anyway. Trusting state
	// alone listed month-old questions as though they were pending.
	if a.State == "blocked" && a.Status != "busy" {
		state = feed.StateBlocked
		// What it actually wants. Best source first: Claude Code's own
		// `needs` line, then the last thing the assistant said. The generic
		// line is the fallback, not the goal — knowing a session is blocked
		// without knowing why still costs you a context switch.
		switch {
		case js.Needs != "":
			prompt = truncate(js.Needs, 240)
		case e.ask != "":
			prompt = e.ask
		case a.Kind == "background":
			prompt = "Background agent finished its turn — waiting on you."
		default:
			prompt = "Finished its turn — waiting on you."
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
	// What it just did, as distinct from what it wants — the two answer
	// different questions and you usually want both before deciding.
	if js.Detail != "" {
		ctx["did"] = truncate(js.Detail, 200)
	}
	// A draft answer, when Claude Code offers one. Shown, never sent: there
	// is no way to write into a live session from outside, and auto-replying
	// on your behalf is not what this is for.
	if js.SuggestedReply != "" {
		ctx["suggested_reply"] = truncate(js.SuggestedReply, 200)
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
		item.Needs = &feed.Needs{Prompt: prompt, Actions: actionsFor(a, pane)}
	}
	return item
}

func (c Claude) Fetch(ctx context.Context) (feed.Feed, error) {
	agents, err := c.listAgents(ctx)
	if err != nil {
		return feed.Feed{}, err
	}
	// Panes are listed once, not per session. A missing multiplexer simply
	// means no send action is offered.
	var panes []mux.Pane
	if m := mux.Detect(); m != nil {
		panes, _ = m.Panes(ctx)
	}

	f := feed.Feed{Schema: feed.Schema, Items: make([]feed.Item, 0, len(agents))}
	for _, a := range agents {
		if !c.alive(a) {
			continue
		}
		e := c.enrich(a.SessionID)
		pane := ""
		if len(panes) > 0 {
			// tmux reports a working directory, which identifies a pane far
			// more reliably than its title; zellij reports neither pid nor
			// path, so there the title is all there is.
			if p, ok := mux.FindByPath(panes, a.Cwd); ok {
				pane = p.ID
			} else if p, ok := mux.FindByTitle(panes, e.aiTitle); ok {
				pane = p.ID
			}
		}
		f.Items = append(f.Items, c.item(a, e, c.job(a.ID), pane))
	}
	return f, nil
}

// alive reports whether a session actually exists right now.
//
// `claude agents --json` also returns agents whose process is long gone: they
// keep whatever state they last recorded, so a question asked six weeks ago
// still reads as "blocked". Those have no pid at all. A pid that no longer
// resolves means the same thing. Either way it is not waiting on you — it is
// over, and listing it as a decision is how the list fills with ghosts.
func (c Claude) alive(a agentInfo) bool {
	if c.IncludeDead {
		return true
	}
	if a.PID <= 0 {
		return false
	}
	proc, err := os.FindProcess(a.PID)
	if err != nil {
		return false
	}
	// Signal 0 checks for existence without touching the process.
	return proc.Signal(syscall.Signal(0)) == nil
}
