package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Notes are what the supervisor knows about the fleet that no single session
// can hold: that teploy depends on Neutron's DB layer, that omni's provider
// key expired. Each session carries its own context; nothing carried the
// facts that span them.
//
// The king writes them with a [note: ...] directive rather than by editing a
// file. That works identically across claude, opencode and codex, needs no
// tool permissions, and keeps the file this program's to validate — a model
// cannot corrupt a store it never touches.

const (
	// maxNotes bounds the store. Injected into every king turn, notes are a
	// standing tax on the context window, so the oldest fall off rather than
	// growing without limit.
	maxNotes = 50
	// maxNoteLen keeps one note to a fact, not a transcript.
	maxNoteLen = 240
)

// Note is one durable fact the supervisor recorded.
type Note struct {
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// ParseKingNotes extracts [note: ...] directives from a response. Same
// line-oriented shape as ParseKingDirectives: a directive is a whole line, so
// prose that happens to mention the syntax cannot become one.
func ParseKingNotes(response string) []string {
	var out []string
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "[note:") || !strings.HasSuffix(line, "]") {
			continue
		}
		text := strings.TrimSpace(line[len("[note:") : len(line)-1])
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

// AddNotes records new facts, skipping ones already known. Deduplication is
// what stops a king that repeats itself each round from filling the store
// with one fact.
func (in *Inbox) AddNotes(texts []string) {
	if len(texts) == 0 {
		return
	}
	in.mu.Lock()
	known := make(map[string]bool, len(in.notes))
	for _, n := range in.notes {
		known[strings.ToLower(n.Text)] = true
	}
	added := false
	for _, t := range texts {
		t = truncateForKing(t, maxNoteLen)
		if t == "" || known[strings.ToLower(t)] {
			continue
		}
		known[strings.ToLower(t)] = true
		in.notes = append(in.notes, Note{Text: t, CreatedAt: time.Now()})
		added = true
	}
	if len(in.notes) > maxNotes {
		in.notes = in.notes[len(in.notes)-maxNotes:]
	}
	in.mu.Unlock()
	if added {
		in.saveNotes()
	}
}

// Notes returns a copy of the recorded facts, oldest first.
func (in *Inbox) Notes() []Note {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]Note(nil), in.notes...)
}

// ClearNotes drops everything. Exposed so a wrong fact does not need a text
// editor to remove.
func (in *Inbox) ClearNotes() {
	in.mu.Lock()
	in.notes = nil
	in.mu.Unlock()
	in.saveNotes()
}

// WithNotesPath enables note persistence. Without it notes live only for the
// session, which is still useful and never fatal.
func (in *Inbox) WithNotesPath(p string) *Inbox {
	in.notesPath = p
	in.loadNotes()
	return in
}

func (in *Inbox) loadNotes() {
	if in.notesPath == "" {
		return
	}
	b, err := os.ReadFile(in.notesPath)
	if err != nil {
		return
	}
	var saved []Note
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	in.mu.Lock()
	in.notes = saved
	in.mu.Unlock()
}

// saveNotes writes atomically, same as state: a crash mid-write must not cost
// the notes that were already there.
func (in *Inbox) saveNotes() {
	if in.notesPath == "" {
		return
	}
	in.mu.Lock()
	b, err := json.MarshalIndent(in.notes, "", "  ")
	in.mu.Unlock()
	if err != nil {
		return
	}
	dir := filepath.Dir(in.notesPath)
	_ = os.MkdirAll(dir, 0o755)
	tmp, err := os.CreateTemp(dir, ".notes-*.json")
	if err != nil {
		return
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), in.notesPath); err != nil {
		os.Remove(tmp.Name())
	}
}
