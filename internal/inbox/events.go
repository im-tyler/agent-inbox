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

// Event is what a Stop hook drops on disk for the inbox to ingest. It lets
// sessions the inbox did not spawn (e.g. a Claude session you run by hand)
// report their state into the central inbox.
type Event struct {
	SessionID string `json:"session_id"`
	Dir       string `json:"dir"`
	Tool      string `json:"tool"`
	Message   string `json:"message"`
	TS        int64  `json:"ts"`
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
		// A turn the inbox is running owns this project's state. An event
		// arriving mid-turn would flip it to waiting, let the UI send again,
		// and leave the original subprocess to have its result discarded.
		if in.active[p.Name] != nil || p.Status == driver.StatusWorking {
			continue
		}
		ts := time.Unix(0, ev.TS)
		// Reject an event older than what we already have. Files are ingested
		// oldest-first, but a hook can be slow to write and arrive out of
		// order relative to a turn that completed here.
		if !p.UpdatedAt.IsZero() && ts.Before(p.UpdatedAt) {
			continue
		}
		p.Status = driver.StatusWaiting
		if ev.Message != "" {
			p.LastMessage = ev.Message
			p.appendHistory(Message{Role: "assistant", Content: ev.Message, Timestamp: ts})
		}
		p.UpdatedAt = ts
		return p.Name, true
	}
	return "", false
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
