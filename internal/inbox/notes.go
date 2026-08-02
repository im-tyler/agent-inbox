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
	Text string `json:"text"`
	// Projects are the fleet members this note names, detected when it was
	// written. Empty means it is a cross-cutting fact — those are the
	// architectural ones, and they outlive any single project.
	Projects  []string  `json:"projects,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// mentions reports whether the note is about one of the given projects, or is
// general enough to be about all of them.
func (n Note) mentions(names map[string]bool) bool {
	if len(n.Projects) == 0 {
		return true
	}
	for _, p := range n.Projects {
		if names[strings.ToLower(p)] {
			return true
		}
	}
	return false
}

// ParseKingNotes extracts [note: ...] directives from a response. Same
// line-oriented shape as ParseKingDirectives: a directive is a whole line, so
// prose that happens to mention the syntax cannot become one.
func ParseKingNotes(response string) []string {
	return parseBracketed(response, "[note:")
}

// ParseKingNoteDrops extracts [note drop: ...] directives. Retracting a fact
// is the supervisor's job, not a maintenance script's: it is the only thing
// that reads every note each turn and knows which one the world has moved
// past. The research calls this reconsolidation — updating a memory when it
// is retrieved is what stops stale facts living forever.
func ParseKingNoteDrops(response string) []string {
	return parseBracketed(response, "[note drop:")
}

func parseBracketed(response, prefix string) []string {
	var out []string
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, prefix) || !strings.HasSuffix(line, "]") {
			continue
		}
		// "[note:" must not swallow "[note drop:".
		if prefix == "[note:" && strings.HasPrefix(lower, "[note drop:") {
			continue
		}
		if text := strings.TrimSpace(line[len(prefix) : len(line)-1]); text != "" {
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
		in.notes = append(in.notes, Note{
			Text:      t,
			Projects:  in.projectsNamedIn(t),
			CreatedAt: time.Now(),
		})
		added = true
	}
	in.notes = evict(in.notes, in.liveNames())
	in.mu.Unlock()
	if added {
		in.saveNotes()
	}
}

// projectsNamedIn finds which fleet members a note is about. Callers hold mu.
// Names under three characters are skipped: a two-letter project would match
// inside half the words in English and tag notes that are not about it.
func (in *Inbox) projectsNamedIn(text string) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, p := range in.projects {
		if len(p.Name) < 3 {
			continue
		}
		if strings.Contains(lower, strings.ToLower(p.Name)) {
			out = append(out, p.Name)
		}
	}
	return out
}

// liveNames is the set of projects that currently exist. Callers hold mu.
func (in *Inbox) liveNames() map[string]bool {
	out := make(map[string]bool, len(in.projects))
	for _, p := range in.projects {
		out[strings.ToLower(p.Name)] = true
	}
	return out
}

// evict brings the store back under the cap.
//
// Not oldest-first. The oldest note is the one that has survived longest
// without being contradicted, which makes it the most likely to be a durable
// architectural fact; the newest is the most likely to be passing status.
// Plain FIFO discards exactly the wrong end.
//
// So: notes about projects that no longer exist go first — they can never be
// relevant again. Then the oldest project-specific note, since a fact scoped
// to one project is narrower than one that names none. Untagged notes are
// given up last.
func evict(notes []Note, live map[string]bool) []Note {
	if len(notes) <= maxNotes {
		return notes
	}
	kept := notes[:0]
	for _, n := range notes {
		if len(n.Projects) > 0 && !n.mentions(live) {
			continue // about nothing that still exists
		}
		kept = append(kept, n)
	}
	notes = kept
	// Still over: drop oldest tagged notes, then oldest of anything.
	for _, taggedOnly := range []bool{true, false} {
		for len(notes) > maxNotes {
			idx := -1
			for i, n := range notes {
				if taggedOnly && len(n.Projects) == 0 {
					continue
				}
				idx = i
				break
			}
			if idx < 0 {
				break
			}
			notes = append(notes[:idx], notes[idx+1:]...)
		}
	}
	return notes
}

// DropNotes removes every note containing any of the given substrings, case
// insensitively, and reports how many went.
func (in *Inbox) DropNotes(patterns []string) int {
	if len(patterns) == 0 {
		return 0
	}
	in.mu.Lock()
	kept := in.notes[:0]
	dropped := 0
	for _, n := range in.notes {
		match := false
		for _, pat := range patterns {
			pat = strings.TrimSpace(strings.ToLower(pat))
			if pat != "" && strings.Contains(strings.ToLower(n.Text), pat) {
				match = true
				break
			}
		}
		if match {
			dropped++
			continue
		}
		kept = append(kept, n)
	}
	in.notes = kept
	in.mu.Unlock()
	if dropped > 0 {
		in.saveNotes()
	}
	return dropped
}

// forgetProject drops notes that named only this project. Once it is gone
// they can never be relevant again, and they would go on costing context in
// every king turn forever.
func (in *Inbox) forgetProject(name string) {
	in.mu.Lock()
	kept := in.notes[:0]
	dropped := 0
	for _, n := range in.notes {
		if len(n.Projects) == 1 && strings.EqualFold(n.Projects[0], name) {
			dropped++
			continue
		}
		kept = append(kept, n)
	}
	in.notes = kept
	in.mu.Unlock()
	if dropped > 0 {
		in.saveNotes()
	}
}

// NotesFor returns the notes worth putting in front of the king this turn:
// those about one of these projects, plus the cross-cutting ones.
func (in *Inbox) NotesFor(names map[string]bool) []Note {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []Note
	for _, n := range in.notes {
		if n.mentions(names) {
			out = append(out, n)
		}
	}
	return out
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
	if in.notesPath == "" || in.closed() {
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
