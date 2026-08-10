package sources

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/feed"
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
	// Label is the configured source name. Empty means the built-in default.
	Label string
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

// Name is the configured instance name — see Claude.Name.
func (c Codex) Name() string {
	if c.Label != "" {
		return c.Label
	}
	return "codex"
}

// profileNote — see Claude.profileNote.
func (c Codex) profileNote() string {
	var parts []string
	if c.Bin != "" {
		parts = append(parts, "bin "+c.Bin)
	}
	if c.Root != "" {
		parts = append(parts, "root "+c.Root)
	}
	return strings.Join(parts, ", ")
}

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
// rollouts lists recent session files, newest first.
//
// A missing root is a legitimate empty answer — Codex may simply never have
// run here. A root that exists but cannot be walked is a failure, and used to
// be indistinguishable from the first case.
func (c Codex) rollouts() ([]string, error) {
	root := c.root()
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("codex sessions root %s: %w", root, err)
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
	var walkErr error
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// One unreadable subdirectory should not cost the rest, but it is
			// worth reporting if nothing else works.
			walkErr = err
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		found = append(found, entry{path, info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("codex sessions root %s: %w", root, err)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })

	paths := make([]string, 0, len(found))
	for _, e := range found {
		paths = append(paths, e.path)
	}
	if len(paths) == 0 && walkErr != nil {
		return nil, fmt.Errorf("codex sessions root %s: %w", root, walkErr)
	}
	return paths, nil
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
		Context:   c.contextFor(project, s.cwd),
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
				// See the OpenCode source: open is interactive, reply is not.
				{Label: "open", Run: []string{c.bin(), "resume", s.id}, Dir: s.cwd, Interactive: true},
			},
		}
	}
	return item
}

func (c Codex) Fetch(ctx context.Context) (feed.Feed, error) {
	paths, err := c.rollouts()
	if err != nil {
		return feed.Feed{}, err
	}

	var counts map[string]int
	var dirs map[string]bool
	if !c.AnyDirectory {
		// The configured binary, not the literal "codex" — see the same
		// comment in the OpenCode source.
		var err error
		counts, err = liveDirCounts(ctx, c.bin(), c.timeout())
		if err != nil {
			return feed.Feed{}, fmt.Errorf("%s: %w", c.Name(), err)
		}
		dirs = make(map[string]bool, len(counts))
		for d := range counts {
			dirs[d] = true
		}
	}

	// Newest rollout per directory: a tab has one conversation you care
	// about, not every one that ran in that repo this week.
	//
	// parsed and failed are counted so that "Codex has no active sessions" and
	// "the Codex adapter can no longer read any of your sessions" can be told
	// apart. They need opposite responses from the user, and both used to
	// render as an ordinary empty list.
	parsed, failed := 0, 0
	byDir := map[string][]codexSession{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			failed++
			continue
		}
		s, ok := c.scanRollout(path, info.ModTime())
		if !ok {
			failed++
			continue
		}
		parsed++
		if dirs != nil && !liveNear(dirs, s.cwd) {
			continue
		}
		byDir[s.cwd] = append(byDir[s.cwd], s)
	}

	// Every candidate failed the same way: that is schema drift, not an empty
	// inbox.
	if parsed == 0 && failed > 0 {
		return feed.Feed{}, fmt.Errorf(
			"codex: none of %d recent session file(s) could be parsed — the rollout format has probably changed", failed)
	}

	// Newest rollouts per directory, bounded by the number of codex processes
	// actually running there — see the same reasoning in the OpenCode source.
	f := feed.Feed{Schema: feed.Schema, Items: make([]feed.Item, 0, len(byDir))}
	for dir, group := range byDir {
		sort.Slice(group, func(i, j int) bool { return group[i].last.After(group[j].last) })
		limit := perDirLimit(counts, dir)
		if limit > len(group) {
			limit = len(group)
		}
		for _, s := range group[:limit] {
			f.Items = append(f.Items, c.item(s))
		}
	}
	// Some parsed and some did not: usable, but say the list is partial.
	f.Truncated = failed > 0
	return f, nil
}
