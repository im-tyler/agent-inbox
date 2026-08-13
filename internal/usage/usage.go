// Package usage reports how much has been spent against a rate limit.
//
// Everything here is an estimate, and the API surface says so rather than
// hiding it. No supported interface reports remaining quota; what exists is the
// per-message usage that Claude Code writes into its own transcripts, which can
// be summed over a rolling window. That is a good proxy and it is not the same
// thing, so Snapshot carries Estimated and every caller renders it as such.
//
// A resource number that looks authoritative and is not gets trusted at exactly
// the moment it matters — deciding whether to start expensive work.
package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// BlockLength is the rate-limit window. The limit resets five hours after
	// the first message of a block rather than on the clock, so blocks are
	// anchored to activity, not to midnight.
	BlockLength = 5 * time.Hour

	// WeekLength is the longer rolling window.
	WeekLength = 7 * 24 * time.Hour

	// retention is how far back entries are kept. A margin past the weekly
	// window, so a block that straddles the boundary is still whole.
	retention = WeekLength + 24*time.Hour

	// maxLine bounds one transcript record. A single assistant turn carrying a
	// large tool result arrives as one line.
	maxLine = 8 << 20
)

// Window is what was spent over a span of time.
type Window struct {
	Start, End                            time.Time
	Input, Output, CacheCreate, CacheRead int64
	// Models seen in this window, sorted. Which model did the work changes what
	// the same token count means against a limit, so the number is not
	// interpretable without it.
	Models []string
}

// Total is every token that counted, cache reads included. They are billed
// differently but they all consume the window.
func (w Window) Total() int64 { return w.Input + w.Output + w.CacheCreate + w.CacheRead }

// Empty reports a window with nothing in it, which a caller should render as
// nothing rather than as a zero — "no data" and "no usage" look identical as a
// number and mean opposite things.
func (w Window) Empty() bool { return w.Total() == 0 }

// Snapshot is the current picture.
type Snapshot struct {
	// Block is the rate-limit window in progress.
	Block Window
	// Week is the rolling seven days.
	Week Window
	// ResetsAt is when Block ends. Zero when there is no block in progress.
	ResetsAt time.Time
	// Account is who the usage belongs to, empty when it cannot be determined.
	// Empty renders as unknown; it must never be guessed, because attributing
	// one account's burn to another is worse than admitting ignorance.
	Account string
	// Estimated is always true. It is a field rather than a doc comment so a
	// caller has to look at it.
	Estimated bool
}

// Source reads usage from one tool's own records.
type Source interface {
	Name() string
	Read() (Snapshot, error)
}

// Summary is the one-line form, or "" when there is nothing to report.
//
// It says burn, never "remaining". Remaining would need a denominator nobody
// publishes, and inventing one is how a number that looks authoritative and is
// not gets trusted at the moment it matters.
//
// Cache reads dominate the total — on real transcripts by a factor of fifty —
// so the headline is the tokens that were newly processed and cache is named
// separately. A single figure would read as fifty times the work actually done.
func (s Snapshot) Summary() string {
	if s.Block.Empty() {
		return ""
	}
	b := s.Block
	out := "~" + compact(b.Input+b.Output+b.CacheCreate) + " tokens this 5h block"
	if b.CacheRead > 0 {
		out += " (+" + compact(b.CacheRead) + " cached)"
	}
	if !s.ResetsAt.IsZero() {
		out += ", resets " + s.ResetsAt.Local().Format("3:04PM")
	}
	return out
}

// compact renders a token count at a glance. Exact figures are in the detail
// view; a sidebar needs to be readable, not auditable.
// Compact renders a token count at a glance, for callers rendering their own
// capacity line.
func Compact(n int64) string { return compact(n) }

func compact(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// entry is one billable message. key is retained so that pruning can rebuild
// the dedup set from what survived.
type entry struct {
	at    time.Time
	key   string
	in    int64
	out   int64
	cc    int64
	cr    int64
	model string
}

// Claude reads Claude Code's transcripts under ~/.claude/projects.
//
// Reading is incremental: each file is remembered by size, and only what has
// been appended since the last read is parsed. A live session's transcript
// grows all day, and re-reading it whole on every refresh would spend more time
// counting tokens than the agents spend producing them.
type Claude struct {
	// Root defaults to ~/.claude/projects.
	Root string
	// Now allows tests to fix the clock.
	Now func() time.Time

	mu      sync.Mutex
	offsets map[string]int64 // path -> bytes already parsed
	seen    map[string]bool  // dedup key -> present
	entries []entry
}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Claude) root() string {
	if c.Root != "" {
		return c.Root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// Read scans for new records and returns the current windows.
func (c *Claude) Read() (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.offsets == nil {
		c.offsets = map[string]int64{}
		c.seen = map[string]bool{}
	}

	root := c.root()
	if root == "" {
		return Snapshot{Estimated: true}, nil
	}
	now := c.now()
	cutoff := now.Add(-retention)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable directory should not cost the rest of the scan.
			return nil //nolint:nilerr // deliberate: keep walking
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// A file untouched since before the retention window holds nothing that
		// can still be in either window.
		if info.ModTime().Before(cutoff) {
			return nil
		}
		c.readFile(path, info.Size())
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return Snapshot{Estimated: true}, err
	}

	c.prune(cutoff)
	return c.snapshot(now), nil
}

// readFile parses whatever has been appended since the last read.
func (c *Claude) readFile(path string, size int64) {
	from := c.offsets[path]
	switch {
	case size == from:
		return // unchanged
	case size < from:
		// The file shrank: it was rotated or replaced, so the offset describes
		// a file that no longer exists. Start again rather than reading from
		// the middle of a record.
		from = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, 0); err != nil {
			return
		}
	}

	// ReadBytes rather than a Scanner, because the offset has to advance only
	// over records that are actually complete. A transcript is being appended
	// to while this runs, so the last line is routinely half-written — and a
	// Scanner cannot say whether the line it handed back ended in a newline or
	// ran out of file. Counting the missing newline anyway advanced the offset
	// past the start of that record, and the turn was never counted once the
	// rest of it arrived.
	r := bufio.NewReaderSize(f, 64<<10)
	consumed := from
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// Whatever is here has no terminator yet. Leave the offset before
			// it so it is re-read whole next time.
			break
		}
		consumed += int64(len(line))
		if len(line) > maxLine {
			continue // too large to be a turn record; skip it, but consume it
		}
		if e, key, ok := parseEntry(line); ok && !c.seen[key] {
			c.seen[key] = true
			c.entries = append(c.entries, e)
		}
	}
	c.offsets[path] = consumed
}

// transcript is the subset of a record this package reads.
type transcript struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			Input       int64 `json:"input_tokens"`
			Output      int64 `json:"output_tokens"`
			CacheCreate int64 `json:"cache_creation_input_tokens"`
			CacheRead   int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// parseEntry decodes one record, returning its dedup key.
//
// The key is message id plus request id, because the same message is written
// more than once — twice in a row in the same file when a turn is recorded and
// again when a session is resumed or forked into a new transcript. Summing
// without it inflates every number here, and it inflates them silently.
func parseEntry(line []byte) (entry, string, bool) {
	line = trimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return entry{}, "", false
	}
	var t transcript
	if json.Unmarshal(line, &t) != nil || t.Type != "assistant" {
		return entry{}, "", false
	}
	u := t.Message.Usage
	if u.Input == 0 && u.Output == 0 && u.CacheCreate == 0 && u.CacheRead == 0 {
		return entry{}, "", false
	}
	at, err := time.Parse(time.RFC3339, t.Timestamp)
	if err != nil {
		return entry{}, "", false
	}
	key := t.Message.ID + "|" + t.RequestID
	if key == "|" {
		// Nothing to deduplicate against. Counting it risks a double; dropping
		// it risks an undercount. Undercounting a limit is the safer error.
		return entry{}, "", false
	}
	return entry{
		at: at, key: key, in: u.Input, out: u.Output,
		cc: u.CacheCreate, cr: u.CacheRead, model: t.Message.Model,
	}, key, true
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// prune drops entries that have fallen out of every window, and the dedup keys
// with them. Without it both grow for as long as the process runs.
func (c *Claude) prune(cutoff time.Time) {
	if len(c.entries) == 0 {
		return
	}
	kept := c.entries[:0]
	for _, e := range c.entries {
		if e.at.After(cutoff) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(c.entries) {
		return
	}
	c.entries = kept
	// The dedup set is rebuilt rather than pruned in place: a key whose entry
	// has aged out cannot be re-read, because reading is forward-only from a
	// stored offset.
	c.seen = make(map[string]bool, len(kept))
	for _, e := range kept {
		c.seen[e.key] = true
	}
}

// snapshot builds the windows from the retained entries.
func (c *Claude) snapshot(now time.Time) Snapshot {
	s := Snapshot{Estimated: true, Account: detectAccount()}
	if len(c.entries) == 0 {
		return s
	}
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].at.Before(c.entries[j].at) })

	weekStart := now.Add(-WeekLength)
	blockStart := currentBlockStart(c.entries, now)

	s.Week = Window{Start: weekStart, End: now}
	s.Block = Window{Start: blockStart, End: blockStart.Add(BlockLength)}
	weekModels, blockModels := map[string]bool{}, map[string]bool{}

	for _, e := range c.entries {
		if e.at.After(weekStart) {
			addTo(&s.Week, e, weekModels)
		}
		if !blockStart.IsZero() && !e.at.Before(blockStart) && e.at.Before(s.Block.End) {
			addTo(&s.Block, e, blockModels)
		}
	}
	s.Week.Models = sortedKeys(weekModels)
	s.Block.Models = sortedKeys(blockModels)
	if !blockStart.IsZero() && !s.Block.Empty() {
		s.ResetsAt = s.Block.End
	} else {
		s.Block = Window{}
	}
	return s
}

// currentBlockStart finds the start of the block now falls in.
//
// Blocks run forward from the first message after a gap of at least
// BlockLength, anchored to the hour — which is how the limit itself behaves.
// Bucketing on wall-clock five-hour boundaries would report a window that
// resets at a time nothing happens.
func currentBlockStart(entries []entry, now time.Time) time.Time {
	var start time.Time
	var last time.Time
	for _, e := range entries {
		switch {
		case start.IsZero(), e.at.Sub(last) >= BlockLength, e.at.Sub(start) >= BlockLength:
			start = e.at.Truncate(time.Hour)
		}
		last = e.at
	}
	if start.IsZero() || now.Sub(start) >= BlockLength {
		return time.Time{} // the last block has already expired
	}
	return start
}

func addTo(w *Window, e entry, models map[string]bool) {
	w.Input += e.in
	w.Output += e.out
	w.CacheCreate += e.cc
	w.CacheRead += e.cr
	if e.model != "" {
		models[e.model] = true
	}
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// detectAccount reads the signed-in account from Claude Code's own config.
//
// Best effort by design. An empty answer renders as unknown, which is the
// honest output: attributing one account's burn to another is a worse failure
// than admitting the account could not be determined.
func detectAccount() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var d struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &d) != nil {
		return ""
	}
	return d.OAuthAccount.EmailAddress
}
