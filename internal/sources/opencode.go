package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/feed"
)

// OpenCode reports your live opencode sessions.
//
// opencode publishes no equivalent of `claude agents --json`, so liveness and
// state are assembled from two things:
//
//   - opencode.db — every session's id, title, directory and last-updated
//     time, plus the `finish` value of its most recent message. "stop" means
//     the assistant ended its turn and you are next; "tool-calls" means it is
//     still working.
//   - the running opencode processes' working directories, via lsof — which
//     of those sessions is actually open in a tab right now.
//
// The cwd match is the equivalent of the pid check on the Claude source, and
// for the same reason: the database holds every session ever created, and
// almost all of them ended on "stop". Without it the inbox fills with months
// of finished work that all looks like it is waiting on you.
//
// Unlike the Claude source this reads a private database, so it is the part
// most likely to break on an opencode upgrade. Everything degrades to an empty
// feed rather than a wrong one.
type OpenCode struct {
	// Bin is the opencode executable. Defaults to "opencode" on PATH.
	Bin string
	// DB is the opencode SQLite path. Defaults to
	// ~/.local/share/opencode/opencode.db.
	DB string
	// Limit bounds how many recent sessions are considered.
	Limit   int
	Timeout time.Duration
	// AnyDirectory keeps sessions with no matching live process. Off by
	// default — those are finished sessions, not open tabs.
	AnyDirectory bool

	Now func() time.Time
}

func (o OpenCode) Name() string { return "opencode" }

func (o OpenCode) bin() string {
	if o.Bin != "" {
		return o.Bin
	}
	return "opencode"
}

func (o OpenCode) db() string {
	if o.DB != "" {
		return o.DB
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

func (o OpenCode) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 15 * time.Second
}

type ocSession struct {
	ID        string
	Title     string
	Directory string
	Updated   int64
	// Finish is the last message's finish value: "stop" means the assistant
	// ended its turn and you are next, "tool-calls" means it is mid-run.
	Finish string
}

// sessions reads every unarchived session together with the `finish` value of
// its most recent message, in one query.
//
// This reads opencode's SQLite rather than `opencode session list` because
// that command is project-scoped — run anywhere but inside a given project it
// reports nothing, which is useless for an inbox spanning every repo. The cost
// is a dependency on opencode's schema, so a missing or changed database
// degrades to an empty feed rather than an error.
func (o OpenCode) sessions(ctx context.Context) ([]ocSession, error) {
	path := o.db()
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 200
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout())
	defer cancel()

	q := fmt.Sprintf(`SELECT s.id, s.title, s.directory, s.time_updated,
	    COALESCE(m.data, '') AS msg
	  FROM session s
	  LEFT JOIN message m ON m.id = (
	    SELECT id FROM message WHERE session_id = s.id
	    ORDER BY time_created DESC, id DESC LIMIT 1)
	  WHERE s.time_archived IS NULL
	  ORDER BY s.time_updated DESC LIMIT %d;`, limit)

	// Read-only URI: the inbox must never write to opencode's database.
	cmd := exec.CommandContext(ctx, "sqlite3", "-json", "file:"+path+"?mode=ro", q)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("opencode db: %v: %s", err, truncate(detail, 200))
		}
		return nil, fmt.Errorf("opencode db: %w", err)
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return nil, nil
	}

	var rows []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Updated   int64  `json:"time_updated"`
		Msg       string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("opencode db: %w", err)
	}

	sessions := make([]ocSession, 0, len(rows))
	for _, r := range rows {
		s := ocSession{ID: r.ID, Title: r.Title, Directory: r.Directory, Updated: r.Updated}
		if r.Msg != "" {
			var msg struct {
				Role   string `json:"role"`
				Finish string `json:"finish"`
			}
			if json.Unmarshal([]byte(r.Msg), &msg) == nil {
				if msg.Role == "assistant" {
					s.Finish = msg.Finish
				} else {
					// The last word is yours; it is working, not waiting.
					s.Finish = "user"
				}
			}
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// liveDirs is the set of working directories opencode is running in right now.
// One lsof call covers every opencode process.
func liveDirs(ctx context.Context, command string, timeout time.Duration) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// -c matches by command name, -d cwd limits to the working directory
	// descriptor, -Fn prints just the name field.
	cmd := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-c", command, "-Fn")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// lsof exits non-zero when it cannot stat some unrelated process; the
	// output we asked for is still there, so the status is not worth failing on.
	_ = cmd.Run()

	dirs := map[string]bool{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "n/") {
			dirs[strings.TrimPrefix(line, "n")] = true
		}
	}
	return dirs
}

// liveNear reports whether sessionDir is close enough to a live process's cwd
// to belong to it: the same directory, or exactly one level into or out of it.
//
// Opencode's recorded session directory does not always equal the process's
// literal working directory — one observed case had the process at
// ".../Clank" and the session recorded a level down at ".../Clank/lab" — so an
// exact match misses real tabs. But ancestry at unbounded depth is not safe:
// a session recorded at "/Users/tyler" is an ancestor of every project on the
// machine, so it would read as "live" against any running opencode process at
// all. One level catches the nesting actually seen without that blowup.
func liveNear(live map[string]bool, sessionDir string) bool {
	for cwd := range live {
		if cwd == sessionDir {
			return true
		}
		if rel, ok := oneLevelApart(cwd, sessionDir); ok && rel {
			return true
		}
	}
	return false
}

// oneLevelApart reports whether b is exactly one path component below a, or a
// is exactly one path component below b. ok is false when neither is a
// prefix of the other at all.
func oneLevelApart(a, b string) (apart bool, ok bool) {
	shorter, longer := a, b
	if len(longer) < len(shorter) {
		shorter, longer = longer, shorter
	}
	if !strings.HasPrefix(longer, shorter+string(filepath.Separator)) {
		return false, false
	}
	rest := strings.Trim(strings.TrimPrefix(longer, shorter), string(filepath.Separator))
	return !strings.Contains(rest, string(filepath.Separator)), true
}

func (o OpenCode) item(s ocSession, finish string) feed.Item {
	project := filepath.Base(s.Directory)
	if project == "." || project == string(filepath.Separator) {
		project = s.Directory
	}

	state := feed.StateRunning
	if finish == "stop" {
		state = feed.StateBlocked
	}

	updated := time.UnixMilli(s.Updated)
	if s.Updated == 0 {
		updated = o.now()
	}

	title := strings.TrimSpace(s.Title)
	// opencode names an unprompted session "New session - <timestamp>", which
	// tells you nothing the row does not already show.
	if title == "" || strings.HasPrefix(title, "New session - ") {
		title = "session in " + project
	}

	item := feed.Item{
		Schema:    feed.Schema,
		Source:    "opencode",
		ID:        s.ID,
		Kind:      "session",
		Title:     title,
		State:     state,
		Since:     updated.UTC().Format(time.RFC3339),
		UpdatedAt: updated.UTC().Format(time.RFC3339),
		Context:   map[string]string{"project": project, "cwd": s.Directory},
	}
	if finish != "" {
		item.Context["finish"] = finish
	}
	if state == feed.StateBlocked {
		item.Needs = &feed.Needs{
			// opencode publishes no one-line "needs", unlike Claude Code's
			// job state. Saying so is better than inventing a summary.
			Prompt: "Finished its turn — waiting on you.",
			Actions: []feed.Action{
				// The dispatch Claude Code has no equivalent of: a message
				// goes straight into the existing session.
				{Label: "reply", Run: []string{o.bin(), "run", "-s", s.ID, "{message}"}, Dir: s.Directory},
				{Label: "open", Run: []string{o.bin(), "--session", s.ID}, Dir: s.Directory},
			},
		}
	}
	return item
}

func (o OpenCode) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o OpenCode) Fetch(ctx context.Context) (feed.Feed, error) {
	sessions, err := o.sessions(ctx)
	if err != nil {
		return feed.Feed{}, err
	}

	var dirs map[string]bool
	if !o.AnyDirectory {
		dirs = liveDirs(ctx, "opencode", o.timeout())
	}

	// Newest session per directory: a tab has one conversation you care
	// about, not the twenty that came before it in the same repo.
	newest := map[string]ocSession{}
	for _, s := range sessions {
		if dirs != nil && !liveNear(dirs, s.Directory) {
			continue
		}
		if prev, ok := newest[s.Directory]; !ok || s.Updated > prev.Updated {
			newest[s.Directory] = s
		}
	}

	f := feed.Feed{Schema: feed.Schema, Items: make([]feed.Item, 0, len(newest))}
	for _, s := range newest {
		f.Items = append(f.Items, o.item(s, s.Finish))
	}
	return f, nil
}
