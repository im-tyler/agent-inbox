package inbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/fsutil"
	"github.com/im-tyler/agent-inbox/internal/ident"
)

// Event is what a hook drops on disk for the inbox to ingest. It lets sessions
// the inbox did not spawn (e.g. a Claude session you run by hand) report their
// state into the central inbox.
type Event struct {
	SessionID string `json:"session_id"`
	Dir       string `json:"dir"`
	Tool      string `json:"tool"`
	Message   string `json:"message"`
	TS        int64  `json:"ts"`

	// Reason is why the session stopped. An event used to carry a message and
	// nothing else, so everything that reached the inbox became the same
	// undifferentiated "waiting" — and the supervisor, whose whole job is
	// deciding what to do about a stalled project, could tell that one was
	// stalled but never why.
	//
	// Empty means ReasonDone: events written by an older binary, or by a hook
	// that predates this field, still ingest as what they were.
	Reason Reason `json:"reason,omitempty"`

	// Detail is the specific ask when Reason needs one — the tool a permission
	// prompt is blocked on, the question being posed. One line, not a
	// transcript.
	Detail string `json:"detail,omitempty"`
}

// Reason is the closed set of ways a session can stop.
//
// Closed because the supervisor branches on it and the fleet renders it: a
// value invented by a hook would reach both as an unhandled case. Anything
// unrecognised is normalised to ReasonDone on ingest.
type Reason string

const (
	// ReasonDone: the turn finished and the session is idle.
	ReasonDone Reason = "done"
	// ReasonPermission: the agent is blocked asking to do something. This is
	// the one that most needs to be distinguishable — it is not progress, it
	// is a decision waiting on a human, and it stays blocked indefinitely.
	ReasonPermission Reason = "permission"
	// ReasonQuestion: the agent asked something and cannot continue until it
	// is answered.
	ReasonQuestion Reason = "question"
	// ReasonError: the turn failed.
	ReasonError Reason = "error"
)

// ParseReason normalises a reason from disk. An unknown value becomes
// ReasonDone rather than an error: an event that reached the spool describes
// something that really happened, and discarding it over a label loses the
// event entirely.
func ParseReason(s string) Reason {
	switch Reason(strings.ToLower(strings.TrimSpace(s))) {
	case ReasonPermission:
		return ReasonPermission
	case ReasonQuestion:
		return ReasonQuestion
	case ReasonError:
		return ReasonError
	default:
		return ReasonDone
	}
}

// Blocking reports whether a reason means the session is stuck on a human
// rather than finished with its turn.
func (r Reason) Blocking() bool {
	return r == ReasonPermission || r == ReasonQuestion
}

// eventSeq disambiguates two events written in the same nanosecond by the same
// process. TS alone was seconds, so two Stop events from one session in the
// same second produced the same filename and the second silently overwrote the
// first.
var eventSeq atomic.Uint64

// WriteEvent files an event in the spool.
//
// The write goes to a temp file and is renamed into place, because Ingest
// reads whatever it finds: writing the final name directly let a reader see a
// half-written file, fail to parse it, and delete it as though it had been
// handled. The event was gone and nothing recorded that it had existed.
func WriteEvent(eventsDir string, ev Event) error {
	name := fmt.Sprintf("%d-%d-%s.json", ev.TS, eventSeq.Add(1), shortID(ev.SessionID))
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	// Events carry assistant output and project paths. 0600, like everything
	// else under the data dir.
	return fsutil.WriteFileAtomic(filepath.Join(eventsDir, name), b, fsutil.FileMode)
}

// Ingest applies and removes pending event files, returning the names of
// projects newly flipped to waiting.
func (in *Inbox) Ingest(eventsDir string) []string {
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		return nil
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		// Skip the temp files of in-flight writes: they are not events yet.
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // names are ts-prefixed, so oldest first
	var updated []string
	for _, fn := range files {
		full := filepath.Join(eventsDir, fn)
		b, err := os.ReadFile(full)
		if err != nil {
			continue // leave it; a read that failed once may succeed next tick
		}
		var ev Event
		if err := json.Unmarshal(b, &ev); err != nil {
			// Quarantine rather than delete. Deleting malformed input as
			// though it were processed destroys the only evidence of whatever
			// wrote it.
			_ = os.Rename(full, full+".bad")
			continue
		}
		if name, ok := in.applyEvent(ev); ok {
			updated = append(updated, name)
			// The supervisor, if it is allowed to notice things. Told the
			// reason as well as the name: "finished" and "stuck on a permission
			// prompt" are the two cases, and they are what it has to choose
			// between.
			in.oversight().Notice(name, string(ParseReason(string(ev.Reason))))
		}
		os.Remove(full)
	}
	if len(updated) > 0 {
		in.save()
	}
	return updated
}

// applyEvent files a Stop-hook event against the project that owns the session
// it came from.
//
// The match is on tool and session id, not just directory. Matching on
// directory alone meant any session that happened to stop in a project's
// folder could rewrite that project's identity: a Claude session run by hand
// inside a Codex project would overwrite the Codex session id with a Claude
// one, and a second Claude session in the same folder could replace the
// managed session's id with its own. Either left the project pointing at a
// session the inbox did not start and could not resume.
//
// An external session that matches nothing is not an error. It belongs in the
// session inbox, which is where unmanaged sessions are meant to appear;
// adopting it here by overwriting a managed session is not the same thing.
func (in *Inbox) applyEvent(ev Event) (string, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, p := range in.projects {
		if !ident.SameDir(p.Dir, ev.Dir) {
			continue
		}
		if p.Tool != ev.Tool {
			continue
		}
		// Only a session this project already owns may speak for it.
		if p.SessionID == "" || ev.SessionID != p.SessionID {
			continue
		}
		ts := time.Unix(0, ev.TS)
		// A turn the inbox is running owns this project's state. An event
		// arriving mid-turn would flip it to waiting, let the UI send again,
		// and leave the original subprocess to have its result discarded.
		if in.active[p.Name] != nil || p.Status == driver.StatusWorking {
			// A permission prompt is the exception, and it is the case the
			// notification hook exists for: it fires *during* a turn, and a
			// turn stuck on a prompt looks exactly like one doing slow work
			// until it hits the timeout half an hour later. So record why it
			// is stuck without taking the turn's state away from it — status
			// stays Working, nothing is written to history, and the running
			// subprocess remains the only thing that can finish this turn.
			if ParseReason(string(ev.Reason)).Blocking() {
				p.WaitReason = ParseReason(string(ev.Reason))
				p.WaitDetail = ev.Detail
				p.Activity = "blocked"
				p.UpdatedAt = ts
				return p.Name, true
			}
			continue
		}
		// Reject an event older than what we already have. Files are ingested
		// oldest-first, but a hook can be slow to write and arrive out of
		// order relative to a turn that completed here.
		if !p.UpdatedAt.IsZero() && ts.Before(p.UpdatedAt) {
			continue
		}
		reason := ParseReason(string(ev.Reason))
		p.Status = driver.StatusWaiting
		if reason == ReasonError {
			p.Status = driver.StatusError
			if p.LastErr = ev.Detail; p.LastErr == "" {
				p.LastErr = "the session reported an error"
			}
		}
		p.WaitReason = reason
		p.WaitDetail = ev.Detail
		if ev.Message != "" {
			p.LastMessage = ev.Message
			p.appendHistory(Message{Role: "assistant", Content: ev.Message, Timestamp: ts})
		}
		// A blocked session has not said anything — it is sitting on a prompt.
		// Without a line here the fleet shows the previous turn's reply beside
		// a badge saying something needs attention, which reads as that reply
		// being what needs attention.
		if reason.Blocking() {
			p.appendHistory(Message{Role: "system", Content: blockedLine(reason, ev.Detail), Timestamp: ts})
		}
		p.UpdatedAt = ts
		return p.Name, true
	}
	return "", false
}

// blockedLine is what a blocked session's thread says, since the session
// itself said nothing.
func blockedLine(r Reason, detail string) string {
	what := "waiting on a decision from you"
	if r == ReasonPermission {
		what = "blocked asking permission"
	}
	if detail != "" {
		return what + ": " + detail
	}
	return what
}

func shortID(s string) string {
	s = strings.ReplaceAll(s, "/", "")
	if s == "" {
		return "noid"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
