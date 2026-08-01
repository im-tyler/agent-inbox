package sources

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentinbox/internal/feed"
)

// Codex reports your live codex sessions.
//
// Codex records each session as a "rollout" JSONL under
// ~/.codex/sessions/<year>/<month>/<day>/. The first record is a session_meta
// carrying the id and working directory; the stream that follows ends, when a
// turn finishes, with an event_msg of type "task_complete". That marker is the
// state signal — across every recent session on this machine the last
// event_msg was task_complete, so its absence means the turn is still running.
//
// The last agent_message before it holds the text the session ended on, which
// is where the ask comes from. Codex publishes no distilled "needs" line the
// way Claude Code does, so this is derived rather than declared.
//
// Liveness is the same lsof cwd match used for opencode, and for the same
// reason: 91 rollouts sit on disk and almost all ended on task_complete, so
// without it the list fills with months of finished work that all looks like
// it is waiting on you.
type Codex struct {
	// Root defaults to ~/.codex/sessions.
	Root string
	// Bin is the codex executable. Defaults to "codex" on PATH.
	Bin string
	// MaxAge bounds how far back rollouts are read.
	MaxAge  time.Duration
	Timeout time.Duration
	// AnyDirectory keeps sessions with no matching live process. Off by
	// default — those are finished sessions, not open tabs.
	AnyDirectory bool

	Now func() time.Time
}

const codexDefaultMaxAge = 7 * 24 * time.Hour

// metaLineMax bounds the session_meta line. It embeds the full base
// instructions and was measured at ~19KB on this machine; 4MB is slack enough
// that a longer prompt does not silently drop the session.
const metaLineMax = 4 * 1024 * 1024

func (c Codex) Name() string { return "codex" }

func (c Codex) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "codex"
}

func (c Codex) root() string {
	if c.Root != "" {
		return c.Root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

func (c Codex) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Codex) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

type codexSession struct {
	id        string
	cwd       string
	lastEvent string
	ask       string
	last      time.Time
}

// rollouts lists recent session files, newest first. The tree is
// year/month/day so a full walk is cheap, but reading every file is not —
// mtime does the filtering before anything is parsed.
func (c Codex) rollouts() []string {
	root := c.root()
	if root == "" {
		return nil
	}
	maxAge := c.MaxAge
	if maxAge <= 0 {
		maxAge = codexDefaultMaxAge
	}
	cutoff := c.now().Add(-maxAge)

	type entry struct {
		path string
		mod  time.Time
	}
	var found []entry
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		found = append(found, entry{path, info.ModTime()})
		return nil
	})
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })

	paths := make([]string, 0, len(found))
	for _, e := range found {
		paths = append(paths, e.path)
	}
	return paths
}

// scanRollout reads a session's id and cwd from the head, then its ending
// state from the tail. Rollouts run to thousands of records, so only the ends
// are parsed.
func (c Codex) scanRollout(path string, mod time.Time) (codexSession, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexSession{}, false
	}
	defer f.Close()

	s := codexSession{last: mod}

	// Head: session_meta is the first record. It carries the full base
	// instructions, so the line runs to tens of kilobytes — a fixed read
	// buffer silently truncates it and the whole session is then skipped for
	// having no id. Scan a line at a time with room to spare instead.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), metaLineMax)
	for scanner.Scan() {
		var r struct {
			Type    string `json:"type"`
			Payload struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			continue
		}
		if r.Type != "session_meta" {
			// session_meta leads the file; anything else means this rollout
			// has no usable header.
			break
		}
		s.id, s.cwd = r.Payload.ID, r.Payload.Cwd
		break
	}
	if s.id == "" || s.cwd == "" {
		return codexSession{}, false
	}

	// Tail: the last event_msg type, and the last agent_message text.
	info, err := f.Stat()
	if err != nil {
		return codexSession{}, false
	}
	partial := false
	if info.Size() > tailBytes {
		if _, err := f.Seek(-tailBytes, io.SeekEnd); err == nil {
			partial = true
		}
	} else if _, err := f.Seek(0, io.SeekStart); err != nil {
		return codexSession{}, false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return codexSession{}, false
	}
	lines := strings.Split(string(data), "\n")
	if partial && len(lines) > 0 {
		lines = lines[1:]
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.Type == "event_msg" && r.Payload.Type != "" {
			s.lastEvent = r.Payload.Type
			if r.Payload.Type == "agent_message" && r.Payload.Message != "" {
				s.ask = askFrom(r.Payload.Message)
			}
		}
		if t, err := time.Parse(time.RFC3339, r.Timestamp); err == nil && t.After(s.last) {
			s.last = t
		}
	}
	return s, true
}

func (c Codex) item(s codexSession) feed.Item {
	project := filepath.Base(s.cwd)
	if project == "." || project == string(filepath.Separator) {
		project = s.cwd
	}

	state := feed.StateRunning
	if s.lastEvent == "task_complete" {
		state = feed.StateBlocked
	}

	// Codex records no session title. Using the closing message as one makes
	// every row a paragraph; the ask belongs in the prompt, where the other
	// sources put it.
	item := feed.Item{
		Schema:    feed.Schema,
		Source:    "codex",
		ID:        s.id,
		Kind:      "session",
		Title:     "session in " + project,
		State:     state,
		Since:     s.last.UTC().Format(time.RFC3339),
		UpdatedAt: s.last.UTC().Format(time.RFC3339),
		Context:   map[string]string{"project": project, "cwd": s.cwd},
	}
	if s.lastEvent != "" {
		item.Context["last_event"] = s.lastEvent
	}
	if state == feed.StateBlocked {
		prompt := s.ask
		if prompt == "" {
			prompt = "Finished its turn — waiting on you."
		}
		item.Needs = &feed.Needs{
			Prompt: prompt,
			Actions: []feed.Action{
				// Like opencode and unlike Claude Code, codex accepts a
				// prompt into an existing session.
				{Label: "reply", Run: []string{c.bin(), "exec", "resume", s.id, "{message}"}, Dir: s.cwd},
				{Label: "open", Run: []string{c.bin(), "resume", s.id}, Dir: s.cwd},
			},
		}
	}
	return item
}

func (c Codex) Fetch(ctx context.Context) (feed.Feed, error) {
	paths := c.rollouts()

	var dirs map[string]bool
	if !c.AnyDirectory {
		dirs = liveDirs(ctx, "codex", c.timeout())
	}

	// Newest rollout per directory: a tab has one conversation you care
	// about, not every one that ran in that repo this week.
	newest := map[string]codexSession{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		s, ok := c.scanRollout(path, info.ModTime())
		if !ok {
			continue
		}
		if dirs != nil && !liveNear(dirs, s.cwd) {
			continue
		}
		if prev, seen := newest[s.cwd]; !seen || s.last.After(prev.last) {
			newest[s.cwd] = s
		}
	}

	f := feed.Feed{Schema: feed.Schema, Items: make([]feed.Item, 0, len(newest))}
	for _, s := range newest {
		f.Items = append(f.Items, c.item(s))
	}
	return f, nil
}
