package inbox

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/fsutil"
	"github.com/im-tyler/agent-inbox/internal/ident"
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

// MaxNotes is the cap on the store, exported so the memory view can say how
// full it is.
const MaxNotes = maxNotes

// Kind separates the things a supervisor remembers, because they are not
// interchangeable and one of them must not be filtered.
type Kind string

const (
	// KindFact is something true about the fleet. Filtered by relevance: a
	// fact about a project you are not talking to is context spent on nothing.
	KindFact Kind = "fact"
	// KindConstraint is a rule the supervisor must respect. Never filtered.
	//
	// "neutron stays on the free model" is not a fact about neutron that can be
	// dropped when neutron is absent from a turn's fleet — it is a standing
	// rule, and a rule that only applies when its subject happens to be present
	// is not a rule. Filtering these was the difference between memory and
	// governance.
	KindConstraint Kind = "constraint"
	// KindPriority is what matters most right now. Never filtered either: the
	// point of stating a priority is to weigh work against everything else,
	// which cannot happen if it is only shown alongside its own subject.
	KindPriority Kind = "priority"
)

// Standing reports a kind that is injected regardless of which projects a turn
// is about.
func (k Kind) Standing() bool { return k == KindConstraint || k == KindPriority }

// Note is one durable thing the supervisor recorded.
type Note struct {
	Text string `json:"text"`
	// Kind defaults to KindFact when absent, so notes written before kinds
	// existed keep behaving exactly as they did.
	Kind Kind `json:"kind,omitempty"`
	// Proposed marks a standing rule the supervisor asked for and the user has
	// not ratified. A proposal is stored and shown; it is never injected, so
	// it binds nothing until accepted.
	//
	// Facts are never proposed. An observation that turns out to be wrong is
	// filtered out by relevance, ages out of a bounded store, and overrides
	// nothing — so it carries its own limits. A rule carries none of them,
	// which is exactly why it needs a principal behind it.
	Proposed bool `json:"proposed,omitempty"`
	// Projects are the fleet members this note names, detected when it was
	// written. Empty means it is a cross-cutting fact — those are the
	// architectural ones, and they outlive any single project.
	Projects  []string  `json:"projects,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// mentions reports whether the note is about one of the given projects, or is
// general enough to be about all of them.
func (n Note) mentions(names map[string]bool) bool {
	// An unratified rule is not injected at all, so relevance never arises.
	if n.Proposed {
		return false
	}
	// A ratified rule applies whether or not its subject is in the room.
	if n.Kind.Standing() {
		return true
	}
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

// ParseKingConstraints and ParseKingPriorities extract the two standing kinds.
// Separate directives rather than one with a label, because the supervisor has
// to choose the kind deliberately: a rule and an observation read the same in
// prose, and only one of them should override what a turn decides to do.
func ParseKingConstraints(response string) []string {
	return parseBracketed(response, "[constraint:")
}

func ParseKingPriorities(response string) []string {
	return parseBracketed(response, "[priority:")
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

// DropNoteExact removes the note whose text matches exactly, reporting whether
// one did.
//
// Exact rather than by substring, and by text rather than by index, because
// this is the user deleting a specific thing they are looking at: a substring
// match could take neighbours with it, and an index could name a different note
// if the store moved between rendering the list and pressing the key.
func (in *Inbox) DropNoteExact(text string) bool {
	dropped := false
	in.notesRMW(func() {
		in.mu.Lock()
		kept := in.notes[:0]
		for _, n := range in.notes {
			if !dropped && n.Text == text {
				dropped = true
				continue
			}
			kept = append(kept, n)
		}
		in.notes = kept
		in.mu.Unlock()
		if dropped {
			in.saveNotes()
		}
	})
	return dropped
}

// AddNotes records new facts, skipping ones already known. Deduplication is
// what stops a king that repeats itself each round from filling the store
// with one fact.
func (in *Inbox) AddNotes(texts []string) { in.addNotes(texts, KindFact) }

// ProposeConstraints and ProposePriorities record standing rules the
// supervisor has asked for. They are stored as proposals and bind nothing
// until the user ratifies them, at which point they move to config.
//
// This is the same stance the dispatcher already takes on actions: a target
// named in a response is a name, not authorisation. A rule stated in a
// response is a request, not policy. The supervisor's replies are shaped by
// what its projects say, which is shaped by whatever those agents have read,
// so a rule it wrote itself is a rule an attacker could have written — and a
// rule, unlike an observation, binds every later turn and never ages out.
func (in *Inbox) ProposeConstraints(texts []string) { in.addNotes(texts, KindConstraint) }
func (in *Inbox) ProposePriorities(texts []string)  { in.addNotes(texts, KindPriority) }

func (in *Inbox) addNotes(texts []string, kind Kind) {
	if len(texts) == 0 {
		return
	}
	in.notesRMW(func() {
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
				Text: t,
				Kind: kind,
				// Every standing rule the supervisor writes starts as a proposal.
				// There is no path from a model response to a rule that binds.
				Proposed:  kind.Standing(),
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
	})
}

// projectsNamedIn finds which fleet members a note is about. Callers hold mu.
//
// The match is on word boundaries, not substrings. Substring matching tagged a
// note with any project whose name appeared inside another word — a project
// called "app" was named by "happened", "apply" and "mapping" — and a wrong tag
// is not cosmetic: it decides which notes are injected into a turn, which are
// evicted first, and which are dropped when a project is deleted. The old
// three-character floor only ever helped two-letter names.
func (in *Inbox) projectsNamedIn(text string) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, p := range in.projects {
		if namesWord(lower, strings.ToLower(p.Name)) {
			out = append(out, p.Name)
		}
	}
	return out
}

// namesWord reports whether name appears in text as a whole word. Word
// characters are letters and digits; a project name's own dots, underscores and
// hyphens are interior to it, so "omni-analyst" is one word rather than two.
func namesWord(text, name string) bool {
	if name == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(name)
		beforeOK := start == 0 || !isWordByte(text[start-1])
		afterOK := end == len(text) || !isWordByte(text[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
		if i >= len(text) {
			return false
		}
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
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
		// A standing rule outlives the project it names. "neutron stays on the
		// free model" is still the rule for whatever replaces neutron, and
		// dropping it because a project was renamed silently repeals it.
		if !n.Kind.Standing() && len(n.Projects) > 0 && !n.mentions(live) {
			continue // about nothing that still exists
		}
		kept = append(kept, n)
	}
	notes = kept
	// Still over: oldest tagged facts first, then oldest untagged fact, and
	// only then a standing rule. Giving up a constraint to make room for an
	// observation is the wrong trade in every case — the observation will be
	// re-derived from the next status line, the rule will not.
	for _, pass := range []func(Note) bool{
		func(n Note) bool { return !n.Kind.Standing() && len(n.Projects) > 0 },
		func(n Note) bool { return !n.Kind.Standing() },
		func(Note) bool { return true },
	} {
		for len(notes) > maxNotes {
			idx := -1
			for i, n := range notes {
				if pass(n) {
					idx = i
					break
				}
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
	dropped := 0
	in.notesRMW(func() {
		in.mu.Lock()
		kept := in.notes[:0]
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
	})
	return dropped
}

// forgetProject drops notes that named only this project, and untags it from
// the ones that named it alongside others. Once it is gone those notes can
// never be relevant to it again, and they would go on costing context in every
// king turn forever.
//
// Untagging is what makes repeated deletion work. Dropping only notes whose
// Projects was exactly [this] left a note tagged [A, B] untouched when A went;
// deleting B afterwards then saw a two-element list again, so the note
// survived both of its projects and was injected forever.
func (in *Inbox) forgetProject(name string) {
	changed := false
	in.notesRMW(func() {
		in.mu.Lock()
		kept := in.notes[:0]
		for _, n := range in.notes {
			if len(n.Projects) == 0 {
				kept = append(kept, n) // cross-cutting; not about any one project
				continue
			}
			// A standing rule outlives the project it names. Removing a project
			// deletes what was observed about it; it does not repeal a decision the
			// user made, and silently repealing one is how a fleet ends up back on
			// a paid model because a repository was renamed.
			if n.Kind.Standing() {
				kept = append(kept, n)
				continue
			}
			remaining := n.Projects[:0]
			for _, p := range n.Projects {
				if !ident.SameName(p, name) {
					remaining = append(remaining, p)
				}
			}
			if len(remaining) == len(n.Projects) {
				kept = append(kept, n)
				continue
			}
			changed = true
			if len(remaining) == 0 {
				continue // was about this project and nothing else
			}
			n.Projects = remaining
			kept = append(kept, n)
		}
		in.notes = kept
		in.mu.Unlock()
		if changed {
			in.saveNotes()
		}
	})
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
	in.notesRMW(func() {
		in.mu.Lock()
		in.notes = nil
		in.mu.Unlock()
		in.saveNotes()
	})
}

// WithNotesPath enables note persistence. Without it notes live only for the
// session, which is still useful and never fatal.
func (in *Inbox) WithNotesPath(p string) *Inbox {
	in.notesPath = p
	if err := in.loadNotes(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: notes not loaded: %v\n", err)
	} else {
		in.rememberNotesMtime()
	}
	in.rememberNotesMtime()
	return in
}

// loadNotes re-reads the store, reporting failure. A read that cannot
// establish the current state must not be followed by a save: the mutation
// would serialize against data it failed to read and replace a damaged file
// with a stale in-memory snapshot — the exact loss the lock exists to
// prevent. An absent store is empty, not the old snapshot in memory.
func (in *Inbox) loadNotes() error {
	if in.notesPath == "" {
		return nil
	}
	b, err := os.ReadFile(in.notesPath)
	var saved []Note
	switch {
	case os.IsNotExist(err):
		// missing means empty
	case err != nil:
		return fmt.Errorf("read notes: %w", err)
	default:
		if err := json.Unmarshal(b, &saved); err != nil {
			return fmt.Errorf("parse notes (original preserved): %w", err)
		}
	}
	in.mu.Lock()
	in.notes = saved
	in.mu.Unlock()
	return nil
}

// saveNotes writes atomically, same as state: a crash mid-write must not cost
// the notes that were already there.
//
// The persist lock spans the snapshot as well as the write, for the reason
// given on Inbox.save — two savers that snapshot in one order and rename in the
// other leave the older set on disk.
func (in *Inbox) saveNotes() {
	in.notesPersistMu.Lock()
	defer in.notesPersistMu.Unlock()
	if in.notesPath == "" || in.closed() {
		return
	}
	in.mu.Lock()
	b, err := json.MarshalIndent(in.notes, "", "  ")
	in.mu.Unlock()
	if err != nil {
		return
	}
	if err := fsutil.WriteFileAtomic(in.notesPath, b, fsutil.FileMode); err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: notes not saved: %v\n", err)
		return
	}
	in.rememberNotesMtime()
}
